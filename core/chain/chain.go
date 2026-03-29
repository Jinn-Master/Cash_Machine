package chain

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ── ABI strings ──────────────────────────────────────────────────────────────

// V2 factory: getPair(tokenA, tokenB) — used by BaseSwap
const v2FactoryABIStr = `[{"inputs":[{"internalType":"address","name":"tokenA","type":"address"},{"internalType":"address","name":"tokenB","type":"address"}],"name":"getPair","outputs":[{"internalType":"address","name":"pair","type":"address"}],"stateMutability":"view","type":"function"}]`

// V2 pair: getReserves + token0
const v2PairABIStr = `[{"constant":true,"inputs":[],"name":"getReserves","outputs":[{"name":"_reserve0","type":"uint112"},{"name":"_reserve1","type":"uint112"},{"name":"_blockTimestampLast","type":"uint32"}],"type":"function"},{"constant":true,"inputs":[],"name":"token0","outputs":[{"name":"","type":"address"}],"type":"function"}]`

// Uniswap V3 QuoterV2 on Base
const uniV3QuoterABIStr = `[{"inputs":[{"components":[{"internalType":"address","name":"tokenIn","type":"address"},{"internalType":"address","name":"tokenOut","type":"address"},{"internalType":"uint256","name":"amountIn","type":"uint256"},{"internalType":"uint24","name":"fee","type":"uint24"},{"internalType":"uint160","name":"sqrtPriceLimitX96","type":"uint160"}],"internalType":"struct IQuoterV2.QuoteExactInputSingleParams","name":"params","type":"tuple"}],"name":"quoteExactInputSingle","outputs":[{"internalType":"uint256","name":"amountOut","type":"uint256"},{"internalType":"uint160","name":"sqrtPriceX96After","type":"uint160"},{"internalType":"uint32","name":"initializedTicksCrossed","type":"uint32"},{"internalType":"uint256","name":"gasEstimate","type":"uint256"}],"stateMutability":"nonpayable","type":"function"}]`

// Aerodrome V2 quoter: quoteExactInputSingle for volatile pools
const aeroQuoterABIStr = `[{"inputs":[{"internalType":"address","name":"tokenIn","type":"address"},{"internalType":"address","name":"tokenOut","type":"address"},{"internalType":"uint256","name":"amountIn","type":"uint256"},{"internalType":"bool","name":"stable","type":"bool"}],"name":"quoteExactInputSingle","outputs":[{"internalType":"uint256","name":"amountOut","type":"uint256"},{"internalType":"bool","name":"stable","type":"bool"},{"internalType":"address","name":"pool","type":"address"},{"internalType":"uint256","name":"routerAmountOut","type":"uint256"}],"stateMutability":"view","type":"function"}]`

// BaseSwap uses standard V2 getReserves via pair address — same ABI as v2Pair

// ArbitrageExecutor.flashArbitrage — updated for Base/3-DEX contract
const arbABIStr = `[{"inputs":[{"internalType":"address","name":"tokenA","type":"address"},{"internalType":"address","name":"tokenB","type":"address"},{"internalType":"uint256","name":"loanAmount","type":"uint256"},{"internalType":"uint24","name":"poolFee","type":"uint24"},{"internalType":"uint8","name":"buyDex","type":"uint8"},{"internalType":"uint8","name":"sellDex","type":"uint8"},{"internalType":"uint256","name":"minAB","type":"uint256"},{"internalType":"uint256","name":"minBA","type":"uint256"},{"internalType":"uint256","name":"deadline","type":"uint256"}],"name":"flashArbitrage","outputs":[],"stateMutability":"nonpayable","type":"function"}]`

// Sync event topic: keccak256("Sync(uint112,uint112)") — same on V2-compatible DEXs
var SyncTopic = common.HexToHash("0x1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1")

// ── Parsed ABIs ───────────────────────────────────────────────────────────────

var (
	V2FactoryABI   abi.ABI
	V2PairABI      abi.ABI
	UniV3QuoterABI abi.ABI
	AeroQuoterABI  abi.ABI
	ArbABI         abi.ABI
)

func init() {
	must := func(s string) abi.ABI {
		a, err := abi.JSON(strings.NewReader(s))
		if err != nil {
			panic(fmt.Sprintf("ABI parse: %v", err))
		}
		return a
	}
	V2FactoryABI   = must(v2FactoryABIStr)
	V2PairABI      = must(v2PairABIStr)
	UniV3QuoterABI = must(uniV3QuoterABIStr)
	AeroQuoterABI  = must(aeroQuoterABIStr)
	ArbABI         = must(arbABIStr)
}

// ── V2 helpers ────────────────────────────────────────────────────────────────

