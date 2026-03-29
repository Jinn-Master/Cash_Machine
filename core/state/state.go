package state

// core/state/state.go
//
// The shared state bus — a single in-memory store all bots read from and write to.
// Uses a single RWMutex so reads are non-blocking while writes are serialised.
//
// Bots subscribe to specific event channels:
//   - NewPairCh:     FactoryWatcher → all bots (new token detected)
//   - HotPriceCh:    any bot → all bots (price update for a tracked token)
//   - ProfitEventCh: any bot → treasury (profit recorded)
//   - AlertCh:       any bot → oversight monitor (error or anomaly)

import (
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// ── Event types ───────────────────────────────────────────────────────────────

// NewTokenEvent is broadcast when the factory watcher detects a new listing.
type NewTokenEvent struct {
	TokenAddr  common.Address
	TokenSym   string
	TokenDec   int
	BaseToken  string // "USDC" or "WETH"
	BaseAddr   common.Address
	PairAddr   common.Address // first pair address seen
	Factory    string         // which factory emitted PairCreated
	DetectedAt time.Time
	DexCount   int            // how many DEXs have it so far
}

// PriceUpdate is broadcast when any bot fetches a fresh price for a token.
type PriceUpdate struct {
	TokenAddr common.Address
	TokenSym  string
	PriceUSD  float64
	Source    string // "uniV3", "aerodrome", "maverick", etc.
	UpdatedAt time.Time
}

// ProfitEvent is broadcast when any bot executes a profitable trade.
type ProfitEvent struct {
	BotName    string
	PairKey    string
	ProfitUSD  float64
	ProfitWei  *big.Int
	TxHash     common.Hash
	ExecutedAt time.Time
}

// AlertEvent is broadcast when any bot detects an error or anomaly.
type AlertEvent struct {
	Level      string // "warn", "error", "critical"
	BotName    string
	Message    string
	Detail     string
	OccurredAt time.Time
}

// LPMigrationEvent is broadcast when LP-Migrator detects a Burn+Mint sequence.
// The Scalper and Arb bots subscribe to act on the opportunity window.
type LPMigrationEvent struct {
	Token           common.Address // project token being migrated
	TokenSym        string
	SrcDEX          string         // DEX losing liquidity ("SwapBased", "BaseSwap")
	DstDEX          string         // DEX gaining liquidity ("Aerodrome", "UniV3")
	SrcPairAddr     common.Address
	DstPairAddr     common.Address // may be zero until Mint arrives
	LiqRemovedUSD   float64
	BurnTxHash      common.Hash
	BurnBlock       uint64
	DetectedAt      time.Time
	ActionWindowSec int // approx seconds the arb window stays open
}

// RWAOracleEvent is broadcast when the macro oracle detects a real-world rate
// change not yet reflected in on-chain DEX prices.
type RWAOracleEvent struct {
	AssetSym            string
	AssetAddr           common.Address
	OldNAV              float64
	NewNAV              float64
	DEXPrice            float64
	DiscrepancyPct      float64
	Direction           string // "LONG_ASSET" or "SHORT_ASSET"
	Source              string // "FRED", "BackedFinance", "Chainlink", "Pyth"
	DetectedAt          time.Time
	ExpectedUpdateBlock uint64
}

// ── Shared state ──────────────────────────────────────────────────────────────

type SharedState struct {
	mu sync.RWMutex

	// Live token price cache: tokenAddr → PriceUpdate
	prices map[common.Address]PriceUpdate

	// Hot tokens: tokens seen by factory watcher in last 24h
	hotTokens map[common.Address]NewTokenEvent

	// Tracked pair keys (for dedup): "USDC/TOKEN"
	trackedPairs map[string]bool

	// Running profit totals per bot
	profitByBot map[string]float64

	// Working capital (tracked by treasury)
	workingCapitalUSD float64

	// Cumulative profit (all time)
	totalProfitUSD float64

	// Last gas price (gwei) — updated by gas monitor
	gasPriceGwei float64

	// ── Event channels (buffered, non-blocking) ───────────────────────────────
	NewPairCh       chan NewTokenEvent
	HotPriceCh      chan PriceUpdate
	ProfitEventCh   chan ProfitEvent
	AlertCh         chan AlertEvent
	LPMigrationCh   chan LPMigrationEvent // LP-Migrator → scalper + arb
	RWAOracleCh     chan RWAOracleEvent   // RWA oracle → arb bot
}

var Global = &SharedState{
	prices:        make(map[common.Address]PriceUpdate),
	hotTokens:     make(map[common.Address]NewTokenEvent),
	trackedPairs:  make(map[string]bool),
	profitByBot:   make(map[string]float64),
	gasPriceGwei:  0.1,

	NewPairCh:     make(chan NewTokenEvent, 256),
	HotPriceCh:    make(chan PriceUpdate, 512),
	ProfitEventCh: make(chan ProfitEvent, 128),
	AlertCh:       make(chan AlertEvent, 128),
	LPMigrationCh: make(chan LPMigrationEvent, 64),
	RWAOracleCh:   make(chan RWAOracleEvent, 64),
}

// ── Price cache ───────────────────────────────────────────────────────────────

func (s *SharedState) SetPrice(addr common.Address, update PriceUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prices[addr] = update
	// Non-blocking broadcast
	select {
	case s.HotPriceCh <- update:
	default:
	}
}

func (s *SharedState) GetPrice(addr common.Address) (PriceUpdate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.prices[addr]
	return p, ok
}

// ── Hot tokens ────────────────────────────────────────────────────────────────

func (s *SharedState) AddHotToken(ev NewTokenEvent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.hotTokens[ev.TokenAddr]; exists {
		return false // already known
	}
	s.hotTokens[ev.TokenAddr] = ev
	// Non-blocking broadcast to all bots
	select {
	case s.NewPairCh <- ev:
	default:
	}
	return true
}

func (s *SharedState) GetHotToken(addr common.Address) (NewTokenEvent, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ev, ok := s.hotTokens[addr]
	return ev, ok
}

func (s *SharedState) HotTokenCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.hotTokens)
}

