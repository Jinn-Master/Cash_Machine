package state

// ── Extension to core/state/state.go for cross-DEX pair tracking ──────────

import (
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// PairDexStatus tracks which DEXes have a specific token pair
// and their liquidity depth at last check.
type PairDexStatus struct {
	TokenA      common.Address
	TokenB      common.Address
	DexID       uint8  // 0-5
	DexName     string // "UniV3", "Aerodrome", etc.
	HasPair     bool
	LiquidityOK bool     // true if quotes succeed with meaningful output
	LastAmountOut int64  // last quoted amount out (for tracking depth changes)
	LastCheckedAt time.Time
	CheckFailCount int     // consecutive failures
}

// CrossDexPairTracker maintains per-token-pair status across all 6 DEXes.
// Used to determine when a pair is ready for arbitrage (appears on 2+ DEXes).
type CrossDexPairTracker struct {
	mu sync.RWMutex
	// Key: "0xTokenA-0xTokenB" (lowercase)
	pairStatusMap map[string][6]*PairDexStatus
}

// NewCrossDexPairTracker creates a new tracker instance.
func NewCrossDexPairTracker() *CrossDexPairTracker {
	return &CrossDexPairTracker{
		pairStatusMap: make(map[string][6]*PairDexStatus),
	}
}

// UpdatePairDexStatus updates the status of a pair on a specific DEX.
func (t *CrossDexPairTracker) UpdatePairDexStatus(tokenA, tokenB common.Address, dexID uint8, hasPair, liquidityOK bool, amountOut int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	key := makePairKey(tokenA, tokenB)
	if _, exists := t.pairStatusMap[key]; !exists {
		// Initialize all 6 DEX statuses for this pair
		t.pairStatusMap[key] = [6]*PairDexStatus{}
	}

	status := t.pairStatusMap[key][dexID]
	if status == nil {
		status = &PairDexStatus{
			TokenA:  tokenA,
			TokenB:  tokenB,
			DexID:   dexID,
		}
		t.pairStatusMap[key][dexID] = status
	}

	status.HasPair = hasPair
	status.LiquidityOK = liquidityOK
	status.LastAmountOut = amountOut
	status.LastCheckedAt = time.Now()

	if !hasPair || !liquidityOK {
		status.CheckFailCount++
	} else {
		status.CheckFailCount = 0
	}
}

// GetPairReadiness returns which DEXes have the pair and are ready.
// Returns count of ready DEXes and the individual status map.
func (t *CrossDexPairTracker) GetPairReadiness(tokenA, tokenB common.Address) (readyCount int, statuses [6]*PairDexStatus) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	key := makePairKey(tokenA, tokenB)
	if statuses, exists := t.pairStatusMap[key]; exists {
		for _, status := range statuses {
			if status != nil && status.HasPair && status.LiquidityOK {
				readyCount++
			}
		}
		return readyCount, statuses
	}
	return 0, [6]*PairDexStatus{}
}

// IsReadyForArb returns true if the pair is available on 2+ DEXes.
func (t *CrossDexPairTracker) IsReadyForArb(tokenA, tokenB common.Address) bool {
	readyCount, _ := t.GetPairReadiness(tokenA, tokenB)
	return readyCount >= 2
}

// GetReadyDexPairs returns a slice of DEX IDs that have the pair ready.
func (t *CrossDexPairTracker) GetReadyDexPairs(tokenA, tokenB common.Address) []uint8 {
	_, statuses := t.GetPairReadiness(tokenA, tokenB)
	var readyDexes []uint8
	for dexID, status := range statuses {
		if status != nil && status.HasPair && status.LiquidityOK {
			readyDexes = append(readyDexes, uint8(dexID))
		}
	}
	return readyDexes
}

// CleanupStale removes pairs that haven't been checked in maxAge.
// Called periodically (e.g., hourly) to free memory.
func (t *CrossDexPairTracker) CleanupStale(maxAge time.Duration) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	cleaned := 0

	for key, statuses := range t.pairStatusMap {
		allStale := true
		for _, status := range statuses {
			if status != nil && status.LastCheckedAt.After(cutoff) {
				allStale = false
				break
			}
		}
		if allStale {
			delete(t.pairStatusMap, key)
			cleaned++
		}
	}
	return cleaned
}

// makePairKey creates a consistent key for a token pair.
func makePairKey(tokenA, tokenB common.Address) string {
	return tokenA.Hex() + "-" + tokenB.Hex()
}

// ── Extension to SharedState ──────────────────────────────────────────────

// Add these fields to the SharedState struct in core/state/state.go:
//
//   crossDexTracker *CrossDexPairTracker
//
// And initialize in var Global:
//
//   crossDexTracker: NewCrossDexPairTracker(),
//
// Then add these methods to SharedState:

// GetCrossDexTracker returns the cross-DEX pair tracker.
// Use this to query pair readiness across all DEXes.
func (s *SharedState) GetCrossDexTracker() *CrossDexPairTracker {
	// This requires adding crossDexTracker field to SharedState first
	// For now, this is a placeholder for integration
	return nil // FIXME: implement after adding field
}

// PairReadyOnMultipleDexes returns true if a pair is available on 2+ DEXes.
// Convenience method for arb bot to decide whether to trade.
func (s *SharedState) PairReadyOnMultipleDexes(tokenA, tokenB common.Address) bool {
	// This requires the crossDexTracker field
	// return s.crossDexTracker.IsReadyForArb(tokenA, tokenB) // FIXME: uncomment after integration
	return true // temporary: assume ready
}
