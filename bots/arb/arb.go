package arb

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Jinn-Master/Cash_Machine/core/chain"
	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
	"github.com/Jinn-Master/Cash_Machine/core/state"
	arbmath "github.com/Jinn-Master/Cash_Machine/core/math"
)

// ── PairState ────────────────────────────────────────────────────────────

// PairAddrs holds resolved on-chain pool addresses for each DEX.
// Zero address means the pair doesn't exist on that DEX.
type PairAddrs struct {
	BaseswapPair  common.Address // BaseSwap V2 pair (for reserve-based quoting)
	AIsToken0     bool           // token ordering in BaseSwap pair
	SwapBasedPair common.Address // SwapBased V2 pair (for reserve-based quoting)
	SBIsToken0    bool           // token ordering in SwapBased pair
	MaverickPool  common.Address // Maverick V2 pool (zero if not found)
	MavTokenAIn   bool           // token ordering in Maverick pool
}

type PairState struct {
	config.TokenPair
	Addrs     PairAddrs
	TradeSize *big.Int
}

func (p *PairState) Key() string {
	return fmt.Sprintf("%s/%s", p.SymA, p.SymB)
}

// ── Executor ───────────────────────────────────────────────────────────

type Executor struct {
	Client       *ethclient.Client
	ContractAddr common.Address
	PrivKey      *ecdsa.PrivateKey
	WalletAddr   common.Address
	ChainID      *big.Int

	mu        sync.Mutex
	cooldowns map[string]time.Time
	tradeLock sync.Mutex
}

func NewExecutor(
	client *ethclient.Client,
	contractAddr common.Address,
	privKey *ecdsa.PrivateKey,
	walletAddr common.Address,
	chainID *big.Int,
) *Executor {
	return &Executor{
		Client:       client,
		ContractAddr: contractAddr,
		PrivKey:      privKey,
		WalletAddr:   walletAddr,
		ChainID:      chainID,
		cooldowns:    make(map[string]time.Time),
	}
}

func (e *Executor) inCooldown(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	last, ok := e.cooldowns[key]
	return ok && time.Since(last) < time.Duration(config.CooldownSeconds)*time.Second
}

func (e *Executor) setCooldown(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cooldowns[key] = time.Now()
}

// ── EvaluatePair: fetch all quotes IN PARALLEL, find best arb, execute ────────
//
// All RPC calls fire simultaneously via goroutines + WaitGroup.
// Total latency = slowest single quote (~80-120ms on Base) regardless of DEX count.