func GetV2Pair(ctx context.Context, client *ethclient.Client, factory, tokenA, tokenB common.Address) (common.Address, error) {
	input, err := V2FactoryABI.Pack("getPair", tokenA, tokenB)
	if err != nil {
		return common.Address{}, err
	}
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &factory, Data: input}, nil)
	if err != nil {
		return common.Address{}, err
	}
	res, err := V2FactoryABI.Unpack("getPair", out)
	if err != nil {
		return common.Address{}, err
	}
	return res[0].(common.Address), nil
}

func GetToken0(ctx context.Context, client *ethclient.Client, pair common.Address) (common.Address, error) {
	input, _ := V2PairABI.Pack("token0")
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &pair, Data: input}, nil)
	if err != nil {
		return common.Address{}, err
	}
	res, err := V2PairABI.Unpack("token0", out)
	if err != nil {
		return common.Address{}, err
	}
	return res[0].(common.Address), nil
}

type Reserves struct{ Reserve0, Reserve1 *big.Int }

func GetReserves(ctx context.Context, client *ethclient.Client, pair common.Address) (Reserves, error) {
	input, _ := V2PairABI.Pack("getReserves")
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &pair, Data: input}, nil)
	if err != nil {
		return Reserves{}, err
	}
	res, err := V2PairABI.Unpack("getReserves", out)
	if err != nil {
		return Reserves{}, err
	}
	return Reserves{Reserve0: res[0].(*big.Int), Reserve1: res[1].(*big.Int)}, nil
}

// ── Uniswap V3 QuoterV2 (quoteExactInputSingle with struct params) ─────────

func UniV3Quote(ctx context.Context, client *ethclient.Client, quoter, tokenIn, tokenOut common.Address, fee uint32, amountIn *big.Int) (*big.Int, error) {
	type QuoteParams struct {
		TokenIn           common.Address
		TokenOut          common.Address
		AmountIn          *big.Int
		Fee               uint32
		SqrtPriceLimitX96 *big.Int
	}
	params := QuoteParams{
		TokenIn:           tokenIn,
		TokenOut:          tokenOut,
		AmountIn:          amountIn,
		Fee:               fee,
		SqrtPriceLimitX96: big.NewInt(0),
	}
	input, err := UniV3QuoterABI.Pack("quoteExactInputSingle", params)
	if err != nil {
		return nil, err
	}
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &quoter, Data: input}, nil)
	if err != nil {
		return nil, err
	}
	res, err := UniV3QuoterABI.Unpack("quoteExactInputSingle", out)
	if err != nil {
		return nil, err
	}
	return res[0].(*big.Int), nil
}

// ── Aerodrome V2 quoter (volatile or stable pool) ────────────────────────────

func AerodromeQuote(ctx context.Context, client *ethclient.Client, quoter, tokenIn, tokenOut common.Address, amountIn *big.Int, stable bool) (*big.Int, error) {
	input, err := AeroQuoterABI.Pack("quoteExactInputSingle", tokenIn, tokenOut, amountIn, stable)
	if err != nil {
		return nil, err
	}
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &quoter, Data: input}, nil)
	if err != nil {
		return nil, err
	}
	res, err := AeroQuoterABI.Unpack("quoteExactInputSingle", out)
	if err != nil {
		return nil, err
	}
	return res[0].(*big.Int), nil
}

// ── BaseSwap quote (V2 constant-product, computed from reserves) ─────────────
// BaseSwap is V2-compatible so we read reserves and compute off-chain.
// Call GetReserves(baseswapPairAddr) then use math.QuickswapQuote (same formula).

// ── Simulate flashArbitrage ───────────────────────────────────────────────────

func SimulateFlashArbitrage(
	ctx context.Context, client *ethclient.Client,
	from, contract, tokenA, tokenB common.Address,
	loanAmount *big.Int, poolFee uint32,
	buyDex, sellDex uint8,
	minAB, minBA, deadline *big.Int,
) error {
	input, err := ArbABI.Pack("flashArbitrage",
		tokenA, tokenB, loanAmount, poolFee, buyDex, sellDex, minAB, minBA, deadline,
	)
	if err != nil {
		return err
	}
	_, err = client.CallContract(ctx, ethereum.CallMsg{From: from, To: &contract, Data: input}, nil)
	return err
}

// ── Build and send EIP-1559 transaction ───────────────────────────────────────

type TxParams struct {
	Contract    common.Address
	TokenA      common.Address
	TokenB      common.Address
	LoanAmount  *big.Int
	PoolFee     uint32
	BuyDex      uint8
	SellDex     uint8
	MinAB       *big.Int
	MinBA       *big.Int
	Deadline    *big.Int
	PrivKey     *ecdsa.PrivateKey
	BaseFee     *big.Int
	PriorityFee *big.Int
	ChainID     *big.Int
}

