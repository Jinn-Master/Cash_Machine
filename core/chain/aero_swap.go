package chain

// core/chain/aero_swap.go
//
// Aerodrome V2 swap execution helper.
// Provides a simple AerodromeSwap() that any bot can call to execute a token swap.

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
)

// Full Aerodrome router ABI — swap + quote
const aeroRouterFullABI = `[
  {
    "inputs": [
      {"name": "amountIn", "type": "uint256"},
      {"name": "amountOutMin", "type": "uint256"},
      {
        "components": [
          {"name": "from",   "type": "address"},
          {"name": "to",     "type": "address"},
          {"name": "stable", "type": "bool"},
          {"name": "factory","type": "address"}
        ],
        "name": "routes",
        "type": "tuple[]"
      },
      {"name": "to",      "type": "address"},
      {"name": "deadline","type": "uint256"}
    ],
    "name": "swapExactTokensForTokens",
    "outputs": [{"name": "amounts", "type": "uint256[]"}],
    "stateMutability": "nonpayable",
    "type": "function"
  }
]`

var AeroRouterABI abi.ABI

func init() {
	var err error
	AeroRouterABI, err = abi.JSON(strings.NewReader(aeroRouterFullABI))
	if err != nil {
		panic(fmt.Sprintf("AeroRouter ABI: %v", err))
	}
}

// AerodromeSwap executes a token swap on Aerodrome V2.
// Returns the transaction hash or an error.
func AerodromeSwap(
	ctx context.Context,
	client *ethclient.Client,
	privKey *ecdsa.PrivateKey,
	tokenIn, tokenOut common.Address,
	amountIn, minOut *big.Int,
	stable bool,
	deadline *big.Int,
) (common.Hash, error) {
	log := logger.Log
	from := crypto.PubkeyToAddress(privKey.PublicKey)

	factory := config.AeroVolatileFactory
	if stable {
		factory = config.AeroStableFactory
	}

	// Note: tokenIn must be approved for the Aerodrome router before calling this.
	// Use AerodromeApprove() first.

	// Build swap calldata
	type Route struct {
		From    common.Address
		To      common.Address
		Stable  bool
		Factory common.Address
	}
	routes := []Route{{
		From:    tokenIn,
		To:      tokenOut,
		Stable:  stable,
		Factory: factory,
	}}

	swapInput, err := AeroRouterABI.Pack("swapExactTokensForTokens",
		amountIn, minOut, routes, from, deadline)
	if err != nil {
		return common.Hash{}, fmt.Errorf("pack swap: %w", err)
	}

	// Step 3: Estimate gas
	gasLimit, err := client.EstimateGas(ctx, ethereum.CallMsg{
		From: from,
		To:   &config.AeroRouter,
		Data: swapInput,
	})
	if err != nil {
		return common.Hash{}, fmt.Errorf("estimate gas: %w", err)
	}
	gasLimit = gasLimit * 125 / 100 // 25% buffer

	header, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("header: %w", err)
	}

	baseFee := header.BaseFee
	if baseFee == nil {
		baseFee = big.NewInt(0)
	}
	priorityFee := big.NewInt(int64(config.PriorityFeeGwei) * 1e9)
	maxFee := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), priorityFee)

	// Step 4: Build and sign EIP-1559 tx
	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return common.Hash{}, fmt.Errorf("nonce: %w", err)
	}

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(config.BaseChainID),
		Nonce:     nonce,
		GasTipCap: priorityFee,
		GasFeeCap: maxFee,
		Gas:       gasLimit,
		To:        &config.AeroRouter,
		Data:      swapInput,
	})

	signer := types.LatestSignerForChainID(big.NewInt(config.BaseChainID))
	signedTx, err := types.SignTx(tx, signer, privKey)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign: %w", err)
	}

	if err := client.SendTransaction(ctx, signedTx); err != nil {
		return common.Hash{}, fmt.Errorf("send: %w", err)
	}

	log.Info("Aerodrome swap sent",
		"hash", signedTx.Hash().Hex(),
		"tokenIn", tokenIn.Hex(),
		"tokenOut", tokenOut.Hex(),
		"amountIn", amountIn.String(),
	)

	return signedTx.Hash(), nil
}

// AerodromeApprove approves the Aerodrome router to spend a token.
func AerodromeApprove(
	ctx context.Context,
	client *ethclient.Client,
	privKey *ecdsa.PrivateKey,
	tokenAddr common.Address,
	amount *big.Int,
) (common.Hash, error) {
	from := crypto.PubkeyToAddress(privKey.PublicKey)

	// ERC20 approve selector
	sel := []byte{0x09, 0x5e, 0xa7, 0xb3}
	data := append(sel, common.LeftPadBytes(config.AeroRouter.Bytes(), 32)...)
	data = append(data, common.LeftPadBytes(amount.Bytes(), 32)...)

	gasLimit, err := client.EstimateGas(ctx, ethereum.CallMsg{
		From: from,
		To:   &tokenAddr,
		Data: data,
	})
	if err != nil {
		return common.Hash{}, fmt.Errorf("estimate gas: %w", err)
	}
	gasLimit = gasLimit * 125 / 100

	header, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("header: %w", err)
	}

	baseFee := header.BaseFee
	if baseFee == nil {
		baseFee = big.NewInt(0)
	}
	priorityFee := big.NewInt(int64(config.PriorityFeeGwei) * 1e9)
	maxFee := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), priorityFee)

	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return common.Hash{}, fmt.Errorf("nonce: %w", err)
	}

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(config.BaseChainID),
		Nonce:     nonce,
		GasTipCap: priorityFee,
		GasFeeCap: maxFee,
		Gas:       gasLimit,
		To:        &tokenAddr,
		Data:      data,
	})

	signer := types.LatestSignerForChainID(big.NewInt(config.BaseChainID))
	signedTx, err := types.SignTx(tx, signer, privKey)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign: %w", err)
	}

	if err := client.SendTransaction(ctx, signedTx); err != nil {
		return common.Hash{}, fmt.Errorf("send: %w", err)
	}

	logger.Log.Info("Aerodrome approve sent", "hash", signedTx.Hash().Hex(), "token", tokenAddr.Hex())
	return signedTx.Hash(), nil
}
