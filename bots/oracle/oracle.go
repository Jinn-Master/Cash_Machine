package oracle

// bots/oracle/oracle.go
//
// Oracle Lag Bot
//
// Exploits the time delay between real-world price moves and on-chain
// oracle updates (Chainlink / Pyth). When the live DEX price diverges
// significantly from the oracle price, protocols using that oracle
// (lending pools, derivatives) will misprice assets briefly.
//
// How it works:
//   1. Subscribe to Chainlink AnswerUpdated events (WebSocket)
//   2. Simultaneously track DEX spot prices for the same pairs
//   3. When oracle lags > OracleLagMinBPS from spot: evaluate trade
//   4. Primary opportunity: Aave borrows/repays at stale oracle price
//      → borrow at stale (cheap) → swap at real price → profit
//      (This requires a separate flashloan bot integration - flagged for Phase 2)
//   5. Secondary opportunity: simple spot arb using the lag as confirmation signal
//
// Current implementation: detection + alerting
// Phase 2: full execution with Aave integration

import (
	"context"
	"fmt"
	"math/big"
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

const chainlinkABIStr = `[{
	"anonymous": false,
	"inputs": [
		{"indexed": true,  "name": "current",   "type": "int256"},
		{"indexed": true,  "name": "roundId",   "type": "uint256"},
		{"indexed": false, "name": "updatedAt", "type": "uint256"}
	],
	"name": "AnswerUpdated",
	"type": "event"
},{
	"inputs":  [],
	"name":    "latestRoundData",
	"outputs": [
		{"name": "roundId",         "type": "uint80"},
		{"name": "answer",          "type": "int256"},
		{"name": "startedAt",       "type": "uint256"},
		{"name": "updatedAt",       "type": "uint256"},
		{"name": "answeredInRound", "type": "uint80"}
	],
	"stateMutability": "view",
	"type": "function"
}]`

type OracleBot struct {
	client     *ethclient.Client
	chainlinkABI abi.ABI

	// Last known oracle prices: feedAddr → price (8 decimals)
	oraclePrices map[common.Address]*big.Int
	// Last known spot prices: tokenAddr → price (USD, 18 decimals)
	spotPrices   map[common.Address]*big.Int
}

func New(client *ethclient.Client) *OracleBot {
	parsed, _ := abi.JSON(strings.NewReader(chainlinkABIStr))
	return &OracleBot{
		client:       client,
		chainlinkABI: parsed,
		oraclePrices: make(map[common.Address]*big.Int),
		spotPrices:   make(map[common.Address]*big.Int),
	}
}

func (o *OracleBot) Run(ctx context.Context) {
	log := logger.Log
	log.Info("📡 oracle lag bot started")

	// Watch Chainlink AnswerUpdated events
	go o.watchChainlink(ctx, config.ChainlinkETHUSD, "ETH/USD")
	go o.watchChainlink(ctx, config.ChainlinkBTCUSD, "BTC/USD")

	// Track spot prices from DEX updates
	go o.trackSpotPrices(ctx)
}

func (o *OracleBot) watchChainlink(ctx context.Context, feedAddr common.Address, pairName string) {
	log := logger.Log
	answerUpdatedTopic := o.chainlinkABI.Events["AnswerUpdated"].ID

	query := ethereum.FilterQuery{
		Addresses: []common.Address{feedAddr},
		Topics:    [][]common.Hash{{answerUpdatedTopic}},
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		logsCh := make(chan types.Log, 16)
		sub, err := o.client.SubscribeFilterLogs(ctx, query, logsCh)
		if err != nil {
			log.Warn("chainlink subscribe failed", "pair", pairName, "err", err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Info("📡 watching Chainlink feed", "pair", pairName)

	drain:
		for {
			select {
			case <-ctx.Done():
				sub.Unsubscribe()
				return
			case err := <-sub.Err():
				log.Warn("chainlink sub dropped", "pair", pairName, "err", err)
				break drain
			case rawLog := <-logsCh:
				o.handleOracleUpdate(rawLog, feedAddr, pairName)
			}
		}
		time.Sleep(5 * time.Second)
	}
}

func (o *OracleBot) handleOracleUpdate(rawLog types.Log, feedAddr common.Address, pairName string) {
	log := logger.Log

	// AnswerUpdated: current (indexed, int256 in topic[1])
	if len(rawLog.Topics) < 2 {
		return
	}

	oraclePrice := new(big.Int).SetBytes(rawLog.Topics[1].Bytes())
	o.oraclePrices[feedAddr] = oraclePrice

	// Check spot vs oracle lag
	var tokenAddr common.Address
	if pairName == "ETH/USD" {
		tokenAddr = common.HexToAddress(config.AddrWETH)
	}

	if spotEv, ok := state.Global.GetPrice(tokenAddr); ok && spotEv.PriceUSD > 0 {
		// Oracle price has 8 decimals
		oraclePriceFloat := float64(oraclePrice.Int64()) / 1e8
		spotPrice := spotEv.PriceUSD

		lagBPS := int(((spotPrice - oraclePriceFloat) / oraclePriceFloat) * 10000)
		if lagBPS < 0 {
			lagBPS = -lagBPS
		}

		if lagBPS >= config.OracleLagMinBPS {
			log.Info("⚡ ORACLE LAG DETECTED",
				"pair",         pairName,
				"oracle_price", fmt.Sprintf("$%.2f", oraclePriceFloat),
				"spot_price",   fmt.Sprintf("$%.2f", spotPrice),
				"lag_bps",      lagBPS,
			)
			state.Global.Alert("warn", "oracle",
				fmt.Sprintf("Oracle lag: %s — %d bps", pairName, lagBPS),
				fmt.Sprintf("Oracle: $%.2f | Spot: $%.2f", oraclePriceFloat, spotPrice),
			)
			// Phase 2: execute the lag trade here
		}
	}
}

func (o *OracleBot) trackSpotPrices(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case priceEv := <-state.Global.HotPriceCh:
			// Update spot prices from hot price channel
			_ = priceEv
		}
	}
}
