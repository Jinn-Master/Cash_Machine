package arb

// bots/arb/factory_watcher.go
//
// Watches PairCreated events from all DEX factory contracts in real time.

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

	"github.com/Jinn-Master/Cash_Machine/core/chain"
	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
	arbmath "github.com/Jinn-Master/Cash_Machine/core/math"
	"github.com/Jinn-Master/Cash_Machine/core/state"
)

// ── PairCreated event ABIs ────────────────────────────────────────────────────

const v2PairCreatedABIJSON = `[{
	"anonymous": false,
	"inputs": [
		{"indexed": true,  "name": "token0", "type": "address"},
		{"indexed": true,  "name": "token1", "type": "address"},
		{"indexed": false, "name": "pair",   "type": "address"},
		{"indexed": false, "name": "",       "type": "uint256"}
	],
	"name": "PairCreated",
	"type": "event"
}]`

const v3PoolCreatedABIJSON = `[{
	"anonymous": false,
	"inputs": [
		{"indexed": true,  "name": "token0",     "type": "address"},
		{"indexed": true,  "name": "token1",     "type": "address"},
		{"indexed": true,  "name": "fee",        "type": "uint24"},
		{"indexed": false, "name": "tickSpacing","type": "int24"},
		{"indexed": false, "name": "pool",       "type": "address"}
	],
	"name": "PoolCreated",
	"type": "event"
}]`

// ── FactoryKind ───────────────────────────────────────────────────────────────

type FactoryKind uint8

const (
	FactoryV2 FactoryKind = iota
	FactoryV3
)

// ── FactoryDef ────────────────────────────────────────────────────────────────

type FactoryDef struct {
	Name  string
	Addr  common.Address
	Kind  FactoryKind
	DexID uint8
}

var watchedFactories = []FactoryDef{
	{Name: "Uniswap V3", Addr: config.UniV3Factory, Kind: FactoryV3, DexID: config.DexUniV3},
	{Name: "Aerodrome", Addr: config.AeroFactory, Kind: FactoryV2, DexID: config.DexAerodrome},
	{Name: "BaseSwap", Addr: config.BaseswapFactory, Kind: FactoryV2, DexID: config.DexBaseSwap},
	{Name: "SwapBased", Addr: config.SwapBasedFactory, Kind: FactoryV2, DexID: config.DexSwapBased},
	{Name: "MaverickV2", Addr: config.MaverickV2Factory, Kind: FactoryV2, DexID: config.DexMaverick},
}

// ── NewPairEvent ──────────────────────────────────────────────────────────────

type NewPairEvent struct {
	Token0   common.Address
	Token1   common.Address
	PairAddr common.Address
	Fee      uint32
	Factory  FactoryDef
}

// ── FactoryWatcher ────────────────────────────────────────────────────────────

type FactoryWatcher struct {
	client   *ethclient.Client
	exec     *Executor
	hotPairs chan<- *PairState
	seen     map[string]time.Time
	seenMu   sync.Mutex
	v2ABI    abi.ABI
	v3ABI    abi.ABI
}

func NewFactoryWatcher(
	client *ethclient.Client,
	exec *Executor,
	hotPairs chan<- *PairState,
) (*FactoryWatcher, error) {
	v2ABI, err := abi.JSON(strings.NewReader(v2PairCreatedABIJSON))
	if err != nil {
		return nil, fmt.Errorf("parse V2 ABI: %w", err)
	}
	v3ABI, err := abi.JSON(strings.NewReader(v3PoolCreatedABIJSON))
	if err != nil {
		return nil, fmt.Errorf("parse V3 ABI: %w", err)
	}
	return &FactoryWatcher{
		client:   client,
		exec:     exec,
		hotPairs: hotPairs,
		seen:     make(map[string]time.Time),
		v2ABI:    v2ABI,
		v3ABI:    v3ABI,
	}, nil
}

