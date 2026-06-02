package chain

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ── Cross-DEX Liquidity Verification ──────────────────────────────────────

// LiquidityCheckResult contains the detailed result of a liquidity check.
type LiquidityCheckResult struct {
	TokenA            common.Address
	TokenB            common.Address
	DexID             uint8
	HasLiquidity      bool
	EstimatedAmountOut *big.Int // amount out if amountIn is provided
	ReserveA          *big.Int
	ReserveB          *big.Int
	ErrorMsg          string
	CheckedAt         time.Time
}

// VerifyCrossLiquidityExists verifies that BOTH legs of the arbitrage trade
// have sufficient liquidity. Returns false if either leg fails validation.
//
// This function prevents submitting trades that will revert on-chain because
// the second DEX lacks the pair or has insufficient liquidity.
func VerifyCrossLiquidityExists(
	ctx context.Context,
	client *ethclient.Client,
	tokenA, tokenB common.Address,
	buyDex, sellDex uint8,
	loanAmount *big.Int,
	minAmountOut *big.Int, // minimum amount from first swap
) (bool, error) {

	// Check if tokenA -> tokenB exists on buyDex
	buyResult, err := CheckDexLiquidity(ctx, client, tokenA, tokenB, buyDex, loanAmount)
	if err != nil || !buyResult.HasLiquidity {
		return false, fmt.Errorf("buyDex %d: %w", buyDex, err)
	}

	// Verify output from first swap is sufficient
	if buyResult.EstimatedAmountOut == nil || buyResult.EstimatedAmountOut.Cmp(big.NewInt(0)) <= 0 {
		return false, fmt.Errorf("buyDex %d: zero output from first swap", buyDex)
	}

	// Check if tokenB -> tokenA exists on sellDex using the output from first swap
	sellResult, err := CheckDexLiquidity(ctx, client, tokenB, tokenA, sellDex, buyResult.EstimatedAmountOut)
	if err != nil || !sellResult.HasLiquidity {
		return false, fmt.Errorf("sellDex %d: %w", sellDex, err)
	}

	// Verify final output meets minimum profit requirement
	if sellResult.EstimatedAmountOut == nil || sellResult.EstimatedAmountOut.Cmp(loanAmount) <= 0 {
		return false, fmt.Errorf("sellDex %d: insufficient output (would fail to repay loan)", sellDex)
	}

	return true, nil
}

// CheckDexLiquidity checks if a specific DEX has the token pair and sufficient liquidity.
// Returns a LiquidityCheckResult with detailed information.
func CheckDexLiquidity(
	ctx context.Context,
	client *ethclient.Client,
	tokenIn, tokenOut common.Address,
	dexID uint8,
	amountIn *big.Int,
) (LiquidityCheckResult, error) {

	result := LiquidityCheckResult{
		TokenA:    tokenIn,
		TokenB:    tokenOut,
		DexID:     dexID,
		CheckedAt: time.Now(),
	}

	// Quote on the specific DEX
	amountOut, err := QuoteOnDex(ctx, client, tokenIn, tokenOut, dexID, amountIn)
	if err != nil {
		result.HasLiquidity = false
		result.ErrorMsg = err.Error()
		return result, err
	}

	// Check if output is meaningful (not zero)
	if amountOut == nil || amountOut.Cmp(big.NewInt(0)) <= 0 {
		result.HasLiquidity = false
		result.ErrorMsg = "zero output"
		return result, nil
	}

	result.HasLiquidity = true
	result.EstimatedAmountOut = amountOut
	return result, nil
}

// QuoteOnDex gets a quote for a swap on a specific DEX (0-5).
// Uses the appropriate quoter/method for each DEX type.
func QuoteOnDex(
	ctx context.Context,
	client *ethclient.Client,
	tokenIn, tokenOut common.Address,
	dexID uint8,
	amountIn *big.Int,
) (*big.Int, error) {

	switch dexID {
	case 0: // UniV3
		// Use UniV3 quoter with default 3000 fee (0.3%) — most common for USDC pairs
		return UniV3Quote(ctx, client, UniV3QuoterAddr, tokenIn, tokenOut, 3000, amountIn)

	case 1: // Aerodrome Volatile
		// Use Aerodrome quoter with volatile pool
		return AerodromeQuote(ctx, client, AeroQuoterAddr, tokenIn, tokenOut, amountIn, false)

	case 2: // BaseSwap (V2)
		// BaseSwap is V2 compatible — get pair and read reserves
		return V2QuoteFromFactory(ctx, client, BaseSwapFactoryAddr, BaseSwapRouterAddr, tokenIn, tokenOut, amountIn)

	case 3: // Aerodrome Stable
		// Use Aerodrome quoter with stable pool
		return AerodromeQuote(ctx, client, AeroQuoterAddr, tokenIn, tokenOut, amountIn, true)

	case 4: // SwapBased (V2)
		// SwapBased is V2 compatible
		return V2QuoteFromFactory(ctx, client, SwapBasedFactoryAddr, SwapBasedRouterAddr, tokenIn, tokenOut, amountIn)

	case 5: // Maverick V2
		// Maverick V2 uses directional liquidity — need to get pool and quote
		return MaverickQuote(ctx, client, MaverickV2QuoterAddr, tokenIn, tokenOut, amountIn)

	default:
		return nil, fmt.Errorf("unknown dex id: %d", dexID)
	}
}

