package arb

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/Jinn-Master/Cash_Machine/core/chain"
	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
)

// ── Opportunity validation with cross-DEX checks ───────────────────────────

// ValidateOpportunity checks if a potential arbitrage opportunity is viable
// on BOTH DEXes before submitting to the chain.
//
// This prevents revert-causing trades and logs exactly why an opportunity failed.
func ValidateOpportunity(
	ctx context.Context,
	client *ethclient.Client,
	tokenA, tokenB common.Address,
	buyDex, sellDex uint8,
	loanAmount *big.Int,
	expectedProfitUSD float64,
) (bool, map[string]interface{}, error) {

	log := logger.Log
	details := make(map[string]interface{})

	details["tokenA"] = tokenA.Hex()
	details["tokenB"] = tokenB.Hex()
	details["buyDex"] = config.DexNames[buyDex]
	details["sellDex"] = config.DexNames[sellDex]
	details["loanAmount"] = loanAmount.String()
	details["expectedProfitUSD"] = expectedProfitUSD

	// Step 1: Verify both DEXes have the pair with sufficient liquidity
	hasLiquidity, err := chain.VerifyCrossLiquidityExists(
		ctx, client,
		tokenA, tokenB,
		buyDex, sellDex,
		loanAmount,
		big.NewInt(0), // minimum from first swap (not enforced here, just checked later)
	)

	if err != nil {
		details["validation_error"] = err.Error()
		log.Warn("opportunity rejected: liquidity check failed",
			"tokenA", tokenA.Hex(),
			"tokenB", tokenB.Hex(),
			"buyDex", config.DexNames[buyDex],
			"sellDex", config.DexNames[sellDex],
			"error", err,
		)
		return false, details, err
	}

	if !hasLiquidity {
		details["validation_error"] = "insufficient liquidity on one or both DEXes"
		log.Warn("opportunity rejected: insufficient liquidity",
			"tokenA", tokenA.Hex(),
			"tokenB", tokenB.Hex(),
			"buyDex", config.DexNames[buyDex],
			"sellDex", config.DexNames[sellDex],
		)
		return false, details, nil
	}

	// Step 2: Verify minimum profit threshold
	minProfitUSD := expectedProfitUSD * (1.0 - config.SlippagePct/100.0)
	if minProfitUSD < 0.001 { // ignore gas costs for now — used as safety threshold
		details["validation_error"] = fmt.Sprintf("profit below minimum: %.4f USD", minProfitUSD)
		log.Debug("opportunity rejected: profit below threshold",
			"tokenA", tokenA.Hex(),
			"expectedProfit", expectedProfitUSD,
			"minProfit", minProfitUSD,
		)
		return false, details, nil
	}

	// Opportunity is valid
	details["validated"] = true
	details["minProfitUSD"] = minProfitUSD
	details["validatedAt"] = time.Now().Format(time.RFC3339Nano)

	log.Info("✅ opportunity validated and ready for execution",
		"tokenA", tokenA.Hex()[:8],
		"tokenB", tokenB.Hex()[:8],
		"buyDex", config.DexNames[buyDex],
		"sellDex", config.DexNames[sellDex],
		"expectedProfit", fmt.Sprintf("$%.2f", expectedProfitUSD),
	)

	return true, details, nil
}

// CheckPairReadiness returns which DEXes have the pair and their liquidity depth.
// Used by the factory watcher to determine when a pair is tradeable.
func CheckPairReadiness(
	ctx context.Context,
	client *ethclient.Client,
	tokenA, tokenB common.Address,
	checkAmount *big.Int, // amount to quote on each DEX
) map[string]interface{} {

	result := map[string]interface{}{
		"tokenA":       tokenA.Hex(),
		"tokenB":       tokenB.Hex(),
		"checkAmount":  checkAmount.String(),
		"checkedAt":    time.Now().Format(time.RFC3339Nano),
		"readyDexes":   []string{},
		"dexDetails":   make(map[string]interface{}),
		"readyForArb":  false,
	}

	readyCount := 0
	dexDetails := make(map[string]interface{})

	// Check each DEX
	for dexID := uint8(0); dexID < config.DexCount; dexID++ {
		dexName := config.DexNames[dexID]
		liqResult, err := chain.CheckDexLiquidity(ctx, client, tokenA, tokenB, dexID, checkAmount)

		dexStatus := map[string]interface{}{
			"hasLiquidity": false,
			"amountOut":    "0",
			"error":        "",
		}

		if err != nil {
			dexStatus["error"] = err.Error()
		} else if liqResult.HasLiquidity && liqResult.EstimatedAmountOut != nil {
			dexStatus["hasLiquidity"] = true
			dexStatus["amountOut"] = liqResult.EstimatedAmountOut.String()
			readyCount++
		}

		dexDetails[dexName] = dexStatus
	}

	result["dexDetails"] = dexDetails
	result["readyDexCount"] = readyCount

	// Pair is ready for arb if at least 2 DEXes have it
	if readyCount >= 2 {
		result["readyForArb"] = true
	}

	return result
}

// PreflightCheck runs all safety checks before submitting a flash loan TX.
// Returns detailed diagnostics if validation fails.
func PreflightCheck(
	ctx context.Context,
	client *ethclient.Client,
	tokenA, tokenB common.Address,
	buyDex, sellDex uint8,
	loanAmount *big.Int,
	minProfit *big.Int,
) (bool, string, error) {

	log := logger.Log

	// 1. Validate DEX IDs
	if buyDex >= config.DexCount || sellDex >= config.DexCount {
		msg := fmt.Sprintf("invalid DEX IDs: buyDex=%d, sellDex=%d (max=%d)", buyDex, sellDex, config.DexCount-1)
		return false, msg, fmt.Errorf(msg)
	}

	if buyDex == sellDex {
		msg := "cannot arbitrage same DEX"
		return false, msg, fmt.Errorf(msg)
	}

	// 2. Verify cross-DEX liquidity
	hasLiquidity, err := chain.VerifyCrossLiquidityExists(
		ctx, client,
		tokenA, tokenB,
		buyDex, sellDex,
		loanAmount,
		big.NewInt(0),
	)

	if err != nil {
		msg := fmt.Sprintf("liquidity check failed: %v", err)
		log.Error("preflight: liquidity verification failed", "error", err)
		return false, msg, err
	}

	if !hasLiquidity {
		msg := "insufficient liquidity on one or both DEXes"
		log.Warn("preflight: pair not ready", "tokenA", tokenA.Hex(), "tokenB", tokenB.Hex())
		return false, msg, nil
	}

	// 3. Verify minimum profit threshold
	if minProfit.Cmp(big.NewInt(0)) <= 0 {
		msg := "minimum profit must be > 0"
		return false, msg, fmt.Errorf(msg)
	}

	log.Debug("✅ preflight checks passed",
		"buyDex", config.DexNames[buyDex],
		"sellDex", config.DexNames[sellDex],
		"minProfit", minProfit.String(),
	)

	return true, "", nil
}