func (fw *FactoryWatcher) Watch(ctx context.Context) {
	log := logger.Log
	log.Info("🏭 factory watcher starting", "factories", len(watchedFactories))
	for _, fDef := range watchedFactories {
		fDef := fDef
		go fw.watchFactory(ctx, fDef)
	}
}

func (fw *FactoryWatcher) watchFactory(ctx context.Context, fDef FactoryDef) {
	log := logger.Log

	var eventTopic common.Hash
	if fDef.Kind == FactoryV3 {
		eventTopic = fw.v3ABI.Events["PoolCreated"].ID
	} else {
		eventTopic = fw.v2ABI.Events["PairCreated"].ID
	}

	query := ethereum.FilterQuery{
		Addresses: []common.Address{fDef.Addr},
		Topics:    [][]common.Hash{{eventTopic}},
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Info("📡 subscribing to factory", "name", fDef.Name)
		logsCh := make(chan types.Log, 32)
		sub, err := fw.client.SubscribeFilterLogs(ctx, query, logsCh)
		if err != nil {
			log.Warn("factory subscribe failed, retrying", "factory", fDef.Name, "err", err)
			time.Sleep(time.Duration(config.ReconnectDelaySec) * time.Second)
			continue
		}
		log.Info("✅ factory subscription active", "factory", fDef.Name)

	drain:
		for {
			select {
			case <-ctx.Done():
				sub.Unsubscribe()
				return
			case err := <-sub.Err():
				log.Warn("factory sub dropped", "factory", fDef.Name, "err", err)
				break drain
			case rawLog := <-logsCh:
				event, err := fw.decodeLog(rawLog, fDef)
				if err != nil {
					log.Debug("decode failed", "factory", fDef.Name, "err", err)
					continue
				}
				go fw.handleNewPair(ctx, event)
			}
		}
		time.Sleep(time.Duration(config.ReconnectDelaySec) * time.Second)
	}
}

func (fw *FactoryWatcher) decodeLog(rawLog types.Log, fDef FactoryDef) (*NewPairEvent, error) {
	ev := &NewPairEvent{Factory: fDef}

	if fDef.Kind == FactoryV3 {
		if len(rawLog.Topics) < 4 {
			return nil, fmt.Errorf("V3 log: not enough topics")
		}
		ev.Token0 = common.HexToAddress(rawLog.Topics[1].Hex())
		ev.Token1 = common.HexToAddress(rawLog.Topics[2].Hex())
		feeBytes := rawLog.Topics[3].Bytes()
		ev.Fee = uint32(feeBytes[29])<<16 | uint32(feeBytes[30])<<8 | uint32(feeBytes[31])

		type v3Data struct {
			TickSpacing *big.Int
			Pool        common.Address
		}
		var d v3Data
		if err := fw.v3ABI.UnpackIntoInterface(&d, "PoolCreated", rawLog.Data); err != nil {
			return nil, fmt.Errorf("V3 unpack: %w", err)
		}
		ev.PairAddr = d.Pool
	} else {
		if len(rawLog.Topics) < 3 {
			return nil, fmt.Errorf("V2 log: not enough topics")
		}
		ev.Token0 = common.HexToAddress(rawLog.Topics[1].Hex())
		ev.Token1 = common.HexToAddress(rawLog.Topics[2].Hex())
		if len(rawLog.Data) < 32 {
			return nil, fmt.Errorf("V2 data too short")
		}
		ev.PairAddr = common.BytesToAddress(rawLog.Data[12:32])
	}

	return ev, nil
}

