package arb

// backrun.go — Mempool watcher that backruns large swaps on Aerodrome and UniV3
//
// Strategy:
//   1. Subscribe to pending transactions via WebSocket
//   2. Decode each tx — is it a large swap on a watched pool?
//   3. Simulate the post-swap pool state to find arb opportunity
//   4. If profitable, fire our arb tx immediately with higher priority fee
//
// This is fundamentally different from polling:
//   - We react to events that CREATE price dislocations
//   - We land in the same block as the trigger swap
//   - No need to find persistent spreads (they barely exist)

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Jinn-Master/Cash_Machine/core/chain"
	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
	arbmath "github.com/Jinn-Master/Cash_Machine/core/math"
)

// MinSwapUSDC — only backrun swaps larger than this (smaller swaps create negligible price impact)
const MinSwapUSDC = 10_000 // $10K minimum

// WatchedPool — a pool we monitor for large swaps
type WatchedPool struct {
	Address  common.Address
	TokenA   common.Address
	TokenB   common.Address
	SymA     string
	SymB     string
	DexID    uint8
	IsToken0 bool // is TokenA token0 in this pool?
}

// BackrunWatcher watches the mempool for large swaps and backruns them
type BackrunWatcher struct {
	client     *ethclient.Client
	exec       *Executor
	watchedPools map[common.Address]*WatchedPool
	seenTxs    sync.Map // deduplicate pending txs
}

// Known pool addresses to watch for large swaps
// These are the highest-volume pools on Base
var BackrunPools = []WatchedPool{
	// Aerodrome USDC/WETH volatile — $6M liquidity
	{
		Address: common.HexToAddress("0xcDAC0d6c6C59727a65F871236188350531885C43"),
		TokenA:  common.HexToAddress(config.AddrUSDC),
		TokenB:  common.HexToAddress(config.AddrWETH),
		SymA: "USDC", SymB: "WETH",
		DexID: config.DexAerodrome,
	},
	// UniV3 USDC/WETH 0.3% — $40M liquidity (highest volume)
	{
		Address: common.HexToAddress("0x6c561B446416E1A00E8E93E221854d6eA4171372"),
		TokenA:  common.HexToAddress(config.AddrUSDC),
		TokenB:  common.HexToAddress(config.AddrWETH),
		SymA: "USDC", SymB: "WETH",
		DexID: config.DexUniV3,
	},
	// UniV3 USDC/WETH 0.05% — $3M liquidity
	{
		Address: common.HexToAddress("0xd0b53D9277642d899DF5C87A3966A349A798F224"),
		TokenA:  common.HexToAddress(config.AddrUSDC),
		TokenB:  common.HexToAddress(config.AddrWETH),
		SymA: "USDC", SymB: "WETH",
		DexID: config.DexUniV3,
	},
	// Aerodrome USDC/WETH stable
	{
		Address: common.HexToAddress("0x3548029694fbB241D45FB24Ba0cd9c9d4E745f16"),
		TokenA:  common.HexToAddress(config.AddrUSDC),
		TokenB:  common.HexToAddress(config.AddrWETH),
		SymA: "USDC", SymB: "WETH",
		DexID: config.DexAeroStable,
	},
}

// Swap event topic — keccak256("Swap(address,address,int256,int256,uint160,uint128,int24)")
// UniV3 Swap topic
var uniV3SwapTopic = common.HexToHash("0xc42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67")

// Aerodrome/V2 Swap topic — keccak256("Swap(address,uint256,uint256,uint256,uint256,address)")  
var aeroSwapTopic = common.HexToHash("0xd78ad95fa46c994b6551d0da85fc275fe613ce37657fb8d5e3d130840159d822")

func NewBackrunWatcher(client *ethclient.Client, exec *Executor) *BackrunWatcher {
	pools := make(map[common.Address]*WatchedPool)
	for i := range BackrunPools {
		p := &BackrunPools[i]
		pools[p.Address] = p
	}
	return &BackrunWatcher{
		client:       client,
		exec:         exec,
		watchedPools: pools,
	}
}

