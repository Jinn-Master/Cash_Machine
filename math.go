package math

import (
	"fmt"
	"math/big"

	"github.com/yourname/money-printer/core/config"
)

// DexQuotes holds one output quote per DEX. Index = DEX ID constant from config.
type DexQuotes [config.DexCount]*big.Int

// BestArb finds the buy/sell DEX pair with the highest gross spread.
// Returns buyDex, sellDex (indices into DexQuotes), and spreadPct.
// buyDex is where output is HIGHEST (cheapest to buy tokenB).
// sellDex is where output is LOWEST (most expensive to sell tokenB back).
// Returns -1, -1, 0 if no valid pair found.
func BestArb(quotes DexQuotes) (buyDex, sellDex int, spreadPct float64) {
	buyDex, sellDex = -1, -1
	for i := 0; i < len(quotes); i++ {
		for j := 0; j < len(quotes); j++ {
			if i == j || quotes[i] == nil || quotes[j] == nil {
				continue
			}
			// i = potential buy DEX (high output = cheap buy)
			// j = potential sell DEX (low output = expensive sell)
			if quotes[i].Cmp(quotes[j]) <= 0 {
				continue // no spread this direction
			}
			sp := GrossSpreadPct(quotes[i], quotes[j])
			if sp > spreadPct {
				spreadPct = sp
				buyDex = i  // buy on DEX with most output (cheapest price)
				sellDex = j // sell on DEX with least output (highest price)
			}
		}
	}
	return
}

// QuickswapQuote computes the V2 constant-product output for amountIn of tokenA.
// Uses integer-only arithmetic — no float, no rounding surprises.
// Formula: amountOut = (amountIn * 997 * reserveB) / (reserveA * 1000 + amountIn * 997)
func QuickswapQuote(reserveA, reserveB, amountIn *big.Int) *big.Int {
	if reserveA.Sign() == 0 || reserveB.Sign() == 0 || amountIn.Sign() == 0 {
		return big.NewInt(0)
	}

	ainFee := new(big.Int).Mul(amountIn, big.NewInt(997))
	num := new(big.Int).Mul(ainFee, reserveB)
	den := new(big.Int).Add(
		new(big.Int).Mul(reserveA, big.NewInt(1000)),
		ainFee,
	)
	return new(big.Int).Div(num, den)
}

// FlashLoanFee returns the Aave V3 flash loan fee for a given amount.
// Uses ceiling division so we never under-repay.
func FlashLoanFee(amount *big.Int) *big.Int {
	// ceil( amount * BPS / 10000 )
	bps := big.NewInt(config.FlashFeeBPS)
	num := new(big.Int).Mul(amount, bps)
	num.Add(num, big.NewInt(9999))
	return new(big.Int).Div(num, big.NewInt(10000))
}

// ApplySlippage returns amount reduced by slippage tolerance.
// Uses integer arithmetic: amount * (10000 - slippageBPS) / 10000
func ApplySlippage(amount *big.Int) *big.Int {
	// config.SlippagePct is float (e.g. 0.5%), convert to BPS (50)
	slippageBPS := int64(config.SlippagePct * 100)
	factor := big.NewInt(10000 - slippageBPS)
	result := new(big.Int).Mul(amount, factor)
	return result.Div(result, big.NewInt(10000))
}

// GrossSpreadPct returns the percentage spread between two output amounts.
// hi = higher output (buy DEX), lo = lower output (sell DEX).
// Returns 0.0 if either is zero.
func GrossSpreadPct(hi, lo *big.Int) float64 {
	if hi.Sign() == 0 || lo.Sign() == 0 {
		return 0.0
	}
	diff := new(big.Int).Sub(hi, lo)
	if diff.Sign() <= 0 {
		return 0.0
	}
	// float64 is fine here — this is display/threshold math, not money math
	hiF := new(big.Float).SetInt(hi)
	loF := new(big.Float).SetInt(lo)
	diffF := new(big.Float).SetInt(diff)
	pct := new(big.Float).Quo(diffF, loF)
	pct.Mul(pct, big.NewFloat(100.0))
	result, _ := pct.Float64()
	_ = hiF
	return result
}

// NetProfitable checks whether the spread covers flash fee + gas cost with buffer.
// ethPerTokenA: how many ETH wei = 1 tokenA wei. Pass 1.0 for WETH, 0 to skip gas conversion.
// Returns (ok, human-readable reason).
func NetProfitable(
	tradeSize, buyOut, sellOut *big.Int,
	gasUnits uint64,
	baseFeeWei, priorityFeeWei *big.Int,
	ethPerTokenA float64,
) (bool, string) {
	feeAmount := FlashLoanFee(tradeSize)
	totalGasWei := new(big.Int).Mul(
		new(big.Int).SetUint64(gasUnits),
		new(big.Int).Add(baseFeeWei, priorityFeeWei),
	)

	var gasCostTokenA *big.Int
	if ethPerTokenA > 0 {
		gasCostF := new(big.Float).SetInt(totalGasWei)
		gasCostF.Quo(gasCostF, big.NewFloat(ethPerTokenA))
		gasCostInt, _ := gasCostF.Int(nil)
		gasCostTokenA = gasCostInt
	} else {
		gasCostTokenA = big.NewInt(0)
	}

	totalCost := new(big.Int).Add(feeAmount, gasCostTokenA)

	// spread = buyOut - sellOut (buyOut is the higher quote)
	spread := new(big.Int).Sub(buyOut, sellOut)
	if spread.Sign() <= 0 {
		return false, "negative spread"
	}

	// threshold = totalCost * PROFIT_BUFFER_PCT
	bufferNum := int64(config.ProfitBufferPct * 100)
	threshold := new(big.Int).Mul(totalCost, big.NewInt(bufferNum))
	threshold.Div(threshold, big.NewInt(100))

	if spread.Cmp(threshold) < 0 {
		return false, fmt.Sprintf(
			"spread=%s < threshold=%s [flash=%s gasTokA=%s]",
			spread, threshold, feeAmount, gasCostTokenA,
		)
	}

	net := new(big.Int).Sub(spread, totalCost)
	return true, fmt.Sprintf("estimated net ≈ %s tokenA-wei", net)
}

// FmtAmount formats a big.Int token amount for human display.
func FmtAmount(amount *big.Int, decimals int, symbol string) string {
	if amount == nil {
		return "nil"
	}
	f := new(big.Float).SetInt(amount)
	divisor := new(big.Float).SetInt(Pow10(decimals))
	f.Quo(f, divisor)
	val, _ := f.Float64()
	return fmt.Sprintf("%.6f %s", val, symbol)
}

// Pow10 returns 10^n as a big.Int.
func Pow10(n int) *big.Int {
	result := big.NewInt(1)
	ten := big.NewInt(10)
	for i := 0; i < n; i++ {
		result.Mul(result, ten)
	}
	return result
}
