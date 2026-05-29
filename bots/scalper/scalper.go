package scalper

// bots/scalper/scalper.go

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Jinn-Master/Cash_Machine/core/chain"
	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
	"github.com/Jinn-Master/Cash_Machine/core/state"
)

type Position struct {
	TokenAddr   common.Address
	TokenSym    string
	BaseToken   string
	BaseAddr    common.Address
	EntryUSD    float64
	TokenAmount *big.Int
	BoughtAt    time.Time
	BuyDEX      string
	TxHash      common.Hash
}

type Scalper struct {
	client     *ethclient.Client
	privKey    *ecdsa.PrivateKey
	wallet     common.Address
	chainID    *big.Int

	mu         sync.Mutex
	openPos    map[common.Address]*Position
	blacklist  map[common.Address]string

	httpClient *http.Client
}

func New(
	client *ethclient.Client,
	privKey *ecdsa.PrivateKey,
	wallet common.Address,
	chainID *big.Int,
) *Scalper {
	return &Scalper{
		client:     client,
		privKey:    privKey,
		wallet:     wallet,
		chainID:    chainID,
		openPos:    make(map[common.Address]*Position),
		blacklist:  make(map[common.Address]string),
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *Scalper) Run(ctx context.Context) {
	log := logger.Log
	log.Info("🎯 scalper bot started")

	for {
		select {
		case <-ctx.Done():
			log.Info("scalper shutting down")
			s.forceCloseAll(ctx)
			return

		case ev := <-state.Global.NewPairCh:
			go s.evaluateNewToken(ctx, ev)

		case <-time.After(30 * time.Second):
			s.checkTimeouts(ctx)
		}
	}
}

type HoneypotResult struct {
	IsSafe         bool
	HasTransferTax bool
	TaxPct         float64
	SellSimPassed  bool
	IsVerified     bool
	RiskLevel      string
	Reason         string
}

func (s *Scalper) CheckHoneypot(ctx context.Context, tokenAddr, baseAddr common.Address, baseSym string) HoneypotResult {
	log := logger.Log
	result := HoneypotResult{RiskLevel: "unknown"}

	// FIX: explicit time.Duration cast on integer constant
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(config.ScalperHoneypotTimeout)*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var mu sync.Mutex

	// Check 1: balanceOf
	wg.Add(1)
	go func() {
		defer wg.Done()
		balInput := []byte{0x70, 0xa0, 0x82, 0x31}
		walletPadded := common.LeftPadBytes(s.wallet.Bytes(), 32)
		balInput = append(balInput, walletPadded...)

		_, err := s.client.CallContract(checkCtx, ethereum.CallMsg{
			To:   &tokenAddr,
			Data: balInput,
		}, nil)

		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			result.RiskLevel = "critical"
			result.Reason = "balanceOf reverts — likely broken token"
			return
		}
		result.SellSimPassed = true
	}()

	// Check 2: bytecode size
	wg.Add(1)
	go func() {
		defer wg.Done()
		code, err := s.client.CodeAt(checkCtx, tokenAddr, nil)
		if err != nil || len(code) == 0 {
			mu.Lock()
			result.RiskLevel = "critical"
			result.Reason = "no bytecode — not a contract"
			mu.Unlock()
			return
		}
		if len(code) < 100 {
			mu.Lock()
			if result.RiskLevel != "critical" {
				result.RiskLevel = "high"
				result.Reason = fmt.Sprintf("suspicious: tiny bytecode (%d bytes)", len(code))
			}
			mu.Unlock()
		}
	}()

	// Check 3: DexScreener
	wg.Add(1)
	go func() {
		defer wg.Done()
		url := fmt.Sprintf("https://api.dexscreener.com/latest/dex/tokens/%s", tokenAddr.Hex())
		resp, err := s.httpClient.Get(url)
		if err != nil {
			return
		}
		defer resp.Body.Close()

		var dsResp struct {
			Pairs []struct {
				Warnings []struct {
					Label   string `json:"label"`
					Message string `json:"message"`
				} `json:"warnings,omitempty"`
				Info struct {
					Websites []interface{} `json:"websites"`
					Socials  []interface{} `json:"socials"`
				} `json:"info"`
			} `json:"pairs"`
		}

		if err := json.NewDecoder(resp.Body).Decode(&dsResp); err != nil {
			return
		}

		mu.Lock()
		defer mu.Unlock()

		if len(dsResp.Pairs) == 0 {
			return
		}

		for _, pair := range dsResp.Pairs {
			for _, w := range pair.Warnings {
				label := strings.ToUpper(w.Label)
				if strings.Contains(label, "HONEYPOT") || strings.Contains(label, "SCAM") {
					result.RiskLevel = "critical"
					result.Reason = "DexScreener warning: " + w.Message
					return
				}
				if strings.Contains(label, "HIGH_TAX") || strings.Contains(label, "TAX") {
					result.HasTransferTax = true
					result.TaxPct = 10.0
					result.RiskLevel = "high"
					result.Reason = "DexScreener tax warning: " + w.Message
				}
			}
			if len(pair.Info.Websites) == 0 && len(pair.Info.Socials) == 0 {
				if result.RiskLevel == "unknown" {
					result.RiskLevel = "medium"
					result.Reason = "no social presence or website"
				}
			} else {
				if result.RiskLevel == "unknown" {
					result.RiskLevel = "low"
					result.IsVerified = true
				}
			}
		}
	}()

	wg.Wait()

	if result.RiskLevel == "critical" || result.RiskLevel == "high" {
		result.IsSafe = false
		return result
	}
	if result.RiskLevel == "unknown" {
		result.RiskLevel = "medium"
		result.IsSafe = false
		result.Reason = "insufficient safety data"
		return result
	}

	result.IsSafe = result.SellSimPassed && result.RiskLevel != "high"
	log.Debug("honeypot check", "token", tokenAddr.Hex(), "risk", result.RiskLevel, "safe", result.IsSafe)
	return result
}

