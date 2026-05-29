package math

import (
	"math/big"
	"testing"
)

func TestQuickswapQuote(t *testing.T) {
	tests := []struct {
		name      string
		reserveA  int64
		reserveB  int64
		amountIn int64
		wantZero  bool
	}{
		{
			name:      "normal swap 1000 -> USDC/WETH",
			reserveA:  1_000_000_000_000_000_000, // 1000 WETH
			reserveB:  3_000_000_000_000,         // 3M USDC
			amountIn: 1_000_000_000_000_000,     // 1 WETH
			wantZero:  false,
		},
		{
			name:      "zero reserve A",
			reserveA:  0,
			reserveB:  1_000_000,
			amountIn: 100,
			wantZero:  true,
		},
		{
			name:      "zero reserve B",
			reserveA:  1_000_000,
			reserveB:  0,
			amountIn: 100,
			wantZero:  true,
		},
		{
			name:      "zero amount in",
			reserveA:  1_000_000,
			amountIn: 0,
			reserveB:  1_000_000,
			wantZero:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := QuickswapQuote(
				big.NewInt(tt.reserveA),
				big.NewInt(tt.reserveB),
				big.NewInt(tt.amountIn),
			)
			if tt.wantZero && result.Sign() != 0 {
				t.Errorf("expected zero, got %s", result.String())
			}
			if !tt.wantZero && result.Sign() <= 0 {
				t.Errorf("expected positive, got %s", result.String())
			}
		})
	}
}

func TestQuickswapQuote_ConstantProduct(t *testing.T) {
	// x * y = k invariant: swapping should always reduce k within rounding
	reserveA := big.NewInt(1_000_000_000_000_000_000) // 1000 ETH
	reserveB := big.NewInt(3_000_000_000_000)         // 3M USDC
	amountIn := big.NewInt(1_000_000_000_000_000)     // 1 ETH

	out := QuickswapQuote(reserveA, reserveB, amountIn)
	if out.Sign() <= 0 {
		t.Fatal("expected positive output")
	}

	// Output should be less than the "ideal" price (no fees)
	idealOut := new(big.Int).Mul(amountIn, reserveB)
	idealOut.Div(idealOut, reserveA)
	if out.Cmp(idealOut) >= 0 {
		t.Errorf("with-fees output %s should be < ideal %s", out, idealOut)
	}
}

func TestFlashLoanFee(t *testing.T) {
	// Aave V3 flash loan fee is 0.05% (5 bps)
	// 1000000 * 5 / 10000 = 500
	amount := big.NewInt(1_000_000)
	fee := FlashLoanFee(amount)
	expected := big.NewInt(500)
	if fee.Cmp(expected) != 0 {
		t.Errorf("FlashLoanFee(1M) = %s, want %s", fee, expected)
	}

	// Ceiling division: amount=1, fee=5bps → ceil(0.0005) = 1
	smallAmount := big.NewInt(1)
	smallFee := FlashLoanFee(smallAmount)
	if smallFee.Cmp(big.NewInt(1)) != 0 {
		t.Errorf("FlashLoanFee(1) = %s, want 1 (ceiling)", smallFee)
	}
}

func TestApplySlippage(t *testing.T) {
	// Default slippage is 0.5% = 50 bps
	// amount * (10000 - 50) / 10000 = amount * 9950 / 10000
	amount := big.NewInt(1_000_000)
	result := ApplySlippage(amount)
	expected := big.NewInt(995_000) // 1M * 0.995
	if result.Cmp(expected) != 0 {
		t.Errorf("ApplySlippage(1M) = %s, want %s", result, expected)
	}
}

func TestGrossSpreadPct(t *testing.T) {
	tests := []struct {
		name string
		hi   int64
		lo   int64
		want float64
	}{
		{"10% spread", 110, 100, 10.0},
		{"zero spread", 100, 100, 0.0},
		{"negative spread", 90, 100, 0.0},
		{"50% spread", 150, 100, 50.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GrossSpreadPct(big.NewInt(tt.hi), big.NewInt(tt.lo))
			if got < tt.want-0.01 || got > tt.want+0.01 {
				t.Errorf("GrossSpreadPct(%d, %d) = %.4f, want %.4f", tt.hi, tt.lo, got, tt.want)
			}
		})
	}
}

func TestNetProfitable(t *testing.T) {
	// Large spread: 200 USDC profit on 200 USDC trade, gas cost negligible
	tradeSize := big.NewInt(200_000_000)    // 200 USDC
	buyOut := big.NewInt(1_200_000_000)     // 1200 tokenB output (buy cheap)
	sellOut := big.NewInt(1_000_000_000)    // 1000 tokenA back (sell at higher price)
	baseFee := big.NewInt(1e9)             // 1 gwei
	priorityFee := big.NewInt(1e8)         // 0.1 gwei

	ok, reason := NetProfitable(tradeSize, buyOut, sellOut, 300000, baseFee, priorityFee, 0)
	if !ok {
		t.Errorf("expected profitable, got: %s", reason)
	}
}

func TestNetProfitable_Unprofitable(t *testing.T) {
	// Tiny spread with high gas: should be unprofitable
	tradeSize := big.NewInt(200_000_000)
	buyOut := big.NewInt(1_001_000_000)     // 1001 tokenB
	sellOut := big.NewInt(1_000_000_000)    // 1000 tokenA back → 1 USDC spread
	// High gas: 500k gas * (50+10)gwei = 30M gwei = 0.03 ETH ≈ $60 at $2000/ETH
	baseFee := big.NewInt(50e9)             // 50 gwei
	priorityFee := big.NewInt(10e9)         // 10 gwei

	ok, _ := NetProfitable(tradeSize, buyOut, sellOut, 500000, baseFee, priorityFee, 1.0) // 1.0 ethPerTokenA for WETH
	if ok {
		t.Errorf("expected unprofitable for tiny spread with high gas, got ok")
	}
}

func TestBestArb(t *testing.T) {
	// DEX 0: 100, DEX 1: 110, DEX 2: 95
	// Best arb: buy on DEX 1 (highest output = cheapest tokenB), sell on DEX 2 (lowest output = most expensive tokenB)
	var quotes DexQuotes
	quotes[0] = big.NewInt(100)
	quotes[1] = big.NewInt(110)
	quotes[2] = big.NewInt(95)

	buyDex, sellDex, spread := BestArb(quotes)
	if buyDex != 1 {
		t.Errorf("expected buyDex=1 (DEX with highest output = cheapest), got %d", buyDex)
	}
	if sellDex != 2 {
		t.Errorf("expected sellDex=2 (DEX with lowest output = most expensive), got %d", sellDex)
	}
	// Spread = (110 - 95) / 95 * 100 = 15.79%
	if spread < 15.0 || spread > 16.0 {
		t.Errorf("expected spread ~15.79%%, got %.2f", spread)
	}
}

func TestFmtAmount(t *testing.T) {
	got := FmtAmount(big.NewInt(1_500_000), 6, "USDC")
	if got != "1.500000 USDC" {
		t.Errorf("FmtAmount = %q, want %q", got, "1.500000 USDC")
	}
}

func TestPow10(t *testing.T) {
	if Pow10(0).Cmp(big.NewInt(1)) != 0 {
		t.Error("Pow10(0) != 1")
	}
	if Pow10(6).Cmp(big.NewInt(1_000_000)) != 0 {
		t.Error("Pow10(6) != 1000000")
	}
	if Pow10(18).Cmp(big.NewInt(1e18)) != 0 {
		t.Error("Pow10(18) != 1e18")
	}
}
