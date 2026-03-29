package solver

// bots/solver/solver.go
//
// Solver Settling Event Monitor
//
// When CoW Protocol or 1inch solvers settle large orders, they move significant
// liquidity in one transaction — temporarily moving prices on the affected pools.
// In the 1-3 blocks AFTER a large settlement, the affected pools are mispriced
// vs other DEXs that weren't in the settlement path.
//
// Strategy:
//   1. Subscribe to CoW Settlement and 1inch router events
//   2. Detect large trades (> $50k equivalent) in settlement events
//   3. In the NEXT block, check affected token pairs for spread vs our 5 DEXs
//   4. Execute flash loan arb if spread > threshold
//
// This is a "reactive MEV" strategy — we're not frontrunning, we're
// cleaning up the price impact left behind by large settlements.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
	"github.com/Jinn-Master/Cash_Machine/core/state"
)

// CoW Trade event: Trade(address indexed owner, address sellToken, address buyToken,
//   uint256 sellAmount, uint256 buyAmount, uint256 feeAmount, bytes32 orderUid)
const cowTradeABIStr = `[{
	"anonymous": false,
	"inputs": [
		{"indexed": true,  "name": "owner",     "type": "address"},
		{"indexed": false, "name": "sellToken",  "type": "address"},
		{"indexed": false, "name": "buyToken",   "type": "address"},
		{"indexed": false, "name": "sellAmount", "type": "uint256"},
		{"indexed": false, "name": "buyAmount",  "type": "uint256"},
		{"indexed": false, "name": "feeAmount",  "type": "uint256"},
		{"indexed": false, "name": "orderUid",   "type": "bytes32"}
	],
	"name": "Trade",
	"type": "event"
}]`

// 1inch Swapped event (simplified)
const oneInchSwappedABIStr = `[{
	"anonymous": false,
	"inputs": [
		{"indexed": false, "name": "sender",      "type": "address"},
		{"indexed": false, "name": "srcToken",    "type": "address"},
		{"indexed": false, "name": "dstToken",    "type": "address"},
		{"indexed": false, "name": "dstReceiver", "type": "address"},
		{"indexed": false, "name": "spentAmount", "type": "uint256"},
		{"indexed": false, "name": "returnAmount","type": "uint256"}
	],
	"name": "Swapped",
	"type": "event"
}]`

const minLargeTradeUSD = 50_000 // only care about trades > $50k

type SolverBot struct {
	client      *ethclient.Client
	cowABI      abi.ABI
	oneInchABI  abi.ABI
}

func New(client *ethclient.Client) *SolverBot {
	cowABI, _ := abi.JSON(strings.NewReader(cowTradeABIStr))
	oneInchABI, _ := abi.JSON(strings.NewReader(oneInchSwappedABIStr))
	return &SolverBot{
		client:     client,
		cowABI:     cowABI,
		oneInchABI: oneInchABI,
	}
}

func (s *SolverBot) Run(ctx context.Context) {
	log := logger.Log
	log.Info("🤝 solver event monitor started")

	go s.watchCoW(ctx)
	go s.watch1inch(ctx)
}

func (s *SolverBot) watchCoW(ctx context.Context) {
	log := logger.Log
	tradeTopic := s.cowABI.Events["Trade"].ID

	query := ethereum.FilterQuery{
		Addresses: []common.Address{config.CowSettlement},
		Topics:    [][]common.Hash{{tradeTopic}},
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		logsCh := make(chan types.Log, 64)
		sub, err := s.client.SubscribeFilterLogs(ctx, query, logsCh)
		if err != nil {
			log.Warn("CoW subscribe failed", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Info("👁️  watching CoW settlement events")

	drain:
		for {
			select {
			case <-ctx.Done():
				sub.Unsubscribe()
				return
			case err := <-sub.Err():
				log.Warn("CoW sub dropped", "err", err)
				break drain
			case rawLog := <-logsCh:
				go s.handleCoWTrade(ctx, rawLog)
			}
		}
		time.Sleep(5 * time.Second)
	}
}

type cowTradeData struct {
	SellToken  common.Address
	BuyToken   common.Address
	SellAmount interface{}
	BuyAmount  interface{}
	FeeAmount  interface{}
	OrderUid   [32]byte
}

func (s *SolverBot) handleCoWTrade(ctx context.Context, rawLog types.Log) {
	log := logger.Log

	var tradeData cowTradeData
	if err := s.cowABI.UnpackIntoInterface(&tradeData, "Trade", rawLog.Data); err != nil {
		return
	}

	// Estimate USD value using price cache
	sellToken := tradeData.SellToken
	priceEv, hasPriceA := state.Global.GetPrice(sellToken)
	if !hasPriceA || priceEv.PriceUSD == 0 {
		return
	}

	// Rough size estimate — CoW trades usually in 18 decimal tokens
	// A real implementation would check token decimals from the token meta cache
	tradeUSD := priceEv.PriceUSD * 1000 // placeholder — replace with real decode

	if tradeUSD < minLargeTradeUSD {
		return
	}

	log.Info("📦 LARGE CoW settlement detected",
		"sell_token", sellToken.Hex(),
		"buy_token",  tradeData.BuyToken.Hex(),
		"est_usd",    fmt.Sprintf("$%.0f", tradeUSD),
		"block",      rawLog.BlockNumber,
	)

	// Signal the arb bot to check these tokens in the next block
	state.Global.Alert("warn", "solver",
		fmt.Sprintf("Large CoW trade: $%.0f", tradeUSD),
		fmt.Sprintf("Sell: %s | Buy: %s", sellToken.Hex(), tradeData.BuyToken.Hex()),
	)

	// Phase 2: trigger arb evaluation for the affected pair after 1 block (~2s)
	// time.Sleep(2 * time.Second)
	// arbBot.EvaluateImmediately(sellToken, tradeData.BuyToken)
}

func (s *SolverBot) watch1inch(ctx context.Context) {
	log := logger.Log
	swappedTopic := s.oneInchABI.Events["Swapped"].ID

	query := ethereum.FilterQuery{
		Addresses: []common.Address{config.OneInchRouter},
		Topics:    [][]common.Hash{{swappedTopic}},
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		logsCh := make(chan types.Log, 64)
		sub, err := s.client.SubscribeFilterLogs(ctx, query, logsCh)
		if err != nil {
			log.Warn("1inch subscribe failed", "err", err)
			time.Sleep(5 * time.Second)
			continue
		}
		log.Info("👁️  watching 1inch swap events")

	drain:
		for {
			select {
			case <-ctx.Done():
				sub.Unsubscribe()
				return
			case err := <-sub.Err():
				log.Warn("1inch sub dropped", "err", err)
				break drain
			case rawLog := <-logsCh:
				go s.handle1inchSwap(ctx, rawLog)
			}
		}
		time.Sleep(5 * time.Second)
	}
}

func (s *SolverBot) handle1inchSwap(ctx context.Context, rawLog types.Log) {
	// Similar to CoW handler — detect large swaps and signal arb bot
	_ = rawLog
}
