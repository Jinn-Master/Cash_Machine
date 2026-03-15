// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

// ============================================================
//  SafeERC20 (inline minimal)
// ============================================================
library SafeERC20 {
    function safeTransfer(IERC20 token, address to, uint256 amount) internal {
        (bool ok, bytes memory data) = address(token).call(
            abi.encodeWithSelector(token.transfer.selector, to, amount)
        );
        require(ok && (data.length == 0 || abi.decode(data, (bool))), "ST failed");
    }

    function safeApprove(IERC20 token, address spender, uint256 amount) internal {
        (bool s1,) = address(token).call(abi.encodeWithSelector(token.approve.selector, spender, 0));
        require(s1, "SA reset");
        (bool s2, bytes memory data) = address(token).call(
            abi.encodeWithSelector(token.approve.selector, spender, amount)
        );
        require(s2 && (data.length == 0 || abi.decode(data, (bool))), "SA failed");
    }
}

// ============================================================
//  Interfaces
// ============================================================
interface IERC20 {
    function transfer(address to, uint256 amount) external returns (bool);
    function approve(address spender, uint256 amount) external returns (bool);
    function balanceOf(address account) external view returns (uint256);
}

/// @dev Uniswap V2-compatible router (Aerodrome V1/legacy, BaseSwap, SushiSwap on Base)
interface IV2Router {
    function swapExactTokensForTokens(
        uint amountIn, uint amountOutMin,
        address[] calldata path, address to, uint deadline
    ) external returns (uint[] memory);
}

/// @dev Uniswap V3-compatible router (Uniswap V3 on Base)
interface IV3Router {
    struct ExactInputSingleParams {
        address tokenIn; address tokenOut; uint24 fee; address recipient;
        uint256 deadline; uint256 amountIn; uint256 amountOutMinimum;
        uint160 sqrtPriceLimitX96;
    }
    function exactInputSingle(ExactInputSingleParams calldata p) external payable returns (uint256);
}

/// @dev Aerodrome V2 router (volatile + stable pools, no fee tier)
interface IAerodromeRouter {
    struct Route {
        address from; address to; bool stable; address factory;
    }
    function swapExactTokensForTokens(
        uint amountIn, uint amountOutMin,
        Route[] calldata routes, address to, uint deadline
    ) external returns (uint[] memory);
}

/// @dev Aave V3 flash loan pool
interface IPool {
    function flashLoanSimple(
        address receiverAddress,
        address asset,
        uint256 amount,
        bytes calldata params,
        uint16 referralCode
    ) external;
}

/// @dev Aave V3 flash loan callback
interface IFlashLoanSimpleReceiver {
    function executeOperation(
        address asset,
        uint256 amount,
        uint256 premium,
        address initiator,
        bytes calldata params
    ) external returns (bool);
}

