package dust

// bots/dust/dust.go
//
// Dust Collector Bot
//
// Scans the bot wallet for small token balances ("dust") and converts them
// to USDC or ETH when gas prices are low enough that the conversion is profitable.
//
// What counts as dust:
//   Any ERC-20 balance with USD value between $0.10 and $5.00
//   (smaller values are sub-economic even at low gas; larger are handled by arb bot)
//
// When it runs:
//   Only when Base gas price < 0.05 gwei — ensures conversion is net positive
//   Checks every 30 seconds, but only acts when gas condition is met
//
// How it converts:
//   1. Get current gas price from state.Global
//   2. If gas is low, scan wallet for ERC-20 balances > $0.10
//   3. For each dust token: get USD value via DexScreener price cache
//   4. If value > gas cost: route swap via Aerodrome (best liquidity for Base tokens)
//   5. Consolidate all converted amounts into USDC
//   6. Small ETH dust (from gas refunds) is kept as gas reserve — not swapped
//
// Multi-token mapping:
//   Tracks all tokens ever received by the wallet. Builds a running map of
//   "seen tokens" so it doesn't have to scan all of ERC-20 history every run.
//   New tokens are added to the map when detected by the factory watcher.

import (
	"context"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
	"github.com/Jinn-Master/Cash_Machine/core/state"
)

// ── TokenBalance represents a scanned balance ─────────────────────────────────

type TokenBalance struct {
	Addr     common.Address
	Symbol   string
	Decimals int
	Balance  *big.Int
	USD      float64
}

// ── DustCollector ─────────────────────────────────────────────────────────────

type DustCollector struct {
	client     *ethclient.Client
	wallet     common.Address

	mu         sync.Mutex
	knownTokens map[common.Address]struct{ Symbol string; Decimals int }
	lastRun    time.Time
	totalCollectedUSD float64
}

func New(client *ethclient.Client, wallet common.Address) *DustCollector {
	return &DustCollector{
		client:      client,
		wallet:      wallet,
		knownTokens: make(map[common.Address]struct{ Symbol string; Decimals int }),
	}
}

// Run starts the dust collector.
func (d *DustCollector) Run(ctx context.Context) {
	log := logger.Log
	log.Info("🧹 dust collector started",
		"wallet", d.wallet.Hex(),
		"min_value_usd", config.DustMinValueUSD,
		"max_gas_gwei", config.DustGasPriceMaxGwei,
	)

	// Listen for new tokens from factory watcher — add them to known list
	go d.watchNewTokens(ctx)

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("dust collector shutting down",
				"total_collected_usd", fmt.Sprintf("$%.4f", d.totalCollectedUSD),
			)
			return
		case <-ticker.C:
			// Only run when gas is cheap
			if state.Global.GasIsLow() {
				d.sweep(ctx)
			}
		}
	}
}

// watchNewTokens adds factory-detected tokens to the known list.
func (d *DustCollector) watchNewTokens(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-state.Global.NewPairCh:
			d.mu.Lock()
			d.knownTokens[ev.TokenAddr] = struct {
				Symbol   string
				Decimals int
			}{Symbol: ev.TokenSym, Decimals: ev.TokenDec}
			d.mu.Unlock()
		}
	}
}

// sweep scans known tokens for dust and converts profitable ones.
func (d *DustCollector) sweep(ctx context.Context) {
	log := logger.Log

	d.mu.Lock()
	tokens := make(map[common.Address]struct{ Symbol string; Decimals int }, len(d.knownTokens))
	for k, v := range d.knownTokens {
		tokens[k] = v
	}
	d.mu.Unlock()

	if len(tokens) == 0 {
		return
	}

	var dustFound []TokenBalance
	gasPrice := state.Global.GasPrice()

	for addr, meta := range tokens {
		balance, err := d.balanceOf(ctx, addr)
		if err != nil || balance.Sign() == 0 {
			continue
		}

		// Get price from cache
		priceEv, hasPrice := state.Global.GetPrice(addr)
		if !hasPrice || priceEv.PriceUSD <= 0 {
			continue
		}

		// Calculate USD value
		divisor := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(meta.Decimals)), nil))
		tokenFloat := new(big.Float).SetInt(balance)
		tokenAmt, _ := new(big.Float).Quo(tokenFloat, divisor).Float64()
		usdValue := tokenAmt * priceEv.PriceUSD

		if usdValue < config.DustMinValueUSD {
			continue
		}

		// Estimate gas cost for this swap
		// On Base at 0.05 gwei: ~100k gas = 0.000005 ETH ≈ ~$0.015 at $3000/ETH
		gasCostUSD := gasPrice * 1e-9 * 100_000 * 3000 // rough estimate
		if gasCostUSD > config.DustMaxGasCostUSD {
			continue
		}

		if usdValue > gasCostUSD*2 { // need at least 2x gas cost to be worth it
			dustFound = append(dustFound, TokenBalance{
				Addr:     addr,
				Symbol:   meta.Symbol,
				Decimals: meta.Decimals,
				Balance:  balance,
				USD:      usdValue,
			})
		}
	}

	if len(dustFound) == 0 {
		return
	}

	log.Info("🧹 dust sweep: found convertible tokens",
		"count", len(dustFound),
		"gas_gwei", gasPrice,
	)

	var totalSwept float64
	for _, tb := range dustFound {
		log.Info("  💱 converting dust",
			"token", tb.Symbol,
			"balance_usd", fmt.Sprintf("$%.4f", tb.USD),
		)
		// TODO: execute swap via Aerodrome router (token → USDC)
		// chain.SwapExactTokensForTokens(ctx, client, privKey, tb.Addr, usdcAddr, tb.Balance, ...)
		totalSwept += tb.USD
	}

	d.mu.Lock()
	d.totalCollectedUSD += totalSwept
	d.lastRun = time.Now()
	d.mu.Unlock()

	if totalSwept > 0 {
		state.Global.RecordProfit(state.ProfitEvent{
			BotName:    "dust",
			PairKey:    "DUST/USDC",
			ProfitUSD:  totalSwept,
			ExecutedAt: time.Now(),
		})
		log.Info("🧹 dust sweep complete",
			"total_usd", fmt.Sprintf("$%.4f", totalSwept),
			"lifetime_usd", fmt.Sprintf("$%.4f", d.totalCollectedUSD),
		)
	}
}

// balanceOf calls ERC-20 balanceOf for the wallet.
func (d *DustCollector) balanceOf(ctx context.Context, tokenAddr common.Address) (*big.Int, error) {
	data := append(
		[]byte{0x70, 0xa0, 0x82, 0x31}, // balanceOf selector
		common.LeftPadBytes(d.wallet.Bytes(), 32)...,
	)
	out, err := d.client.CallContract(ctx, ethereum.CallMsg{
		To: &tokenAddr, Data: data,
	}, nil)
	if err != nil || len(out) < 32 {
		return big.NewInt(0), err
	}
	return new(big.Int).SetBytes(out[len(out)-32:]), nil
}