func (e *Executor) EvaluatePair(ctx context.Context, p *PairState) {
	log := logger.Log

	if e.inCooldown(p.Key()) {
		return
	}

	// ── 1. Fire all quotes in parallel ───────────────────────────────────────
	var (
		quotes arbmath.DexQuotes
		mu     sync.Mutex
		wg     sync.WaitGroup
	)

	// Helper: run a quote fetch in a goroutine, store result thread-safely
	fetch := func(dexId uint8, fn func() (*big.Int, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := fn()
			if err == nil && out != nil && out.Sign() > 0 {
				mu.Lock()
				quotes[dexId] = out
				mu.Unlock()
			}
		}()
	}

	// DEX 0: Uniswap V3
	fetch(config.DexUniV3, func() (*big.Int, error) {
		return chain.UniV3Quote(ctx, e.Client, config.UniV3Quoter,
			p.AddrA, p.AddrB, p.UniV3Fee, p.TradeSize)
	})

	// DEX 1: Aerodrome volatile
	fetch(config.DexAerodrome, func() (*big.Int, error) {
		return chain.AerodromeQuote(ctx, e.Client, config.AeroQuoter,
			p.AddrA, p.AddrB, p.TradeSize, false)
	})

	// DEX 2: BaseSwap — reserve read + local constant-product math
	if p.Addrs.BaseswapPair != (common.Address{}) {
		fetch(config.DexBaseSwap, func() (*big.Int, error) {
			res, err := chain.GetReserves(ctx, e.Client, p.Addrs.BaseswapPair)
			if err != nil {
				return nil, err
			}
			ra, rb := res.Reserve0, res.Reserve1
			if !p.Addrs.AIsToken0 {
				ra, rb = res.Reserve1, res.Reserve0
			}
			out := arbmath.QuickswapQuote(ra, rb, p.TradeSize)
			if out.Sign() == 0 {
				return nil, fmt.Errorf("zero output")
			}
			return out, nil
		})
	}

	// DEX 3: Aerodrome stable
	fetch(config.DexAeroStable, func() (*big.Int, error) {
		return chain.AerodromeQuote(ctx, e.Client, config.AeroQuoter,
			p.AddrA, p.AddrB, p.TradeSize, true)
	})

	// DEX 4: SwapBased — reserve read + local math
	if p.Addrs.SwapBasedPair != (common.Address{}) {
		fetch(config.DexSwapBased, func() (*big.Int, error) {
			res, err := chain.GetReserves(ctx, e.Client, p.Addrs.SwapBasedPair)
			if err != nil {
				return nil, err
			}
			ra, rb := res.Reserve0, res.Reserve1
			if !p.Addrs.SBIsToken0 {
				ra, rb = res.Reserve1, res.Reserve0
			}
			out := arbmath.QuickswapQuote(ra, rb, p.TradeSize)
			if out.Sign() == 0 {
				return nil, fmt.Errorf("zero output")
			}
			return out, nil
		})
	}

	// DEX 5: Maverick V2
	if p.Addrs.MaverickPool != (common.Address{}) {
		fetch(config.DexMaverick, func() (*big.Int, error) {
			return chain.MaverickV2Quote(ctx, e.Client,
				p.Addrs.MaverickPool, p.Addrs.MavTokenAIn, p.TradeSize)
		})
	}

	wg.Wait()

	// ── 2. Log all quotes at debug level ─────────────────────────────────────
	for i, q := range quotes {
		if q != nil {
			log.Debug("quote",
				"pair", p.Key(),
				"dex", config.DexNames[i],
				"out", arbmath.FmtAmount(q, p.DecB, p.SymB),
			)
		}
	}

	// ── 3. Find optimal buy/sell DEX pair ─────────────────────────────────────
	buyDex, sellDex, spreadPct := arbmath.BestArb(quotes)
	if buyDex < 0 {
		log.Debug("no arb pair", "pair", p.Key())
		return
	}

	log.Debug("best spread",
		"pair", p.Key(),
		"buy", config.DexNames[buyDex],
		"sell", config.DexNames[sellDex],
		"spread", fmt.Sprintf("%.3f%%", spreadPct),
	)

	if spreadPct < config.MinProfitPct {
		return
	}

	log.Info("⚡ OPPORTUNITY",
		"pair", p.Key(),
		"buy_on", config.DexNames[buyDex],
		"sell_on", config.DexNames[sellDex],
		"spread", fmt.Sprintf("%.3f%%", spreadPct),
	)

	e.ExecuteTrade(ctx, p, uint8(buyDex), uint8(sellDex), quotes[buyDex], quotes[sellDex])
}

// ── ExecuteTrade ─────────────────────────────────────────────────────────
// ⭐ CRITICAL: NOW includes cross-DEX validation before TX submission