// PruneHotTokens removes tokens older than maxAge (called hourly).
func (s *SharedState) PruneHotTokens(maxAge time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	pruned := 0
	for addr, ev := range s.hotTokens {
		if ev.DetectedAt.Before(cutoff) {
			delete(s.hotTokens, addr)
			pruned++
		}
	}
	return pruned
}

// ── Pair tracking ─────────────────────────────────────────────────────────────

func (s *SharedState) MarkTracked(pairKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trackedPairs[pairKey] = true
}

func (s *SharedState) IsTracked(pairKey string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.trackedPairs[pairKey]
}

// ── Profit tracking ───────────────────────────────────────────────────────────

func (s *SharedState) RecordProfit(ev ProfitEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profitByBot[ev.BotName] += ev.ProfitUSD
	s.totalProfitUSD += ev.ProfitUSD
	select {
	case s.ProfitEventCh <- ev:
	default:
	}
}

func (s *SharedState) ProfitSummary() map[string]float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]float64, len(s.profitByBot)+1)
	for k, v := range s.profitByBot {
		out[k] = v
	}
	out["total"] = s.totalProfitUSD
	return out
}

func (s *SharedState) TotalProfit() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.totalProfitUSD
}

// ── Treasury state ────────────────────────────────────────────────────────────

func (s *SharedState) SetWorkingCapital(usd float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workingCapitalUSD = usd
}

func (s *SharedState) WorkingCapital() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.workingCapitalUSD
}

// ── Gas price ─────────────────────────────────────────────────────────────────

func (s *SharedState) SetGasPrice(gwei float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gasPriceGwei = gwei
}

func (s *SharedState) GasPrice() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gasPriceGwei
}

func (s *SharedState) GasIsLow() bool {
	return s.GasPrice() <= 0.05 // Base gwei threshold for dust collection
}

// ── Alerts ────────────────────────────────────────────────────────────────────

func (s *SharedState) Alert(level, bot, msg, detail string) {
	ev := AlertEvent{
		Level:      level,
		BotName:    bot,
		Message:    msg,
		Detail:     detail,
		OccurredAt: time.Now(),
	}
	select {
	case s.AlertCh <- ev:
	default:
	}
}
