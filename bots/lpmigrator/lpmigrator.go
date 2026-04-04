package lpmigrator

// bots/lpmigrator/lpmigrator.go
//
// LP-Migrator Bot — Volatility Capture on Liquidity Migration Events
//
// ── The Opportunity ──────────────────────────────────────────────────────────
//
// When a project migrates liquidity from DEX A → DEX B (typically to earn
// Aerodrome gauge rewards), the following sequence happens on-chain:
//
//   Block N:   Burn(lpTokens) on DEX A  → liquidity removed → price spikes on DEX A
//   Block N:   (same tx or next) transfer tokens to new pool deployer
//   Block N+1: Mint(lpTokens) on DEX B  → new pool created or deepened
//   Block N+1 to N+5: Price on DEX A is now extremely thin, creating a
//              multi-block arb window vs DEX B where price is anchored
//
// ── What we detect ───────────────────────────────────────────────────────────
//
// We watch ALL V2 pair contracts for Burn events (LP removal). A large Burn
// (> $10k liquidity removed) is correlated with migration if:
//   1. It removes > 80% of the pool's total liquidity in one tx
//   2. A Mint event on a different DEX for the same token follows within 20 blocks
//
// The second condition (cross-DEX Mint) is the confirmation signal.
// The first condition alone is actionable as a pre-signal.
//
// ── The Trade ────────────────────────────────────────────────────────────────
//
// Pre-signal (Burn detected, Mint not yet seen):
//   → Alert scalper bot: monitor this token, ready to buy on DEX B
//   → Alert arb bot: pre-position quote polling on this token
//
// Confirmed migration (Burn + Mint within 20 blocks):
//   → Broadcast LPMigrationEvent to all bots via state.Global.LPMigrationCh
//   → Arb bot evaluates: DEX A (thin, inflated) vs DEX B (newly priced)
//   → Scalper bot: buy on DEX B before larger traders discover the new pool
//
// ── What we watch ────────────────────────────────────────────────────────────
//
// V2 pairs emit:
//   Burn(address indexed sender, uint amount0, uint amount1, address indexed to)
//   Mint(address indexed sender, uint amount0, uint amount1)
//
// We subscribe to ALL Burn/Mint events from all known V2 factory pairs by
// watching the factory-level Transfer events (LP token transfers to zero addr = burn).
//
// Architecture: one goroutine per factory, decodes Burn/Mint, cross-references
// pending burns against incoming mints using a time-windowed correlation map.

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
	"github.com/Jinn-Master/Cash_Machine/core/state"
)

// ── ABIs ──────────────────────────────────────────────────────────────────────

// Uniswap V2-compatible pair events (BaseSwap, SwapBased, Aerodrome V2)
const v2PairEventsABIStr = `[
  {
    "anonymous": false,
    "inputs": [
      {"indexed": true,  "name": "sender",  "type": "address"},
      {"indexed": false, "name": "amount0", "type": "uint256"},
      {"indexed": false, "name": "amount1", "type": "uint256"},
      {"indexed": true,  "name": "to",      "type": "address"}
    ],
    "name": "Burn",
    "type": "event"
  },
  {
    "anonymous": false,
    "inputs": [
      {"indexed": true,  "name": "sender",  "type": "address"},
      {"indexed": false, "name": "amount0", "type": "uint256"},
      {"indexed": false, "name": "amount1", "type": "uint256"}
    ],
    "name": "Mint",
    "type": "event"
  },
  {
    "constant": true,
    "inputs":  [],
    "name":    "token0",
    "outputs": [{"name": "", "type": "address"}],
    "type": "function"
  },
  {
    "constant": true,
    "inputs":  [],
    "name":    "token1",
    "outputs": [{"name": "", "type": "address"}],
    "type": "function"
  },
  {
    "constant": true,
    "inputs":  [],
    "name":    "getReserves",
    "outputs": [
      {"name": "_reserve0",          "type": "uint112"},
      {"name": "_reserve1",          "type": "uint112"},
      {"name": "_blockTimestampLast","type": "uint32"}
    ],
    "type": "function"
  }
]`