func (fw *FactoryWatcher) handleNewPair(ctx context.Context, ev *NewPairEvent) {
	log := logger.Log

	usdcAddr := common.HexToAddress(config.AddrUSDC)
	wethAddr := common.HexToAddress(config.AddrWETH)

	var baseAddr, nicheAddr common.Address
	var baseSym string

	switch {
	case ev.Token0 == usdcAddr:
		baseAddr, nicheAddr, baseSym = usdcAddr, ev.Token1, "USDC"
	case ev.Token1 == usdcAddr:
		baseAddr, nicheAddr, baseSym = usdcAddr, ev.Token0, "USDC"
	case ev.Token0 == wethAddr:
		baseAddr, nicheAddr, baseSym = wethAddr, ev.Token1, "WETH"
	case ev.Token1 == wethAddr:
		baseAddr, nicheAddr, baseSym = wethAddr, ev.Token0, "WETH"
	default:
		return // not a USDC or WETH pair
	}

	nicheKey := strings.ToLower(nicheAddr.Hex())

	// Dedup within 10-minute window
	fw.seenMu.Lock()
	if lastSeen, ok := fw.seen[nicheKey]; ok && time.Since(lastSeen) < 10*time.Minute {
		fw.seenMu.Unlock()
		return
	}
	fw.seen[nicheKey] = time.Now()
	fw.seenMu.Unlock()

	log.Info("🆕 new pair detected",
		"factory", ev.Factory.Name,
		"base", baseSym,
		"niche", nicheAddr.Hex(),
		"pair", ev.PairAddr.Hex(),
	)

	// Wait ~3 blocks for liquidity to settle
	select {
	case <-ctx.Done():
		return
	case <-time.After(6 * time.Second):
	}

	// Check all factories for this pair
	dexPresence := fw.checkAllFactories(ctx, baseAddr, nicheAddr, ev.Factory)
	dexCount := 1
	for _, present := range dexPresence {
		if present {
			dexCount++
		}
	}

	log.Info("🔍 cross-factory check", "token", nicheAddr.Hex(), "dex_count", dexCount)

	// Wait up to 60s for 2nd DEX
	if dexCount < 2 {
		log.Info("⏳ only 1 DEX — waiting for second listing", "token", nicheAddr.Hex())
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			dexPresence = fw.checkAllFactories(ctx, baseAddr, nicheAddr, ev.Factory)
			dexCount = 1
			for _, present := range dexPresence {
				if present {
					dexCount++
				}
			}
			if dexCount >= 2 {
				log.Info("✅ second DEX found!", "token", nicheAddr.Hex())
				break
			}
		}
		if dexCount < 2 {
			log.Info("⏭️  skipping — only 1 DEX after 60s", "token", nicheAddr.Hex())
			return
		}
	}

	// Validate liquidity with a probe quote
	var probeAmt *big.Int
	if baseSym == "USDC" {
		probeAmt = big.NewInt(1_000_000)
	} else {
		probeAmt = big.NewInt(1_000_000_000_000_000)
	}

	fee := ev.Fee
	if fee == 0 {
		fee = 3000
	}

	quoteOut, err := chain.UniV3Quote(ctx, fw.client, config.UniV3Quoter,
		baseAddr, nicheAddr, fee, probeAmt)
	if err != nil || quoteOut == nil || quoteOut.Sign() == 0 {
		quoteOut, err = chain.AerodromeQuote(ctx, fw.client, config.AeroQuoter,
			baseAddr, nicheAddr, probeAmt, false)
		if err != nil || quoteOut == nil || quoteOut.Sign() == 0 {
			log.Info("⏭️  skipping — no valid quote", "token", nicheAddr.Hex())
			return
		}
	}

	// Fetch token metadata
	nicheSym, nicheDecimals := fw.fetchTokenMeta(ctx, nicheAddr)

	var decA int
	if baseSym == "USDC" {
		decA = 6
	} else {
		decA = 18
	}

	var tradeSize uint64
	if baseSym == "USDC" {
		tradeSize = 50_000_000
	} else {
		tradeSize = 10_000_000_000_000_000
	}

	tp := config.TokenPair{
		SymA:      baseSym,
		AddrA:     baseAddr,
		DecA:      decA,
		SymB:      nicheSym,
		AddrB:     nicheAddr,
		DecB:      nicheDecimals,
		UniV3Fee:  fee,
		TradeSize: tradeSize,
	}

	ps := &PairState{
		TokenPair: tp,
		TradeSize: new(big.Int).SetUint64(tradeSize),
	}

	// Resolve V2 pair addresses
	if bsPair, err := chain.GetV2Pair(ctx, fw.client,
		config.BaseswapFactory, baseAddr, nicheAddr); err == nil && bsPair != (common.Address{}) {
		t0, err := chain.GetToken0(ctx, fw.client, bsPair)
		if err == nil {
			ps.Addrs.BaseswapPair = bsPair
			ps.Addrs.AIsToken0 = (t0 == baseAddr)
		}
	}

	if sbPair, err := chain.GetV2Pair(ctx, fw.client,
		config.SwapBasedFactory, baseAddr, nicheAddr); err == nil && sbPair != (common.Address{}) {
		t0, err := chain.GetToken0(ctx, fw.client, sbPair)
		if err == nil {
			ps.Addrs.SwapBasedPair = sbPair
			ps.Addrs.SBIsToken0 = (t0 == baseAddr)
		}
	}

	// Resolve Maverick V2 pool
	if mavPool, tokenAIn, err := chain.MaverickV2FindPool(ctx, fw.client,
		baseAddr, nicheAddr); err == nil && mavPool != (common.Address{}) {
		ps.Addrs.MaverickPool = mavPool
		ps.Addrs.MavTokenAIn = tokenAIn
	}

	log.Info("🚀 new pair validated — hot-loading into bot",
		"pair", ps.Key(),
		"dex_count", dexCount,
		"trade_size", arbmath.FmtAmount(ps.TradeSize, decA, baseSym),
	)

	// Broadcast to state bus so scalper + other bots hear about it
	state.Global.AddHotToken(state.NewTokenEvent{
		TokenAddr:  nicheAddr,
		TokenSym:   nicheSym,
		TokenDec:   nicheDecimals,
		BaseToken:  baseSym,
		BaseAddr:   baseAddr,
		PairAddr:   ev.PairAddr,
		Factory:    ev.Factory.Name,
		DetectedAt: time.Now(),
		DexCount:   dexCount,
	})

	// Send into hot-reload channel for arb bot
	select {
	case fw.hotPairs <- ps:
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		log.Warn("hotPairs channel full — dropping pair", "pair", ps.Key())
	}
}