func (e *Executor) ExecuteTrade(
	ctx context.Context,
	p *PairState,
	buyDex, sellDex uint8,
	buyOut, sellOut *big.Int,
) {
	log := logger.Log
	key := p.Key()

	e.tradeLock.Lock()
	defer e.tradeLock.Unlock()
	defer e.setCooldown(key)

	// ─────────────────────────────────────────────────────────────────────────
	// ⭐⭐⭐ CRITICAL FIX: Validate BOTH DEXes BEFORE TX submission
	// This is the KEY missing step that was causing 30-50% reverts
	// ─────────────────────────────────────────────────────────────────────────

	log.Info("🔍 CROSS-DEX VALIDATION starting",
		"pair", key,
		"buyDex", config.DexNames[buyDex],
		"sellDex", config.DexNames[sellDex],
	)

	// Verify both DEXes have the pair with sufficient liquidity
	hasLiquidity, err := chain.VerifyCrossLiquidityExists(
		ctx, e.Client,
		p.AddrA, p.AddrB,
		buyDex, sellDex,
		p.TradeSize,
		big.NewInt(0),
	)

	if err != nil {
		log.Warn("❌ VALIDATION FAILED: Cross-DEX liquidity check error",
			"pair", key,
			"buyDex", config.DexNames[buyDex],
			"sellDex", config.DexNames[sellDex],
			"error", err.Error(),
		)
		state.Global.Alert("warn", "ARB", "Cross-DEX validation error",
			fmt.Sprintf("%s: %v", key, err))
		return
	}

	if !hasLiquidity {
		log.Warn("❌ VALIDATION FAILED: Pair not ready on both DEXes",
			"pair", key,
			"buyDex", config.DexNames[buyDex],
			"sellDex", config.DexNames[sellDex],
			"detail", "Second DEX missing pair or insufficient liquidity — SKIPPING TX to save gas",
		)
		state.Global.Alert("info", "ARB", "Pair not ready on both DEXes",
			fmt.Sprintf("%s on %s→%s (would revert on-chain)", key, config.DexNames[buyDex], config.DexNames[sellDex]))
		return // Skip TX — would revert on-chain
	}

	log.Info("✅ VALIDATION PASSED: Both DEXes confirmed",
		"pair", key,
		"buyDex", config.DexNames[buyDex],
		"sellDex", config.DexNames[sellDex],
	)

	// ─────────────────────────────────────────────────────────────────────────
	// Safe to proceed with TX submission (both DEXes confirmed)
	// ─────────────────────────────────────────────────────────────────────────

	// Slippage-adjusted minimums
	minAB := arbmath.ApplySlippage(buyOut)
	repay := new(big.Int).Add(p.TradeSize, arbmath.FlashLoanFee(p.TradeSize))
	minBA := arbmath.ApplySlippage(repay)

	header, err := e.Client.HeaderByNumber(ctx, nil)
	if err != nil {
		log.Error("header fetch failed", "err", err)
		return
	}
	deadline := new(big.Int).SetUint64(header.Time + config.DeadlineBuffer)
	baseFee := header.BaseFee
	if baseFee == nil {
		baseFee = big.NewInt(0)
	}
	priorityFee := new(big.Int).Mul(big.NewInt(config.PriorityFeeGwei), big.NewInt(1e9))

	// ── 1. Simulate (free eth_call) ───────────────────────────────────────────
	err = chain.SimulateFlashArbitrage(ctx, e.Client,
		e.WalletAddr, e.ContractAddr,
		p.AddrA, p.AddrB,
		p.TradeSize, p.UniV3Fee,
		buyDex, sellDex,
		minAB, minBA, deadline,
	)
	if err != nil {
		log.Info("simulation failed — skipping", "pair", key, "err", err)
		return
	}
	log.Debug("simulation passed", "pair", key)

	// ── 2. Gas estimate + net profit check ────────────────────────────────────
	input, _ := chain.ArbABI.Pack("flashArbitrage",
		p.AddrA, p.AddrB, p.TradeSize, p.UniV3Fee, buyDex, sellDex, minAB, minBA, deadline,
	)
	gasEst, err := e.Client.EstimateGas(ctx, ethereum.CallMsg{
		From: e.WalletAddr,
		To:   &e.ContractAddr,
		Data: input,
	})
	if err != nil {
		log.Warn("gas estimate failed", "pair", key, "err", err)
		return
	}
	gasLimit := uint64(float64(gasEst) * config.GasLimitBufferArb)

	ethPerTokenA := 0.0
	if p.SymA == "WETH" {
		ethPerTokenA = 1.0
	}
	ok, reason := arbmath.NetProfitable(
		p.TradeSize, buyOut, sellOut,
		gasLimit, baseFee, priorityFee,
		ethPerTokenA,
	)
	if !ok {
		log.Info("unprofitable after gas", "pair", key, "reason", reason)
		return
	}
	log.Info("profit check passed", "pair", key, "detail", reason)

	// ── 3. Broadcast ──────────────────────────────────────────────────────────
	txHash, err := chain.BuildAndSendFlashArbitrage(ctx, e.Client, chain.TxParams{
		Contract:    e.ContractAddr,
		TokenA:      p.AddrA,
		TokenB:      p.AddrB,
		LoanAmount:  p.TradeSize,
		PoolFee:     p.UniV3Fee,
		BuyDex:      buyDex,
		SellDex:     sellDex,
		MinAB:       minAB,
		MinBA:       minBA,
		Deadline:    deadline,
		PrivKey:     e.PrivKey,
		BaseFee:     baseFee,
		PriorityFee: priorityFee,
		ChainID:     e.ChainID,
	})
	if err != nil {
		log.Error("send tx failed", "pair", key, "err", err)
		return
	}
	log.Info("🚀 tx sent (after validation passed)", "pair", key,
		"buyDex", config.DexNames[buyDex],
		"sellDex", config.DexNames[sellDex],
		"hash", txHash.Hex(),
	)

	go func() {
		rCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for {
			select {
			case <-rCtx.Done():
				log.Warn("receipt timeout", "hash", txHash.Hex())
				return
			case <-time.After(1 * time.Second):
				receipt, err := e.Client.TransactionReceipt(rCtx, txHash)
				if err != nil {
					continue
				}
				if receipt.Status == 1 {
					log.Info("✅ confirmed",
						"pair", key,
						"block", receipt.BlockNumber,
						"gasUsed", receipt.GasUsed,
					)
				} else {
					log.Warn("❌ REVERTED", "pair", key, "block", receipt.BlockNumber)
				}
				return
			}
		}
	}()
}

