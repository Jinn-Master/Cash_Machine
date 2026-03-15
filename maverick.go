package chain

// core/chain/maverick.go
//
// Maverick V2 quoting on Base.
//
// Maverick V2 uses directional liquidity bins — liquidity providers can add
// liquidity that moves in only one direction (up or down). This creates
// persistent mispricings vs AMMs like Uniswap V3 because:
//
//   1. When price moves strongly in one direction, the directional bins
//      provide much cheaper liquidity than static bins → lower buy price
//   2. Bins on the opposite side thin out, creating a wider spread
//
// This is different from all other DEXs we watch and creates unique arb opportunities.
//
// Maverick V2 API: quoteExactInput(bytes path, uint256 amountIn)
// The path for a single hop is: tokenIn ++ poolAddress ++ tokenOut (packed bytes)

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/yourname/money-printer/core/config"
)

const maverickV2QuoterABIStr = `[
  {
    "inputs": [
      {"internalType": "bytes",   "name": "path",     "type": "bytes"},
      {"internalType": "uint256", "name": "amountIn", "type": "uint256"}
    ],
    "name": "calculateMultiHopSwap",
    "outputs": [
      {"internalType": "uint256", "name": "returnAmount", "type": "uint256"}
    ],
    "stateMutability": "view",
    "type": "function"
  },
  {
    "inputs": [
      {"internalType": "contract IMaverickV2Pool", "name": "pool",     "type": "address"},
      {"internalType": "bool",                    "name": "tokenAIn", "type": "bool"},
      {"internalType": "uint256",                 "name": "amountIn", "type": "uint256"}
    ],
    "name": "calculateSwap",
    "outputs": [
      {"internalType": "uint256", "name": "amountOut",   "type": "uint256"},
      {"internalType": "uint256", "name": "gasEstimate", "type": "uint256"}
    ],
    "stateMutability": "view",
    "type": "function"
  }
]`

// MaverickV2PoolLensABIStr — used to find pools for a token pair
const maverickV2PoolLensABIStr = `[
  {
    "inputs": [
      {"internalType": "contract IMaverickV2Factory", "name": "factory",  "type": "address"},
      {"internalType": "contract IERC20",             "name": "tokenA",   "type": "address"},
      {"internalType": "contract IERC20",             "name": "tokenB",   "type": "address"},
      {"internalType": "uint256",                     "name": "startIndex","type": "uint256"},
      {"internalType": "uint256",                     "name": "endIndex",  "type": "uint256"}
    ],
    "name": "getTokenPairPools",
    "outputs": [
      {
        "components": [
          {"internalType": "contract IMaverickV2Pool", "name": "pool",   "type": "address"},
          {"internalType": "contract IERC20",          "name": "tokenA", "type": "address"},
          {"internalType": "contract IERC20",          "name": "tokenB", "type": "address"}
        ],
        "internalType": "struct IMaverickV2PoolLens.PoolInfo[]",
        "name": "pools",
        "type": "tuple[]"
      }
    ],
    "stateMutability": "view",
    "type": "function"
  }
]`

var (
	maverickV2QuoterABI  abi.ABI
	maverickV2LensABI    abi.ABI
)

func init() {
	var err error
	maverickV2QuoterABI, err = abi.JSON(strings.NewReader(maverickV2QuoterABIStr))
	if err != nil {
		panic(fmt.Sprintf("maverickV2 quoter ABI: %v", err))
	}
	maverickV2LensABI, err = abi.JSON(strings.NewReader(maverickV2PoolLensABIStr))
	if err != nil {
		panic(fmt.Sprintf("maverickV2 lens ABI: %v", err))
	}
}

// MaverickV2FindPool returns the best pool address for a token pair.
// Maverick V2 can have multiple pools per pair (different fee tiers / bin widths).
// We take the first one — a future improvement would score by liquidity depth.
func MaverickV2FindPool(
	ctx context.Context,
	client *ethclient.Client,
	tokenA, tokenB common.Address,
) (common.Address, bool, error) {
	// getTokenPairPools(factory, tokenA, tokenB, startIndex=0, endIndex=10)
	input, err := maverickV2LensABI.Pack("getTokenPairPools",
		config.MaverickV2Factory,
		tokenA, tokenB,
		big.NewInt(0), big.NewInt(10),
	)
	if err != nil {
		return common.Address{}, false, err
	}

	lens := config.MaverickV2PoolLens
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &lens, Data: input}, nil)
	if err != nil {
		return common.Address{}, false, err
	}

	// Decode tuple[] — each entry has pool, tokenA, tokenB
	type poolInfo struct {
		Pool   common.Address
		TokenA common.Address
		TokenB common.Address
	}
	type result struct {
		Pools []poolInfo
	}
	var res result
	if err := maverickV2LensABI.UnpackIntoInterface(&res, "getTokenPairPools", out); err != nil {
		return common.Address{}, false, fmt.Errorf("unpack getTokenPairPools: %w", err)
	}

	if len(res.Pools) == 0 {
		return common.Address{}, false, nil
	}

	// tokenAIn = true if our tokenA is the pool's tokenA
	bestPool := res.Pools[0]
	tokenAIn := bestPool.TokenA == tokenA
	return bestPool.Pool, tokenAIn, nil
}

// MaverickV2Quote returns the output amount for an exact-input swap.
// Uses calculateSwap(pool, tokenAIn, amountIn) which is a view call — no gas.
func MaverickV2Quote(
	ctx context.Context,
	client *ethclient.Client,
	poolAddr common.Address,
	tokenAIn bool,
	amountIn *big.Int,
) (*big.Int, error) {
	input, err := maverickV2QuoterABI.Pack("calculateSwap",
		poolAddr,
		tokenAIn,
		amountIn,
	)
	if err != nil {
		return nil, err
	}

	quoter := config.MaverickV2Quoter
	out, err := client.CallContract(ctx, ethereum.CallMsg{To: &quoter, Data: input}, nil)
	if err != nil {
		return nil, fmt.Errorf("maverickV2 calculateSwap: %w", err)
	}

	res, err := maverickV2QuoterABI.Unpack("calculateSwap", out)
	if err != nil {
		return nil, fmt.Errorf("maverickV2 unpack: %w", err)
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("maverickV2: empty result")
	}

	amountOut, ok := res[0].(*big.Int)
	if !ok || amountOut == nil || amountOut.Sign() == 0 {
		return nil, fmt.Errorf("maverickV2: zero output")
	}
	return amountOut, nil
}