func (fw *FactoryWatcher) checkAllFactories(
	ctx context.Context,
	baseAddr, nicheAddr common.Address,
	emitter FactoryDef,
) map[uint8]bool {
	result := make(map[uint8]bool)
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, fDef := range watchedFactories {
		if fDef.Addr == emitter.Addr {
			continue
		}
		fDef := fDef
		wg.Add(1)
		go func() {
			defer wg.Done()
			var found bool
			if fDef.Kind == FactoryV3 {
				probe := big.NewInt(1_000_000)
				out, err := chain.UniV3Quote(ctx, fw.client, config.UniV3Quoter,
					baseAddr, nicheAddr, 3000, probe)
				found = err == nil && out != nil && out.Sign() > 0
				if !found {
					for _, fee := range []uint32{500, 10000} {
						out, err = chain.UniV3Quote(ctx, fw.client, config.UniV3Quoter,
							baseAddr, nicheAddr, fee, probe)
						if err == nil && out != nil && out.Sign() > 0 {
							found = true
							break
						}
					}
				}
			} else {
				pairAddr, err := chain.GetV2Pair(ctx, fw.client, fDef.Addr, baseAddr, nicheAddr)
				found = err == nil && pairAddr != (common.Address{})
			}
			if found {
				mu.Lock()
				result[fDef.DexID] = true
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	return result
}

func (fw *FactoryWatcher) fetchTokenMeta(ctx context.Context, addr common.Address) (symbol string, decimals int) {
	symbol = "UNKNOWN"
	decimals = 18
	sym, dec, err := chain.GetTokenMeta(ctx, fw.client, addr)
	if err == nil {
		if sym != "" {
			symbol = sym
		}
		if dec > 0 {
			decimals = dec
		}
	}
	return
}