// ── PendingBurn tracks a detected burn awaiting a correlating Mint ────────────

type PendingBurn struct {
	Token       common.Address // niche token in the burned pair
	TokenSym    string
	SrcDEX      string
	SrcPair     common.Address
	Amount0     *big.Int
	Amount1     *big.Int
	LiqUSD      float64       // estimated USD value of removed liquidity
	BurnTxHash  common.Hash
	BurnBlock   uint64
	DetectedAt  time.Time
}

// ── PoolRegistry maps pair address → (token0, token1, dexName) ───────────────
// Built lazily as Burn/Mint events arrive — we don't pre-fetch all pairs.

type PoolInfo struct {
	Token0  common.Address
	Token1  common.Address
	DEXName string
}

// ── LPMigratorBot ─────────────────────────────────────────────────────────────

type LPMigratorBot struct {
	client *ethclient.Client
	pairABI abi.ABI

	// Correlation map: niche token address → pending burns (waiting for mint)
	// Key is the *niche* token (non-USDC, non-WETH side of the pair)
	pendingMu   sync.Mutex
	pendingBurns map[common.Address][]PendingBurn

	// Pool info cache: pair address → token pair + DEX
	poolMu   sync.RWMutex
	poolCache map[common.Address]PoolInfo

	burnTopic common.Hash
	mintTopic common.Hash
}

func New(client *ethclient.Client) (*LPMigratorBot, error) {
	pairABI, err := abi.JSON(strings.NewReader(v2PairEventsABIStr))
	if err != nil {
		return nil, fmt.Errorf("parse pair ABI: %w", err)
	}
	return &LPMigratorBot{
		client:       client,
		pairABI:      pairABI,
		pendingBurns: make(map[common.Address][]PendingBurn),
		poolCache:    make(map[common.Address]PoolInfo),
		burnTopic:    pairABI.Events["Burn"].ID,
		mintTopic:    pairABI.Events["Mint"].ID,
	}, nil
}

// Run starts the LP migrator — watches all V2 factories for Burn+Mint sequences.
func (bot *LPMigratorBot) Run(ctx context.Context) {
	log := logger.Log
	log.Info("🔄 LP-Migrator bot started — watching for liquidity migrations")

	// Subscribe to Burn and Mint events from ALL V2 factories
	// We watch both topics on the same subscription — more efficient than separate subs
	for _, fDef := range config.WatchedFactories {
		if fDef.IsV3 {
			continue // V3 uses different event model
		}
		fDef := fDef
		go bot.watchFactory(ctx, fDef)
	}

	// Expiry goroutine: remove pending burns older than 60 seconds
	// (if no Mint arrives in 60s, the migration likely isn't happening)
	go bot.expirePendingBurns(ctx)
}

// watchFactory polls for Burn+Mint events block-by-block.
// Uses FilterLogs (eth_getLogs) instead of subscriptions — compatible with Alchemy.
func (bot *LPMigratorBot) watchFactory(ctx context.Context, fDef config.FactoryConfig) {
	log := logger.Log
	log.Info("📡 LP-Migrator: starting poll watcher", "dex", fDef.Name)

	// Start from current block
	var fromBlock uint64
	if header, err := bot.client.HeaderByNumber(ctx, nil); err == nil {
		fromBlock = header.Number.Uint64()
	}

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			header, err := bot.client.HeaderByNumber(ctx, nil)
			if err != nil {
				continue
			}
			toBlock := header.Number.Uint64()
			if toBlock < fromBlock {
				continue
			}
			if toBlock-fromBlock > 20 {
				toBlock = fromBlock + 20
			}

			query := ethereum.FilterQuery{
				FromBlock: new(big.Int).SetUint64(fromBlock),
				ToBlock:   new(big.Int).SetUint64(toBlock),
				Topics:    [][]common.Hash{{bot.burnTopic, bot.mintTopic}},
			}

			logs, err := bot.client.FilterLogs(ctx, query)
			if err != nil {
				log.Warn("LP-Migrator poll failed", "dex", fDef.Name, "err", err)
				continue
			}

			for _, rawLog := range logs {
				go bot.processLog(ctx, rawLog, fDef.Name)
			}

			fromBlock = toBlock + 1
		}
	}
}