// ============================================================
//  ArbitrageExecutor — Base chain, 5 DEXs, Aave V3 flash loans
//
//  DEX indices (dexId):
//    0 = Uniswap V3             (V3 router)
//    1 = Aerodrome volatile     (Aerodrome router, stable=false)
//    2 = BaseSwap               (V2-compatible router)
//    3 = Aerodrome stable       (Aerodrome router, stable=true)
//    4 = SwapBased              (V2-compatible router)
//
//  Direction encoding:
//    buyDex  = dexId where we BUY  tokenB (spend tokenA)
//    sellDex = dexId where we SELL tokenB (receive tokenA)
// ============================================================
contract ArbitrageExecutor is IFlashLoanSimpleReceiver {
    using SafeERC20 for IERC20;

    // ── Storage ─────────────────────────────────────────────────────────────
    address public owner;
    IPool            public aavePool;
    IV3Router        public uniV3Router;
    IAerodromeRouter public aeroRouter;
    IV2Router        public baseswapRouter;
    IV2Router        public swapBasedRouter;
    address public aeroVolatileFactory;
    address public aeroStableFactory;

    uint256 public minProfit;
    bool private _locked;

    // Transient storage for flash loan callback
    address private _cbTokenA;
    address private _cbTokenB;
    uint24  private _cbPoolFee;
    uint8   private _cbBuyDex;
    uint8   private _cbSellDex;
    uint256 private _cbMinAB;
    uint256 private _cbMinBA;
    uint256 private _cbDeadline;

    // ── Events ───────────────────────────────────────────────────────────────
    event ArbExecuted(
        address indexed tokenA, address indexed tokenB,
        uint8 buyDex, uint8 sellDex,
        uint256 loanAmount, uint256 profit
    );
    event Withdrawn(address indexed token, uint256 amount);
    event MinProfitUpdated(uint256 oldVal, uint256 newVal);
    event OwnershipTransferred(address indexed prev, address indexed next);

    // ── Modifiers ────────────────────────────────────────────────────────────
    modifier onlyOwner() { require(msg.sender == owner, "Not owner"); _; }
    modifier nonReentrant() {
        require(!_locked, "Reentrant");
        _locked = true; _; _locked = false;
    }

    // ── Constructor ──────────────────────────────────────────────────────────
    constructor(
        address _aavePool,
        address _uniV3Router,
        address _aeroRouter,
        address _aeroVolatileFactory,
        address _aeroStableFactory,
        address _baseswapRouter,
        address _swapBasedRouter,
        uint256 _minProfit
    ) {
        require(_aavePool            != address(0), "bad aave");
        require(_uniV3Router         != address(0), "bad uni");
        require(_aeroRouter          != address(0), "bad aero");
        require(_aeroVolatileFactory != address(0), "bad aeroVolFactory");
        require(_aeroStableFactory   != address(0), "bad aeroStabFactory");
        require(_baseswapRouter      != address(0), "bad baseswap");
        require(_swapBasedRouter     != address(0), "bad swapbased");
        owner                = msg.sender;
        aavePool             = IPool(_aavePool);
        uniV3Router          = IV3Router(_uniV3Router);
        aeroRouter           = IAerodromeRouter(_aeroRouter);
        aeroVolatileFactory  = _aeroVolatileFactory;
        aeroStableFactory    = _aeroStableFactory;
        baseswapRouter       = IV2Router(_baseswapRouter);
        swapBasedRouter      = IV2Router(_swapBasedRouter);
        minProfit            = _minProfit;
    }

    // ────────────────────────────────────────────────────────────────────────
    //  Entry point: called by our off-chain bot
    //
    //  @param tokenA       Flash-loan asset (e.g. USDC, WETH)
    //  @param tokenB       Intermediate asset
    //  @param loanAmount   Amount of tokenA to borrow
    //  @param poolFee      Uniswap V3 fee tier (ignored for V2/Aero legs)
    //  @param buyDex       DEX id (0/1/2) to buy tokenB on
    //  @param sellDex      DEX id (0/1/2) to sell tokenB on
    //  @param minAB        Min tokenB out on buy leg (slippage guard)
    //  @param minBA        Min tokenA out on sell leg (must cover loan+premium+profit)
    //  @param deadline     Unix timestamp deadline
    // ────────────────────────────────────────────────────────────────────────
    function flashArbitrage(
        address tokenA,
        address tokenB,
        uint256 loanAmount,
        uint24  poolFee,
        uint8   buyDex,
        uint8   sellDex,
        uint256 minAB,
        uint256 minBA,
        uint256 deadline
    ) external onlyOwner nonReentrant {
        require(buyDex  < 5,           "bad buyDex");
        require(sellDex < 5,           "bad sellDex");
        require(buyDex != sellDex,     "same dex");
        require(deadline > block.timestamp, "expired");
        require(loanAmount > 0,        "zero loan");

        // Store callback params in storage (cleared in executeOperation)
        _cbTokenA  = tokenA;
        _cbTokenB  = tokenB;
        _cbPoolFee = poolFee;
        _cbBuyDex  = buyDex;
        _cbSellDex = sellDex;
        _cbMinAB   = minAB;
        _cbMinBA   = minBA;
        _cbDeadline = deadline;

        // Trigger Aave V3 flash loan — callback fires synchronously
        aavePool.flashLoanSimple(
            address(this),
            tokenA,
            loanAmount,
            "",         // params unused; we use storage
            0
        );
    }

    // ────────────────────────────────────────────────────────────────────────
    //  Aave V3 flash loan callback
    //  Called by Aave during flashLoanSimple. We have the funds here.
    //  We must repay (amount + premium) before this function returns.
    // ────────────────────────────────────────────────────────────────────────
    function executeOperation(
        address asset,
        uint256 amount,
        uint256 premium,
        address initiator,
        bytes calldata /*params*/
    ) external override returns (bool) {
        require(msg.sender == address(aavePool),   "not aave");
        require(initiator  == address(this),       "not self");

        // Load and clear transient storage
        address tokenA  = _cbTokenA;
        address tokenB  = _cbTokenB;
        uint24  poolFee = _cbPoolFee;
        uint8   buyDex  = _cbBuyDex;
        uint8   sellDex = _cbSellDex;
        uint256 minAB   = _cbMinAB;
        uint256 minBA   = _cbMinBA;
        uint256 deadline = _cbDeadline;
        _cbTokenA = address(0); _cbTokenB = address(0);

        uint256 startBalance = IERC20(tokenA).balanceOf(address(this));
        // startBalance == amount (the loan)

        // ── Leg 1: buy tokenB on buyDex ─────────────────────────────────────
        _swapAtoB(tokenA, tokenB, amount, poolFee, buyDex, minAB, deadline);

        // ── Leg 2: sell tokenB on sellDex ───────────────────────────────────
        uint256 midBal = IERC20(tokenB).balanceOf(address(this));
        require(midBal > 0, "no tokenB");
        _swapBtoA(tokenB, tokenA, midBal, poolFee, sellDex, minBA, deadline);

        // ── Repay Aave: amount + premium ─────────────────────────────────────
        uint256 repay = amount + premium;
        uint256 endBalance = IERC20(tokenA).balanceOf(address(this));
        require(endBalance >= repay + minProfit, "profit < min");

        IERC20(tokenA).safeApprove(address(aavePool), repay);

        uint256 profit = endBalance - repay;
        emit ArbExecuted(tokenA, tokenB, buyDex, sellDex, amount, profit);

        return true;
    }

    // ────────────────────────────────────────────────────────────────────────
    //  Internal swap helpers
    // ────────────────────────────────────────────────────────────────────────

    function _swapAtoB(
        address tokenA, address tokenB,
        uint256 amountIn, uint24 poolFee,
        uint8 dexId, uint256 minOut, uint256 deadline
    ) internal {
        if (dexId == 0) {
            // Uniswap V3
            IERC20(tokenA).safeApprove(address(uniV3Router), amountIn);
            uniV3Router.exactInputSingle(IV3Router.ExactInputSingleParams({
                tokenIn: tokenA, tokenOut: tokenB, fee: poolFee,
                recipient: address(this), deadline: deadline,
                amountIn: amountIn, amountOutMinimum: minOut, sqrtPriceLimitX96: 0
            }));
        } else if (dexId == 1) {
            // Aerodrome volatile
            IERC20(tokenA).safeApprove(address(aeroRouter), amountIn);
            IAerodromeRouter.Route[] memory routes = new IAerodromeRouter.Route[](1);
            routes[0] = IAerodromeRouter.Route({
                from: tokenA, to: tokenB, stable: false, factory: aeroVolatileFactory
            });
            aeroRouter.swapExactTokensForTokens(amountIn, minOut, routes, address(this), deadline);
        } else if (dexId == 2) {
            // BaseSwap (V2)
            IERC20(tokenA).safeApprove(address(baseswapRouter), amountIn);
            address[] memory path = new address[](2);
            path[0] = tokenA; path[1] = tokenB;
            baseswapRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
        } else if (dexId == 3) {
            // Aerodrome stable
            IERC20(tokenA).safeApprove(address(aeroRouter), amountIn);
            IAerodromeRouter.Route[] memory routes = new IAerodromeRouter.Route[](1);
            routes[0] = IAerodromeRouter.Route({
                from: tokenA, to: tokenB, stable: true, factory: aeroStableFactory
            });
            aeroRouter.swapExactTokensForTokens(amountIn, minOut, routes, address(this), deadline);
        } else {
            // SwapBased (V2) — dexId == 4
            require(dexId == 4, "invalid dexId");
            IERC20(tokenA).safeApprove(address(swapBasedRouter), amountIn);
            address[] memory path = new address[](2);
            path[0] = tokenA; path[1] = tokenB;
            swapBasedRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
        }
    }

    function _swapBtoA(
        address tokenB, address tokenA,
        uint256 amountIn, uint24 poolFee,
        uint8 dexId, uint256 minOut, uint256 deadline
    ) internal {
        if (dexId == 0) {
            IERC20(tokenB).safeApprove(address(uniV3Router), amountIn);
            uniV3Router.exactInputSingle(IV3Router.ExactInputSingleParams({
                tokenIn: tokenB, tokenOut: tokenA, fee: poolFee,
                recipient: address(this), deadline: deadline,
                amountIn: amountIn, amountOutMinimum: minOut, sqrtPriceLimitX96: 0
            }));
        } else if (dexId == 1) {
            IERC20(tokenB).safeApprove(address(aeroRouter), amountIn);
            IAerodromeRouter.Route[] memory routes = new IAerodromeRouter.Route[](1);
            routes[0] = IAerodromeRouter.Route({
                from: tokenB, to: tokenA, stable: false, factory: aeroVolatileFactory
            });
            aeroRouter.swapExactTokensForTokens(amountIn, minOut, routes, address(this), deadline);
        } else if (dexId == 2) {
            IERC20(tokenB).safeApprove(address(baseswapRouter), amountIn);
            address[] memory path = new address[](2);
            path[0] = tokenB; path[1] = tokenA;
            baseswapRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
        } else if (dexId == 3) {
            IERC20(tokenB).safeApprove(address(aeroRouter), amountIn);
            IAerodromeRouter.Route[] memory routes = new IAerodromeRouter.Route[](1);
            routes[0] = IAerodromeRouter.Route({
                from: tokenB, to: tokenA, stable: true, factory: aeroStableFactory
            });
            aeroRouter.swapExactTokensForTokens(amountIn, minOut, routes, address(this), deadline);
        } else {
            require(dexId == 4, "invalid dexId");
            IERC20(tokenB).safeApprove(address(swapBasedRouter), amountIn);
            address[] memory path = new address[](2);
            path[0] = tokenB; path[1] = tokenA;
            swapBasedRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
        }
    }

    // ────────────────────────────────────────────────────────────────────────
    //  Admin
    // ────────────────────────────────────────────────────────────────────────

    function setMinProfit(uint256 _minProfit) external onlyOwner {
        emit MinProfitUpdated(minProfit, _minProfit);
        minProfit = _minProfit;
    }

    function withdraw(address token) external onlyOwner nonReentrant {
        uint256 bal = IERC20(token).balanceOf(address(this));
        require(bal > 0, "empty");
        IERC20(token).safeTransfer(owner, bal);
        emit Withdrawn(token, bal);
    }

    function withdrawNative() external onlyOwner nonReentrant {
        uint256 bal = address(this).balance;
        require(bal > 0, "no native");
        (bool ok,) = payable(owner).call{value: bal}("");
        require(ok, "transfer failed");
    }

    function transferOwnership(address newOwner) external onlyOwner {
        require(newOwner != address(0), "zero addr");
        emit OwnershipTransferred(owner, newOwner);
        owner = newOwner;
    }

    receive() external payable {}
}