func BuildAndSendFlashArbitrage(ctx context.Context, client *ethclient.Client, p TxParams) (common.Hash, error) {
	from := crypto.PubkeyToAddress(p.PrivKey.PublicKey)

	input, err := ArbABI.Pack("flashArbitrage",
		p.TokenA, p.TokenB, p.LoanAmount, p.PoolFee,
		p.BuyDex, p.SellDex, p.MinAB, p.MinBA, p.Deadline,
	)
	if err != nil {
		return common.Hash{}, err
	}

	gasEst, err := client.EstimateGas(ctx, ethereum.CallMsg{From: from, To: &p.Contract, Data: input})
	if err != nil {
		return common.Hash{}, fmt.Errorf("gas estimate: %w", err)
	}
	gasLimit := uint64(float64(gasEst) * 1.25)

	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		return common.Hash{}, err
	}

	// Base L2: maxFee = baseFee*2 + tip. BaseFee on Base is tiny (~0.001 gwei typical).
	maxFee := new(big.Int).Add(
		new(big.Int).Mul(p.BaseFee, big.NewInt(2)),
		p.PriorityFee,
	)

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   p.ChainID,
		Nonce:     nonce,
		GasTipCap: p.PriorityFee,
		GasFeeCap: maxFee,
		Gas:       gasLimit,
		To:        &p.Contract,
		Data:      input,
	})

	signer := types.LatestSignerForChainID(p.ChainID)
	signed, err := types.SignTx(tx, signer, p.PrivKey)
	if err != nil {
		return common.Hash{}, err
	}

	return signed.Hash(), client.SendTransaction(ctx, signed)
}

func WaitForReceipt(ctx context.Context, client *ethclient.Client, hash common.Hash) (*types.Receipt, error) {
	for {
		receipt, err := client.TransactionReceipt(ctx, hash)
		if err == nil {
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1 * time.Second): // Base produces blocks every ~2s
		}
	}
}

// DexNames maps dexId (0–4) to a human-readable label used in logs.
// Must stay in sync with the DEX ID constants in config/config.go.
var DexNames = [5]string{
	"UniV3",        // 0
	"AeroVolatile", // 1
	"BaseSwap",     // 2
	"AeroStable",   // 3
	"SwapBased",    // 4
}

// ── ERC-20 metadata ───────────────────────────────────────────────────────────

const erc20ABIStr = `[
	{"constant":true,"inputs":[],"name":"symbol","outputs":[{"name":"","type":"string"}],"type":"function"},
	{"constant":true,"inputs":[],"name":"decimals","outputs":[{"name":"","type":"uint8"}],"type":"function"}
]`

var erc20ABI abi.ABI

func init() {
	var err error
	erc20ABI, err = abi.JSON(strings.NewReader(erc20ABIStr))
	if err != nil {
		panic(fmt.Sprintf("erc20 ABI parse: %v", err))
	}
}

// GetTokenMeta fetches the ERC-20 symbol and decimals of a token.
// Some non-standard tokens return bytes32 instead of string for symbol —
// we handle both encodings gracefully.
func GetTokenMeta(ctx context.Context, client *ethclient.Client, token common.Address) (symbol string, decimals int, err error) {
	// Fetch decimals
	decInput, _ := erc20ABI.Pack("decimals")
	decOut, decErr := client.CallContract(ctx, ethereum.CallMsg{To: &token, Data: decInput}, nil)
	if decErr == nil && len(decOut) >= 32 {
		decimals = int(new(big.Int).SetBytes(decOut[len(decOut)-1:]).Int64())
	} else {
		decimals = 18 // safe default
	}

	// Fetch symbol — try string encoding first
	symInput, _ := erc20ABI.Pack("symbol")
	symOut, symErr := client.CallContract(ctx, ethereum.CallMsg{To: &token, Data: symInput}, nil)
	if symErr != nil {
		return "UNKNOWN", decimals, fmt.Errorf("symbol call failed: %w", symErr)
	}

	// Attempt standard ABI string decode
	res, err := erc20ABI.Unpack("symbol", symOut)
	if err == nil && len(res) > 0 {
		if s, ok := res[0].(string); ok && s != "" {
			return s, decimals, nil
		}
	}

	// Fallback: some older tokens encode symbol as bytes32
	if len(symOut) >= 32 {
		b := symOut[:32]
		// Trim null bytes
		n := 0
		for n < 32 && b[n] != 0 {
			n++
		}
		if n > 0 {
			return string(b[:n]), decimals, nil
		}
	}

	return "UNKNOWN", decimals, nil
}