// processLog handles a single Burn or Mint event log.
func (bot *LPMigratorBot) processLog(ctx context.Context, rawLog types.Log, dexName string) {
	switch rawLog.Topics[0] {
	case bot.burnTopic:
		bot.handleBurn(ctx, rawLog, dexName)
	case bot.mintTopic:
		bot.handleMint(ctx, rawLog, dexName)
	}
}

// handleBurn processes a Burn event — potential start of an LP migration.
func (bot *LPMigratorBot) handleBurn(ctx context.Context, rawLog types.Log, dexName string) {
	log := logger.Log
	pairAddr := rawLog.Address

	// Decode amounts from data
	type burnData struct {
		Amount0 *big.Int
		Amount1 *big.Int
	}
	var d burnData
	if err := bot.pairABI.UnpackIntoInterface(&d, "Burn", rawLog.Data); err != nil {
		return
	}

	// Resolve pool info (cached)
	poolInfo, err := bot.getPoolInfo(ctx, pairAddr, dexName)
	if err != nil {
		return
	}

	// Filter: only care about pairs involving USDC or WETH
	usdcAddr := common.HexToAddress(config.AddrUSDC)
	wethAddr := common.HexToAddress(config.AddrWETH)

	var nicheToken, baseToken common.Address
	var baseIsToken0 bool
	switch {
	case poolInfo.Token0 == usdcAddr || poolInfo.Token0 == wethAddr:
		baseToken = poolInfo.Token0
		nicheToken = poolInfo.Token1
		baseIsToken0 = true
	case poolInfo.Token1 == usdcAddr || poolInfo.Token1 == wethAddr:
		baseToken = poolInfo.Token1
		nicheToken = poolInfo.Token0
		baseIsToken0 = false
	default:
		return // not a base-paired pool
	}
	_ = baseToken

	// Estimate USD value of removed liquidity
	var baseAmount *big.Int
	if baseIsToken0 {
		baseAmount = d.Amount0
	} else {
		baseAmount = d.Amount1
	}

	// USDC has 6 decimals, WETH has 18
	var liqUSD float64
	if baseToken == usdcAddr {
		liqUSD = float64(baseAmount.Uint64()) / 1e6 * 2 // *2 because pool is 50/50
	} else {
		// WETH — use cached price
		if priceEv, ok := state.Global.GetPrice(wethAddr); ok {
			ethAmt := new(big.Float).Quo(
				new(big.Float).SetInt(baseAmount),
				new(big.Float).SetFloat64(1e18),
			)
			ethFloat, _ := ethAmt.Float64()
			liqUSD = ethFloat * priceEv.PriceUSD * 2
		}
	}

	// Only care about significant burns (> $10k removed)
	const minBurnUSD = 10_000
	if liqUSD < minBurnUSD {
		return
	}

	// Get niche token symbol from price cache or use "UNKNOWN"
	tokenSym := "UNKNOWN"
	if priceEv, ok := state.Global.GetPrice(nicheToken); ok {
		tokenSym = priceEv.TokenSym
	}

	log.Info("🔥 LARGE LP BURN detected — potential migration starting",
		"dex",       dexName,
		"token",     tokenSym,
		"liq_usd",   fmt.Sprintf("$%.0f", liqUSD),
		"pair",      pairAddr.Hex(),
		"block",     rawLog.BlockNumber,
	)

	// Alert Telegram immediately — this is a high-value signal
	state.Global.Alert("warn", "lpmigrator",
		fmt.Sprintf("Large LP burn: %s on %s ($%.0f)", tokenSym, dexName, liqUSD),
		fmt.Sprintf("Pair: %s | Block: %d", pairAddr.Hex(), rawLog.BlockNumber),
	)

	// Register as pending burn — waiting for correlating Mint on another DEX
	pending := PendingBurn{
		Token:      nicheToken,
		TokenSym:   tokenSym,
		SrcDEX:     dexName,
		SrcPair:    pairAddr,
		Amount0:    d.Amount0,
		Amount1:    d.Amount1,
		LiqUSD:     liqUSD,
		BurnTxHash: rawLog.TxHash,
		BurnBlock:  rawLog.BlockNumber,
		DetectedAt: time.Now(),
	}

	bot.pendingMu.Lock()
	bot.pendingBurns[nicheToken] = append(bot.pendingBurns[nicheToken], pending)
	bot.pendingMu.Unlock()

	// Pre-signal the arb and scalper bots to start watching this token closely
	// Even before the Mint arrives, this is actionable:
	// DEX A price will spike (thin liquidity), DEX B price is still normal
	go bot.emitPreSignal(ctx, pending)
}

