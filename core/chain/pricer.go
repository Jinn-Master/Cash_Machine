package chain

// core/chain/pricer.go
//
// On-chain price feed — reads sqrtPriceX96 from Uniswap V3 USDC/WETH 0.05% pool
// to get the real-time WETH/USD price. Falls back to cached price on error.

import (
	"context"
	"math/big"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Jinn-Master/Cash_Machine/core/logger"
)

var poolSlot0ABI abi.ABI

func init() {
	var err error
	poolSlot0ABI, err = abi.JSON(strings.NewReader(`[{"inputs":[],"name":"slot0","outputs":[{"internalType":"uint160","name":"sqrtPriceX96","type":"uint160"},{"internalType":"int24","name":"tick","type":"int24"},{"internalType":"uint16","name":"observationIndex","type":"uint16"},{"internalType":"uint16","name":"observationCardinality","type":"uint16"},{"internalType":"uint16","name":"observationCardinalityNext","type":"uint16"},{"internalType":"uint8","name":"feeProtocol","type":"uint8"},{"internalType":"bool","name":"unlocked","type":"bool"}],"stateMutability":"view","type":"function"}]`))
	if err != nil {
		panic("poolSlot0 ABI: " + err.Error())
	}
}

var q96 = new(big.Int).Lsh(big.NewInt(1), 96)
var decimalsAdj = new(big.Float).SetFloat64(1e12)

var (
	cachedPrice   float64
	cachedPriceMu sync.RWMutex
)

// FetchWETHPrice reads the current WETH/USD price from the Uniswap V3
// USDC/WETH 0.05% pool on Base.
func FetchWETHPrice(ctx context.Context, client *ethclient.Client) (float64, error) {
	log := logger.Log

	poolAddr := common.HexToAddress("0xd0b53D9277642d899DF5C87A3966A349A798F224")

	input, _ := poolSlot0ABI.Pack("slot0")
	out, err := client.CallContract(ctx, ethereum.CallMsg{
		To:   &poolAddr,
		Data: input,
	}, nil)
	if err != nil {
		log.Warn("price fetch: call failed, using cached", "err", err)
		return getCachedPrice(), nil
	}

	res, err := poolSlot0ABI.Unpack("slot0", out)
	if err != nil {
		log.Warn("price fetch: unpack failed, using cached", "err", err)
		return getCachedPrice(), nil
	}

	sqrtPriceX96, ok := res[0].(*big.Int)
	if !ok || sqrtPriceX96 == nil || sqrtPriceX96.Sign() == 0 {
		return getCachedPrice(), nil
	}

	priceF := new(big.Float).SetInt(sqrtPriceX96)
	priceF.Quo(priceF, new(big.Float).SetInt(q96))
	priceF.Mul(priceF, priceF)
	priceF.Mul(priceF, decimalsAdj)

	priceUSD, _ := priceF.Float64()
	if priceUSD <= 0 {
		return getCachedPrice(), nil
	}

	setCachedPrice(priceUSD)
	return priceUSD, nil
}

func getCachedPrice() float64 {
	cachedPriceMu.RLock()
	defer cachedPriceMu.RUnlock()
	if cachedPrice == 0 {
		return 3000.0
	}
	return cachedPrice
}

func setCachedPrice(p float64) {
	cachedPriceMu.Lock()
	defer cachedPriceMu.Unlock()
	cachedPrice = p
}