func (s *Scalper) evaluateNewToken(ctx context.Context, ev state.NewTokenEvent) {
	log := logger.Log
	tokenAddr := ev.TokenAddr

	s.mu.Lock()
	if reason, blacklisted := s.blacklist[tokenAddr]; blacklisted {
		s.mu.Unlock()
		log.Debug("scalper: blacklisted token skipped", "token", ev.TokenSym, "reason", reason)
		return
	}
	if _, hasPos := s.openPos[tokenAddr]; hasPos {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	log.Info("🎯 scalper evaluating new token",
		"token",   ev.TokenSym,
		"addr",    tokenAddr.Hex(),
		"factory", ev.Factory,
	)

	baseAddr := common.HexToAddress(config.AddrUSDC)
	if ev.BaseToken == "WETH" {
		baseAddr = common.HexToAddress(config.AddrWETH)
	}

	hpResult := s.CheckHoneypot(ctx, tokenAddr, baseAddr, ev.BaseToken)
	if !hpResult.IsSafe {
		s.mu.Lock()
		s.blacklist[tokenAddr] = hpResult.Reason
		s.mu.Unlock()
		log.Info("⛔ scalper: honeypot/rug detected — blacklisted",
			"token",  ev.TokenSym,
			"risk",   hpResult.RiskLevel,
			"reason", hpResult.Reason,
		)
		state.Global.Alert("warn", "scalper",
			fmt.Sprintf("Honeypot detected: %s", ev.TokenSym),
			hpResult.Reason,
		)
		return
	}

	if ev.DexCount < 2 {
		log.Info("🕐 scalper: waiting for 2nd DEX listing",
			"token",        ev.TokenSym,
			"current_dexes", ev.DexCount,
		)
		return
	}

	log.Info("✅ scalper: token passes safety checks, evaluating entry",
		"token",     ev.TokenSym,
		"dex_count", ev.DexCount,
	)

	// Execute the scalp: buy token on Aerodrome
	s.executeScalp(ctx, ev, hpResult)
}

func (s *Scalper) executeScalp(ctx context.Context, ev state.NewTokenEvent, hpResult HoneypotResult) {
	log := logger.Log
	tokenAddr := ev.TokenAddr
	baseAddr := ev.BaseAddr
	baseSym := ev.BaseToken

	// Position sizing: max $100 USDC per scalp
	positionUSD := config.ScalperMaxPositionUSDC
	amountIn := new(big.Int).SetUint64(uint64(positionUSD * 1_000_000)) // USDC 6 decimals

	// Determine if pool is stable (e.g., for stablecoin pairs)
	isStable := baseSym == "USDC"

	// Calculate minimum output with slippage tolerance
	// Get a quote first to determine minOut
	quoteOut, err := chain.AerodromeQuote(ctx, s.client, config.AeroQuoter,
		baseAddr, tokenAddr, amountIn, isStable)
	if err != nil || quoteOut == nil || quoteOut.Sign() == 0 {
		log.Warn("scalper: quote failed, skipping entry",
			"token", ev.TokenSym, "err", err)
		return
	}

	// Apply slippage tolerance (0.5% default from config)
	minOut := new(big.Int).Mul(quoteOut, big.NewInt(9950))
	minOut.Div(minOut, big.NewInt(10000)) // 99.5% of quote = 0.5% slippage

	deadline := new(big.Int).Set(uint64(time.Now().Add(60 * time.Second).Unix()))

	// Approve then swap
	approveHash, err := chain.AerodromeApprove(ctx, s.client, s.privKey, baseAddr, amountIn)
	if err != nil {
		log.Error("scalper: approve failed", "token", ev.TokenSym, "err", err)
		return
	}
	log.Debug("scalper: approve pending", "hash", approveHash.Hex())

	// Small delay for approve to confirm
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}

	swapHash, err := chain.AerodromeSwap(ctx, s.client, s.privKey,
		baseAddr, tokenAddr, amountIn, minOut, isStable, deadline)
	if err != nil {
		log.Error("scalper: swap failed", "token", ev.TokenSym, "err", err)
		state.Global.Alert("error", "scalper",
			fmt.Sprintf("Buy failed: %s", ev.TokenSym), err.Error())
		return
	}

	log.Info("🎯 SCALP EXECUTED",
		"token",    ev.TokenSym,
		"amount",   fmt.Sprintf("$%.0f USDC", positionUSD),
		"tx",       swapHash.Hex(),
		"buy_dex",  "Aerodrome",
		"risk",     hpResult.RiskLevel,
	)

	// Track position
	s.mu.Lock()
	s.openPos[tokenAddr] = &Position{
		TokenAddr: tokenAddr,
		TokenSym:  ev.TokenSym,
		BaseToken: baseSym,
		EntryUSD:  positionUSD,
		BoughtAt:  time.Now(),
		BuyDEX:    "Aerodrome",
		TxHash:    swapHash,
	}
	s.mu.Unlock()

	// Record profit event for treasury tracking
	state.Global.RecordProfit(state.ProfitEvent{
		BotName:    "scalper",
		PairKey:    baseSym + "/" + ev.TokenSym,
		ProfitUSD:  0, // Will be realized on sell
		ExecutedAt: time.Now(),
		TxHash:     swapHash,
	})
}