// ── WatchSyncEvents: Tier-1 WebSocket listener ───────────────────────────────

func WatchSyncEvents(ctx context.Context, client *ethclient.Client, exec *Executor, p *PairState) {
	log := logger.Log
	key := p.Key()

	if p.Addrs.BaseswapPair == (common.Address{}) {
		log.Warn("no BaseSwap pair for Sync subscription, skipping event watch", "pair", key)
		return
	}

	query := ethereum.FilterQuery{
		Addresses: []common.Address{p.Addrs.BaseswapPair},
		Topics:    [][]common.Hash{{chain.SyncTopic}},
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Info("📡 subscribing to Sync events", "pair", key)
		logsCh := make(chan types.Log, 64)
		sub, err := client.SubscribeFilterLogs(ctx, query, logsCh)
		if err != nil {
			log.Warn("subscribe failed, retrying", "pair", key, "err", err)
			time.Sleep(time.Duration(config.ReconnectDelaySec) * time.Second)
			continue
		}
		log.Info("✅ Sync subscription active", "pair", key)

	drain:
		for {
			select {
			case <-ctx.Done():
				sub.Unsubscribe()
				return
			case err := <-sub.Err():
				log.Warn("subscription dropped", "pair", key, "err", err)
				break drain
			case <-logsCh:
				go exec.EvaluatePair(ctx, p)
			}
		}
		time.Sleep(time.Duration(config.ReconnectDelaySec) * time.Second)
	}
}

// ── PollLowCapPairs: Tier-2 poller ───────────────────────────────────────────

func PollLowCapPairs(ctx context.Context, exec *Executor, pairs []*PairState) {
	log := logger.Log
	log.Info("🕐 low-cap poller started",
		"count", len(pairs),
		"interval_s", config.LowCapPollInterval,
	)
	ticker := time.NewTicker(time.Duration(config.LowCapPollInterval) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, p := range pairs {
				p := p
				go exec.EvaluatePair(ctx, p)
			}
		}
	}
}

// ── PollSinglePair: dedicated poller for factory hot-loaded pairs ─────────────

func PollSinglePair(ctx context.Context, exec *Executor, p *PairState, intervalSec int) {
	log := logger.Log
	log.Info("⚡ hot-pair poller started",
		"pair", p.Key(),
		"interval_s", intervalSec,
	)

	ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
	defer ticker.Stop()

	// Fire once immediately
	go exec.EvaluatePair(ctx, p)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			go exec.EvaluatePair(ctx, p)
		}
	}
}
