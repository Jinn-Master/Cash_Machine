package chain

// core/chain/pricier.go
//
// On-chain price feed — reads sqrtPriceX96 from Uniswap V3 USDC/WETH 0.05% pool
// to get the real-time WETH/USD price. Falls back to cached price on error.
//
// Price formula (Uniswap V3):
//   price = (sqrtPriceX96 / 2^96)^2
//   Adjust for decimals: WETH has 18, USDC has 6 → multiply by 10^(18-6) = 10^12
//   Final: priceUSD = price * 1e12

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

// Uniswap V3 slot0 ABI — only the slot0 function we need
const poolSlot0ABIStr = `[{\"inputs\":[],\"name\":\"slot0\",\"outputs\":[{\"internalType\":\"uint160\",\"name\":\"sqrtPriceX96\",\"type\":\"uint160\"},{\"internalType\":\"int24\",\"name\":\"tick\",\"type\":\"int24\"},{\"internalType\":\"uint16\",\"name\":\"observationIndex\",\"type\":\"uint16\"},{\"internalType\":\"uint16\",\"name\":\"observationCardinality\",\"type\":\"uint16\"},{\"internalType\":\"uint16\",\"name\":\"observationCardinalityNext\",\"type\":\"uint16\"},{\"internalType\":\"uint8\",\"name\":\"feeProtocol\",\"type\":\"uint8\"},{\"internalType\":\"bool\",\"name\":\"unlocked\",\"type\":\"bool\"}],\"stateMutability\":\"view\",\"type\":\"function\"}]`

// Q96 is 2^96 — the fixed-point precision used by Uniswap V3
var Q96 = new(big.Int).Lsh(big.NewInt(1), 96)

// Decimals adjustment: WETH (18) - USDC (6) = 12
var decimalsAdj = new(big.Float).SetFloat64(1e12)

// cachedPrice is updated by the price poller in main.go
var cachedPrice float64
var cachedPriceMu sync.RWMutex

// FetchWETHPrice reads the current WETH/USD price from the Uniswap V3
// USDC/WETH 0.05% pool on Base.
func FetchWETHPrice(ctx context.Context, client *ethclient.Client) (float64, error) {
	log := logger.Log

	// Pool address: USDC/WETH 0.05% on Base
	// This is the highest-liquidity WETH/USD pool on Base
	poolAddr := common.HexToAddress("0xd0b53D9277642d899DF5C87A3966A349A798F224")

	abi, err := abiParse(poolSlot0ABIStr)
	if err != nil {
		return 0, err
	}

	input, _ := abi.Pack("slot0")
	out, err := client.CallContract(ctx, ethereum.CallMsg{
		To:   &poolAddr,
		Data: input,
	}, nil)
	if err != nil {
		log.Warn("price fetch: call failed, using cached", "err", err)
		return getCachedPrice(), nil
	}

	res, err := abi.Unpack("slot0", out)
	if err != nil {
		log.Warn("price fetch: unpack failed, using cached", "err", err)
		return getCachedPrice(), nil
	}

	sqrtPriceX96, ok := res[0].(*big.Int)
	if !ok || sqrtPriceX96 == nil || sqrtPriceX96.Sign() == 0 {
		return getCachedPrice(), nil
	}

	// price = (sqrtPriceX96 / Q96)^2 * 10^(18-6)
	priceF := new(big.Float).SetInt(sqrtPriceX96)
	priceF.Quo(priceF, new(big.Float).SetInt(Q96))
	priceF.Mul(priceF, priceF) // square it
	priceF.Mul(priceF, decimalsAdj)

	priceUSD, _ := priceF.Float64()
	if priceUSD <= 0 {
		return getCachedPrice(), nil
	}

	setCachedPrice(priceUSD)
	return priceUSD, nil
}

// getCachedPrice returns the last known good price.
func getCachedPrice() float64 {
	cachedPriceMu.RLock()
	defer cachedPriceMu.RUnlock()
	if cachedPrice == 0 {
		return 3000.0 // last resort fallback
	}
	return cachedPrice
}

// setCachedPrice updates the cached price.
func setCachedPrice(p float64) {
	cachedPriceMu.Lock()
	defer cachedPriceMu.Unlock()
	cachedPrice = p
}

// abiParse is a helper to parse ABI JSON strings.
func abiParse(s string) (*abi.ABI, error) {
	return abi.JSON(strings.NewReader(s))
}