// emitPreSignal sends a partial LPMigrationEvent before the Mint is confirmed.
// This gives the scalper bot ~4-20 seconds head start.
func (bot *LPMigratorBot) emitPreSignal(ctx context.Context, burn PendingBurn) {
	log := logger.Log

	// Emit pre-signal with empty DstDEX — signals "watch this token"
	ev := state.LPMigrationEvent{
		Token:           burn.Token,
		TokenSym:        burn.TokenSym,
		SrcDEX:          burn.SrcDEX,
		DstDEX:          "UNKNOWN", // not confirmed yet
		SrcPairAddr:     burn.SrcPair,
		LiqRemovedUSD:   burn.LiqUSD,
		BurnTxHash:      burn.BurnTxHash,
		BurnBlock:       burn.BurnBlock,
		DetectedAt:      burn.DetectedAt,
		ActionWindowSec: 20, // conservative
	}

	select {
	case state.Global.LPMigrationCh <- ev:
		log.Debug("LP-Migrator: pre-signal emitted", "token", burn.TokenSym)
	case <-ctx.Done():
	default:
		log.Warn("LP-Migrator: LPMigrationCh full", "token", burn.TokenSym)
	}
}

// handleMint processes a Mint event — potential completion of an LP migration.
func (bot *LPMigratorBot) handleMint(ctx context.Context, rawLog types.Log, dexName string) {
	log := logger.Log
	pairAddr := rawLog.Address

	poolInfo, err := bot.getPoolInfo(ctx, pairAddr, dexName)
	if err != nil {
		return
	}

	usdcAddr := common.HexToAddress(config.AddrUSDC)
	wethAddr := common.HexToAddress(config.AddrWETH)

	var nicheToken common.Address
	switch {
	case poolInfo.Token0 == usdcAddr || poolInfo.Token0 == wethAddr:
		nicheToken = poolInfo.Token1
	case poolInfo.Token1 == usdcAddr || poolInfo.Token1 == wethAddr:
		nicheToken = poolInfo.Token0
	default:
		return
	}

	// Check if we have a pending burn for this token on a DIFFERENT DEX
	bot.pendingMu.Lock()
	burns, hasBurns := bot.pendingBurns[nicheToken]
	if !hasBurns || len(burns) == 0 {
		bot.pendingMu.Unlock()
		return
	}

	// Find a burn from a different DEX
	var matchedBurn *PendingBurn
	var matchIdx int
	for i, burn := range burns {
		if burn.SrcDEX != dexName {
			matchedBurn = &burns[i]
			matchIdx = i
			break
		}
	}
	if matchedBurn == nil {
		bot.pendingMu.Unlock()
		return
	}

	// Check it's within the correlation window (20 blocks = ~40s on Base)
	blockDiff := rawLog.BlockNumber - matchedBurn.BurnBlock
	if blockDiff > 20 {
		bot.pendingMu.Unlock()
		return
	}

	// Remove from pending
	bot.pendingBurns[nicheToken] = append(burns[:matchIdx], burns[matchIdx+1:]...)
	confirmedBurn := *matchedBurn
	bot.pendingMu.Unlock()

	log.Info("🚀 LP MIGRATION CONFIRMED",
		"token",       confirmedBurn.TokenSym,
		"from",        confirmedBurn.SrcDEX,
		"to",          dexName,
		"liq_usd",     fmt.Sprintf("$%.0f", confirmedBurn.LiqUSD),
		"block_delta", blockDiff,
	)

	// Estimate action window: typically 2-10 blocks before arb bots converge
	actionWindow := 20 - int(blockDiff)*2 // shrinks as more blocks pass
	if actionWindow < 4 {
		actionWindow = 4
	}

	// Emit confirmed migration event to all bots
	ev := state.LPMigrationEvent{
		Token:           nicheToken,
		TokenSym:        confirmedBurn.TokenSym,
		SrcDEX:          confirmedBurn.SrcDEX,
		DstDEX:          dexName,
		SrcPairAddr:     confirmedBurn.SrcPair,
		DstPairAddr:     pairAddr,
		LiqRemovedUSD:   confirmedBurn.LiqUSD,
		BurnTxHash:      confirmedBurn.BurnTxHash,
		BurnBlock:       confirmedBurn.BurnBlock,
		DetectedAt:      time.Now(),
		ActionWindowSec: actionWindow,
	}

	select {
	case state.Global.LPMigrationCh <- ev:
	case <-ctx.Done():
	default:
		// Channel full — log and drop
		log.Warn("LP-Migrator: channel full on confirmed migration")
	}

	// Also record as an opportunity alert for oversight reporting
	state.Global.Alert("warn", "lpmigrator",
		fmt.Sprintf("✅ Migration confirmed: %s — %s → %s",
			confirmedBurn.TokenSym, confirmedBurn.SrcDEX, dexName),
		fmt.Sprintf("Liquidity: $%.0f | Window: ~%ds | Block delta: %d",
			confirmedBurn.LiqUSD, actionWindow, blockDiff),
	)
}