// Watch subscribes to Swap events on all watched pools and backruns large ones
func (b *BackrunWatcher) Watch(ctx context.Context) {
	log := logger.Log

	// Build filter for all watched pool addresses
	addrs := make([]common.Address, 0, len(b.watchedPools))
	for addr := range b.watchedPools {
		addrs = append(addrs, addr)
	}

	query := ethereum.FilterQuery{
		Addresses: addrs,
		Topics: [][]common.Hash{
			{uniV3SwapTopic, aeroSwapTopic},
		},
	}

	log.Info("🔍 Backrun watcher starting",
		"pools", len(addrs),
		"min_swap_usdc", MinSwapUSDC,
	)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		logsCh := make(chan types.Log, 256)
		sub, err := b.client.SubscribeFilterLogs(ctx, query, logsCh)
		if err != nil {
			log.Warn("backrun subscribe failed, retrying", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Info("✅ Backrun watcher active — watching swap events on all pools")

	drain:
		for {
			select {
			case <-ctx.Done():
				sub.Unsubscribe()
				return
			case err := <-sub.Err():
				log.Warn("backrun subscription dropped", "err", err)
				break drain
			case swapLog := <-logsCh:
				go b.handleSwap(ctx, swapLog)
			}
		}
		time.Sleep(5 * time.Second)
	}
}

// handleSwap processes a swap event and fires a backrun if profitable
func (b *BackrunWatcher) handleSwap(ctx context.Context, swapLog types.Log) {
	log := logger.Log
	pool, ok := b.watchedPools[swapLog.Address]
	if !ok {
		return
	}

	// Deduplicate — same tx can emit multiple swaps
	key := fmt.Sprintf("%s-%d", swapLog.TxHash.Hex(), swapLog.Index)
	if _, loaded := b.seenTxs.LoadOrStore(key, true); loaded {
		return
	}
	// Clean up old entries after 30s
	go func() {
		time.Sleep(30 * time.Second)
		b.seenTxs.Delete(key)
	}()

	// Estimate swap size from log data
	swapSizeUSDC := estimateSwapSize(swapLog, pool)
	if swapSizeUSDC < float64(MinSwapUSDC) {
		return
	}

	log.Info("🎯 Large swap detected — evaluating backrun",
		"pool", pool.SymA+"/"+pool.SymB,
		"dex", config.DexNames[pool.DexID],
		"size_usdc", fmt.Sprintf("$%.0f", swapSizeUSDC),
		"tx", swapLog.TxHash.Hex()[:12],
	)

	// After the swap lands, immediately check all pairs for arb
	// The swap has already happened by the time we get the log,
	// so we evaluate current state for arb opportunities
	pairState := &PairState{
		TokenPair: config.TokenPair{
			SymA:     pool.SymA,
			AddrA:    pool.TokenA,
			DecA:     6, // USDC
			SymB:     pool.SymB,
			AddrB:    pool.TokenB,
			DecB:     18,
			UniV3Fee: 3000,
		},
		TradeSize: new(big.Int).SetUint64(config.TradeSizes["USDC"]),
	}

	// Resolve pool addresses for quoting
	pairState.Addrs.AeroPair = common.HexToAddress("0xcDAC0d6c6C59727a65F871236188350531885C43")
	pairState.Addrs.AeroStablePair = common.HexToAddress("0x3548029694fbB241D45FB24Ba0cd9c9d4E745f16")

	// Use higher priority fee for backrun — we want to land right after the swap
	b.exec.EvaluatePairBackrun(ctx, pairState)
}

// estimateSwapSize estimates the USD size of a swap from the log data
func estimateSwapSize(swapLog types.Log, pool *WatchedPool) float64 {
	if len(swapLog.Data) < 32 {
		return 0
	}

	// For V2-style swaps (Aerodrome): data has amount0In, amount1In, amount0Out, amount1Out
	// For V3-style swaps: data has amount0, amount1, sqrtPriceX96, liquidity, tick
	// We look at the first 32 bytes as the primary amount
	amount := new(big.Int).SetBytes(swapLog.Data[:32])

	// If pool involves USDC (6 decimals), convert to USD
	if strings.EqualFold(pool.SymA, "USDC") || strings.EqualFold(pool.SymB, "USDC") {
		// USDC amount in 6 decimals
		usdcF, _ := new(big.Float).Quo(
			new(big.Float).SetInt(amount),
			big.NewFloat(1e6),
		).Float64()
		if usdcF > 1e9 { // sanity check — V3 amounts can be negative (int256)
			return 0
		}
		return usdcF
	}

	// WETH — rough estimate at $3000/ETH
	ethF, _ := new(big.Float).Quo(
		new(big.Float).SetInt(amount),
		big.NewFloat(1e18),
	).Float64()
	return ethF * 3000
}

// EvaluatePairBackrun is like EvaluatePair but with boosted priority fee
func (e *Executor) EvaluatePairBackrun(ctx context.Context, p *PairState) {
	log := logger.Log

	var (
		quotes arbmath.DexQuotes
		mu     sync.Mutex
		wg     sync.WaitGroup
	)

	fetch := func(dexId uint8, fn func() (*big.Int, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := fn()
			if err != nil {
				log.Debug("backrun quote failed", "dex", config.DexNames[dexId], "err", err)
				return
			}
			if out != nil && out.Sign() > 0 {
				mu.Lock()
				quotes[dexId] = out
				mu.Unlock()
			}
		}()
	}

	fetch(config.DexUniV3, func() (*big.Int, error) {
		return chain.UniV3Quote(ctx, e.Client, config.UniV3Quoter,
			p.AddrA, p.AddrB, p.UniV3Fee, p.TradeSize)
	})
	fetch(config.DexAerodrome, func() (*big.Int, error) {
		if p.Addrs.AeroPair == (common.Address{}) {
			return nil, fmt.Errorf("no aero pool")
		}
		return chain.AerodromePoolQuote(ctx, e.Client, p.Addrs.AeroPair, p.AddrA, p.TradeSize)
	})
	fetch(config.DexAeroStable, func() (*big.Int, error) {
		if p.Addrs.AeroStablePair == (common.Address{}) {
			return nil, fmt.Errorf("no aero stable pool")
		}
		return chain.AerodromePoolQuote(ctx, e.Client, p.Addrs.AeroStablePair, p.AddrA, p.TradeSize)
	})

	wg.Wait()

	buyDex, sellDex, spreadPct := arbmath.BestArb(quotes)
	if buyDex < 0 || spreadPct < config.MinProfitPct {
		return
	}

	log.Info("⚡ BACKRUN OPPORTUNITY",
		"pair", p.Key(),
		"buy", config.DexNames[buyDex],
		"sell", config.DexNames[sellDex],
		"spread", fmt.Sprintf("%.3f%%", spreadPct),
	)

	// Execute with boosted priority fee (2x normal) to land right after the swap
	e.ExecuteTradeBackrun(ctx, p, uint8(buyDex), uint8(sellDex), quotes[buyDex], quotes[sellDex])
}

// ExecuteTradeBackrun executes with boosted priority fee for backrun positioning
func (e *Executor) ExecuteTradeBackrun(ctx context.Context, p *PairState, buyDex, sellDex uint8, buyOut, sellOut *big.Int) {
	log := logger.Log

	e.tradeLock.Lock()
	defer e.tradeLock.Unlock()

	minAB := big.NewInt(1)
	// minBA := big.NewInt(1) // safety off for backrun testing

	header, err := e.Client.HeaderByNumber(ctx, nil)
	if err != nil {
		log.Error("backrun: header fetch failed", "err", err)
		return
	}
	deadline := new(big.Int).SetUint64(header.Time + config.DeadlineBuffer)
	baseFee := header.BaseFee
	if baseFee == nil {
		baseFee = big.NewInt(0)
	}

	// 2x priority fee to land right after the swap we're backrunning
	priorityFee := new(big.Int).Mul(big.NewInt(config.PriorityFeeGwei*2), big.NewInt(1e9))

	// Simulate first
	err = chain.SimulateFlashArbitrage(ctx, e.Client,
		e.WalletAddr, e.ContractAddr,
		p.AddrA, p.AddrB,
		p.TradeSize, p.UniV3Fee,
		buyDex, sellDex,
		minAB, big.NewInt(1), deadline,
	)
	if err != nil {
		log.Info("backrun simulation failed", "pair", p.Key(), "err", err)
		return
	}

	log.Info("✅ Backrun simulation passed — firing tx", "pair", p.Key())

	txHash, err := chain.BuildAndSendFlashArbitrage(ctx, e.Client, chain.TxParams{
		Contract:    e.ContractAddr,
		TokenA:      p.AddrA,
		TokenB:      p.AddrB,
		LoanAmount:  p.TradeSize,
		PoolFee:     p.UniV3Fee,
		BuyDex:      buyDex,
		SellDex:     sellDex,
		MinAB:       minAB,
		MinBA:       big.NewInt(1),
		Deadline:    deadline,
		PrivKey:     e.PrivKey,
		BaseFee:     baseFee,
		PriorityFee: priorityFee,
		ChainID:     e.ChainID,
	})
	if err != nil {
		log.Error("backrun tx failed", "pair", p.Key(), "err", err)
		return
	}
	log.Info("🚀 BACKRUN TX SENT",
		"pair", p.Key(),
		"hash", txHash.Hex(),
		"buy", config.DexNames[buyDex],
		"sell", config.DexNames[sellDex],
	)
}

// isHexString checks if a string looks like hex data
func isHexString(s string) bool {
	_, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	return err == nil
}