// V2QuoteFromFactory gets a quote for V2-compatible DEXes by:
// 1. Finding the pair address from factory
// 2. Reading reserves
// 3. Calculating output using constant product formula
func V2QuoteFromFactory(
	ctx context.Context,
	client *ethclient.Client,
	factory, router, tokenIn, tokenOut common.Address,
	amountIn *big.Int,
) (*big.Int, error) {

	// Get pair address
	pair, err := GetV2Pair(ctx, client, factory, tokenIn, tokenOut)
	if err != nil {
		return nil, fmt.Errorf("GetV2Pair: %w", err)
	}

	// Check if pair is zero address (doesn't exist)
	if pair == (common.Address{}) {
		return nil, fmt.Errorf("pair does not exist on this factory")
	}

	// Get reserves
	reserves, err := GetReserves(ctx, client, pair)
	if err != nil {
		return nil, fmt.Errorf("GetReserves: %w", err)
	}

	// Determine if tokenIn is token0 or token1
	token0, err := GetToken0(ctx, client, pair)
	if err != nil {
		return nil, fmt.Errorf("GetToken0: %w", err)
	}

	var resIn, resOut *big.Int
	if token0 == tokenIn {
		resIn = reserves.Reserve0
		resOut = reserves.Reserve1
	} else {
		resIn = reserves.Reserve1
		resOut = reserves.Reserve0
	}

	// Standard constant product: y = (x * Y) / (X + x)
	// Where x = amountIn, X = reserveIn, Y = reserveOut
	amountInWith997 := new(big.Int).Mul(amountIn, big.NewInt(997))
	numerator := new(big.Int).Mul(amountInWith997, resOut)
	denominator := new(big.Int).Add(new(big.Int).Mul(resIn, big.NewInt(1000)), amountInWith997)

	if denominator.Cmp(big.NewInt(0)) == 0 {
		return nil, fmt.Errorf("denominator is zero")
	}

	amountOut := new(big.Int).Div(numerator, denominator)
	return amountOut, nil
}

// MaverickQuote gets a quote for Maverick V2.
// This is a simplified version — in production, you'd call the Maverick quoter contract.
func MaverickQuote(
	ctx context.Context,
	client *ethclient.Client,
	quoter, tokenIn, tokenOut common.Address,
	amountIn *big.Int,
) (*big.Int, error) {

	// For now, return a placeholder error if quoter is not available
	// In production, implement the full Maverick quoter call
	if quoter == (common.Address{}) {
		return nil, fmt.Errorf("maverick quoter not configured")
	}

	// TODO: Call Maverick quoter contract
	// This requires the Maverick PoolLens or Quoter ABI
	return nil, fmt.Errorf("maverick quotes not yet implemented in liquidity_verify")
}

// ── Contract address helpers ──────────────────────────────────────────────

// These should match core/config/config.go
var (
	UniV3QuoterAddr       = common.HexToAddress("0x3d4e44Eb1374240CE5F1B136aa68B6a572e6f586")
	AeroQuoterAddr        = common.HexToAddress("0x254cF9E1E6e233aa1AC962CB9B05b2cfeAaE15b0")
	BaseSwapFactoryAddr   = common.HexToAddress("0xFDa619b6d20975be80A10332cD39b9a4b0FAa8BB")
	BaseSwapRouterAddr    = common.HexToAddress("0x327Df1E6de05895d2ab08513aaDD9313Fe505d86")
	SwapBasedFactoryAddr  = common.HexToAddress("0x04C9f118d21e8B767D2e50C946f0cC9F6C367300")
	SwapBasedRouterAddr   = common.HexToAddress("0xaaa3b1F1bd7BCc97fD1917c18ADE665C5D31F066")
	MaverickV2QuoterAddr  = common.HexToAddress("0x6eBF4D8b42b27bbBF0a8cFc0781a0D63494FBB18")
)
