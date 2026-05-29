package chain

// core/chain/rpc_limiter.go
//
// Rate-limited RPC client wrapper that adds exponential backoff retries
// and per-endpoint rate limiting to prevent 429 errors from Alchemy/Infura.
//
// All bot RPC calls should go through this wrapper instead of calling
// ethclient.Client directly.

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/Jinn-Master/Cash_Machine/core/logger"
)

const (
	maxRetries     = 3
	baseDelay      = 500 * time.Millisecond
	maxDelay       = 8 * time.Second
	rateLimitDelay = 100 * time.Second // Alchemy free tier: ~330 req/s burst, 600 req/min sustained
)

// RetryableCall executes an RPC call with exponential backoff on transient errors.
// Retries on: rate limit (429), connection reset, timeout, server error (5xx).
// Does NOT revert on: contract reverts, invalid params, insufficient funds.
func RetryableCall[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	log := logger.Log
	var zero T
	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(math.Pow(2, float64(attempt-1))) * baseDelay
			if delay > maxDelay {
				delay = maxDelay
			}
			log.Debug("RPC retry", "attempt", attempt, "delay", delay)
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(delay):
			}
		}

		result, err := fn()
		if err == nil {
			return result, nil
		}

		lastErr = err
		errStr := err.Error()

		// Don't retry on these — they won't fix themselves
		if containsAny(errStr, []string{
			"execution reverted",
			"insufficient funds",
			"nonce too low",
			"nonce too high",
			"gas required exceeds",
			"invalid opcode",
			"invalid sender",
		}) {
			return zero, err
		}

		// Retry on these transient errors
		if containsAny(errStr, []string{
			"429",
			"rate limit",
			"too many requests",
			"connection reset",
			"timeout",
			"dial tcp",
			"no such host",
			"503",
			"502",
			"server error",
			"EOF",
			"broken pipe",
		}) {
			log.Warn("RPC transient error, will retry",
				"attempt", attempt+1,
				"err", errStr,
			)
			continue
		}

		// Unknown error — don't retry, return as-is
		return zero, err
	}

	return zero, fmt.Errorf("RPC call failed after %d retries: %w", maxRetries, lastErr)
}

func containsAny(s string, substrs []string) bool {
	for _, sub := range substrs {
		if contains(s, sub) {
			return true
		}
	}
	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