func (s *Scalper) checkTimeouts(ctx context.Context) {
	log := logger.Log
	s.mu.Lock()
	var expired []*Position
	for _, pos := range s.openPos {
		// FIX: explicit time.Duration cast on integer constant
		if time.Since(pos.BoughtAt) > time.Duration(config.ScalperMaxHoldSec)*time.Second {
			expired = append(expired, pos)
		}
	}
	s.mu.Unlock()

	for _, pos := range expired {
		log.Warn("⏰ scalper: force-selling expired position",
			"token",    pos.TokenSym,
			"held_for", time.Since(pos.BoughtAt).Round(time.Second),
		)
		s.forceClose(ctx, pos)
	}
}

func (s *Scalper) forceClose(ctx context.Context, pos *Position) {
	log := logger.Log
	log.Warn("scalper: force-closing position (swap execution TODO)", "token", pos.TokenSym)
	s.mu.Lock()
	delete(s.openPos, pos.TokenAddr)
	s.mu.Unlock()
}

func (s *Scalper) forceCloseAll(ctx context.Context) {
	s.mu.Lock()
	positions := make([]*Position, 0, len(s.openPos))
	for _, pos := range s.openPos {
		positions = append(positions, pos)
	}
	s.mu.Unlock()
	for _, pos := range positions {
		s.forceClose(ctx, pos)
	}
}