// getPoolInfo returns cached or freshly fetched token pair info for a pool.
func (bot *LPMigratorBot) getPoolInfo(ctx context.Context, pairAddr common.Address, dexName string) (PoolInfo, error) {
	bot.poolMu.RLock()
	if info, ok := bot.poolCache[pairAddr]; ok {
		bot.poolMu.RUnlock()
		return info, nil
	}
	bot.poolMu.RUnlock()

	// Fetch token0 via RPC
	t0Input, _ := bot.pairABI.Pack("token0")
	t0Out, err := bot.client.CallContract(ctx, ethereum.CallMsg{
		To:   &pairAddr,
		Data: t0Input,
	}, nil)
	if err != nil || len(t0Out) < 32 {
		return PoolInfo{}, fmt.Errorf("token0 call failed: %w", err)
	}
	token0 := common.BytesToAddress(t0Out[12:32])

	// Fetch token1
	t1Input, _ := bot.pairABI.Pack("token1")
	t1Out, err := bot.client.CallContract(ctx, ethereum.CallMsg{
		To:   &pairAddr,
		Data: t1Input,
	}, nil)
	if err != nil || len(t1Out) < 32 {
		return PoolInfo{}, fmt.Errorf("token1 call failed: %w", err)
	}
	token1 := common.BytesToAddress(t1Out[12:32])

	info := PoolInfo{Token0: token0, Token1: token1, DEXName: dexName}

	bot.poolMu.Lock()
	bot.poolCache[pairAddr] = info
	bot.poolMu.Unlock()

	return info, nil
}

// expirePendingBurns removes burns older than 60 seconds.
func (bot *LPMigratorBot) expirePendingBurns(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cutoff := time.Now().Add(-60 * time.Second)
			bot.pendingMu.Lock()
			for token, burns := range bot.pendingBurns {
				var fresh []PendingBurn
				for _, b := range burns {
					if b.DetectedAt.After(cutoff) {
						fresh = append(fresh, b)
					}
				}
				if len(fresh) == 0 {
					delete(bot.pendingBurns, token)
				} else {
					bot.pendingBurns[token] = fresh
				}
			}
			bot.pendingMu.Unlock()
		}
	}
}
