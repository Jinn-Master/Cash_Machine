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

/// @dev Uniswap V3-compatible router (Uniswap V3 on Base & Arbitrum)
interface IV3Router {
    struct ExactInputSingleParams {
        address tokenIn; address tokenOut; uint24 fee; address recipient;
        uint256 deadline; uint256 amountIn; uint256 amountOutMinimum;
        uint160 sqrtPriceLimitX96;
    }
    function exactInputSingle(ExactInputSingleParams calldata p) external payable returns (uint256);
}

/// @dev Uniswap V2-compatible router (BaseSwap, SwapBased on Base; SushiSwap, Camelot on Arbitrum)
interface IV2Router {
    function swapExactTokensForTokens(
        uint amountIn, uint amountOutMin,
        address[] calldata path, address to, uint deadline
    ) external returns (uint[] memory);
}

/// @dev Aerodrome V2 router (volatile + stable pools, Base only)
interface IAerodromeRouter {
    struct Route {
        address from; address to; bool stable; address factory;
    }
    function swapExactTokensForTokens(
        uint amountIn, uint amountOutMin,
        Route[] calldata routes, address to, uint deadline
    ) external returns (uint[] memory);
}

/// @dev Maverick V2 router (Base & Arbitrum — directional liquidity bins)
interface IMaverickV2Router {
    function exactInputSingle(
        address tokenIn,
        address tokenOut,
        address pool,
        address recipient,
        uint256 deadline,
        uint256 amountIn,
        uint256 amountOutMinimum,
        bytes calldata data
    ) external returns (uint256 amountOut);
}

/// @dev Aave V3 flash loan pool (Base & Arbitrum)
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
//  ArbitrageExecutor — Base & Arbitrum, 6 DEXs, Aave V3 flash loans
//
//  DEX indices (dexId):
//    Base chain:
//      0 = Uniswap V3             (V3 router)
//      1 = Aerodrome volatile     (Aerodrome router, stable=false)
//      2 = BaseSwap               (V2-compatible router)
//      3 = Aerodrome stable       (Aerodrome router, stable=true)
//      4 = SwapBased              (V2-compatible router)
//      5 = Maverick V2            (Maverick router)
//
//    Arbitrum chain:
//      0 = Uniswap V3             (V3 router)
//      1 = Camelot                (V2-compatible router)
//      2 = SushiSwap              (V2-compatible router)
//      3 = GMX (via wrapper)      (V2-compatible router)
//      4 = Curve (via wrapper)    (V2-compatible router)
//      5 = Maverick V2            (Maverick router)
// ============================================================
contract ArbitrageExecutor is IFlashLoanSimpleReceiver {
    using SafeERC20 for IERC20;

    // ── Storage ─────────────────────────────────────────────────────────────
    address public owner;
    IPool            public aavePool;
    IV3Router        public uniV3Router;
    IAerodromeRouter public aeroRouter;      // zero on Arbitrum
    IV2Router        public dexBRouter;      // BaseSwap (Base) / Camelot (Arb)
    IV2Router        public dexCRouter;      // SwapBased (Base) / SushiSwap (Arb)
    IV2Router        public dexDRouter;      // GMX/Curve wrapper (Arb) / zero (Base)
    IMaverickV2Router public maverickRouter;
    address public aeroVolatileFactory;      // zero on Arbitrum
    address public aeroStableFactory;        // zero on Arbitrum

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
    address private _cbMaverickPool; // Maverick pool address for dexId=5

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
        address _aeroRouter,           // pass address(0) on Arbitrum
        address _aeroVolatileFactory,  // pass address(0) on Arbitrum
        address _aeroStableFactory,    // pass address(0) on Arbitrum
        address _dexBRouter,           // BaseSwap (Base) / Camelot (Arb)
        address _dexCRouter,           // SwapBased (Base) / SushiSwap (Arb)
        address _dexDRouter,           // GMX/Curve wrapper (Arb) / address(0) on Base
        address _maverickRouter,
        uint256 _minProfit
    ) {
        require(_aavePool    != address(0), "bad aave");
        require(_uniV3Router != address(0), "bad uni");
        require(_dexBRouter  != address(0), "bad dexB");
        require(_dexCRouter  != address(0), "bad dexC");
        owner                = msg.sender;
        aavePool             = IPool(_aavePool);
        uniV3Router          = IV3Router(_uniV3Router);
        aeroRouter           = IAerodromeRouter(_aeroRouter);
        aeroVolatileFactory  = _aeroVolatileFactory;
        aeroStableFactory    = _aeroStableFactory;
        dexBRouter           = IV2Router(_dexBRouter);
        dexCRouter           = IV2Router(_dexCRouter);
        dexDRouter           = IV2Router(_dexDRouter);
        maverickRouter       = IMaverickV2Router(_maverickRouter);
        minProfit            = _minProfit;
    }

    // ── Entry point ──────────────────────────────────────────────────────────
    function flashArbitrage(
        address tokenA,
        address tokenB,
        uint256 loanAmount,
        uint24  poolFee,
        uint8   buyDex,
        uint8   sellDex,
        uint256 minAB,
        uint256 minBA,
        uint256 deadline,
        address maverickPool  // pass address(0) if neither leg uses Maverick
    ) external onlyOwner nonReentrant {
        require(buyDex  < 6,           "bad buyDex");
        require(sellDex < 6,           "bad sellDex");
        require(buyDex != sellDex,     "same dex");
        require(deadline > block.timestamp, "expired");
        require(loanAmount > 0,        "zero loan");

        _cbTokenA      = tokenA;
        _cbTokenB      = tokenB;
        _cbPoolFee     = poolFee;
        _cbBuyDex      = buyDex;
        _cbSellDex     = sellDex;
        _cbMinAB       = minAB;
        _cbMinBA       = minBA;
        _cbDeadline    = deadline;
        _cbMaverickPool = maverickPool;

        aavePool.flashLoanSimple(address(this), tokenA, loanAmount, "", 0);
    }

    // ── Aave V3 flash loan callback ──────────────────────────────────────────
    function executeOperation(
        address /*asset*/,
        uint256 amount,
        uint256 premium,
        address initiator,
        bytes calldata /*params*/
    ) external override returns (bool) {
        require(msg.sender == address(aavePool), "not aave");
        require(initiator  == address(this),     "not self");

        // Load callback storage
        address tokenA       = _cbTokenA;
        address tokenB       = _cbTokenB;
        uint24  poolFee      = _cbPoolFee;
        uint8   buyDex       = _cbBuyDex;
        uint8   sellDex      = _cbSellDex;
        uint256 minAB        = _cbMinAB;
        uint256 minBA        = _cbMinBA;
        uint256 deadline     = _cbDeadline;
        address maverickPool = _cbMaverickPool;

        // Clear all callback storage (gas refund + safety)
        _cbTokenA = address(0); _cbTokenB = address(0);
        _cbPoolFee = 0; _cbBuyDex = 0; _cbSellDex = 0;
        _cbMinAB = 0; _cbMinBA = 0; _cbDeadline = 0;
        _cbMaverickPool = address(0);

        // Leg 1: buy tokenB with tokenA
        _swapAtoB(tokenA, tokenB, amount, poolFee, buyDex, minAB, deadline, maverickPool);

        // Leg 2: sell tokenB back to tokenA
        uint256 midBal = IERC20(tokenB).balanceOf(address(this));
        require(midBal > 0, "no tokenB");
        _swapBtoA(tokenB, tokenA, midBal, poolFee, sellDex, minBA, deadline, maverickPool);

        // Verify profit and repay Aave
        uint256 repay      = amount + premium;
        uint256 endBalance = IERC20(tokenA).balanceOf(address(this));
        require(endBalance >= repay + minProfit, "profit < min");

        IERC20(tokenA).safeApprove(address(aavePool), repay);

        emit ArbExecuted(tokenA, tokenB, buyDex, sellDex, amount, endBalance - repay);
        return true;
    }

    // ── Internal swap helpers ────────────────────────────────────────────────

    function _swapAtoB(
        address tokenA, address tokenB,
        uint256 amountIn, uint24 poolFee,
        uint8 dexId, uint256 minOut, uint256 deadline,
        address maverickPool
    ) internal {
        if (dexId == 0) {
            IERC20(tokenA).safeApprove(address(uniV3Router), amountIn);
            uniV3Router.exactInputSingle(IV3Router.ExactInputSingleParams({
                tokenIn: tokenA, tokenOut: tokenB, fee: poolFee,
                recipient: address(this), deadline: deadline,
                amountIn: amountIn, amountOutMinimum: minOut, sqrtPriceLimitX96: 0
            }));
        } else if (dexId == 1) {
            require(address(aeroRouter) != address(0), "aero not set");
            IERC20(tokenA).safeApprove(address(aeroRouter), amountIn);
            IAerodromeRouter.Route[] memory routes = new IAerodromeRouter.Route[](1);
            routes[0] = IAerodromeRouter.Route({ from: tokenA, to: tokenB, stable: false, factory: aeroVolatileFactory });
            aeroRouter.swapExactTokensForTokens(amountIn, minOut, routes, address(this), deadline);
        } else if (dexId == 2) {
            IERC20(tokenA).safeApprove(address(dexBRouter), amountIn);
            address[] memory path = new address[](2);
            path[0] = tokenA; path[1] = tokenB;
            dexBRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
        } else if (dexId == 3) {
            if (address(aeroRouter) != address(0)) {
                IERC20(tokenA).safeApprove(address(aeroRouter), amountIn);
                IAerodromeRouter.Route[] memory routes = new IAerodromeRouter.Route[](1);
                routes[0] = IAerodromeRouter.Route({ from: tokenA, to: tokenB, stable: true, factory: aeroStableFactory });
                aeroRouter.swapExactTokensForTokens(amountIn, minOut, routes, address(this), deadline);
            } else {
                require(address(dexDRouter) != address(0), "dexD not set");
                IERC20(tokenA).safeApprove(address(dexDRouter), amountIn);
                address[] memory path = new address[](2);
                path[0] = tokenA; path[1] = tokenB;
                dexDRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
            }
        } else if (dexId == 4) {
            IERC20(tokenA).safeApprove(address(dexCRouter), amountIn);
            address[] memory path = new address[](2);
            path[0] = tokenA; path[1] = tokenB;
            dexCRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
        } else {
            // dexId == 5: Maverick V2
            require(address(maverickRouter) != address(0), "maverick not set");
            require(maverickPool != address(0), "maverick pool not set");
            IERC20(tokenA).safeApprove(address(maverickRouter), amountIn);
            maverickRouter.exactInputSingle(tokenA, tokenB, maverickPool, address(this), deadline, amountIn, minOut, "");
        }
    }

    function _swapBtoA(
        address tokenB, address tokenA,
        uint256 amountIn, uint24 poolFee,
        uint8 dexId, uint256 minOut, uint256 deadline,
        address maverickPool
    ) internal {
        if (dexId == 0) {
            IERC20(tokenB).safeApprove(address(uniV3Router), amountIn);
            uniV3Router.exactInputSingle(IV3Router.ExactInputSingleParams({
                tokenIn: tokenB, tokenOut: tokenA, fee: poolFee,
                recipient: address(this), deadline: deadline,
                amountIn: amountIn, amountOutMinimum: minOut, sqrtPriceLimitX96: 0
            }));
        } else if (dexId == 1) {
            require(address(aeroRouter) != address(0), "aero not set");
            IERC20(tokenB).safeApprove(address(aeroRouter), amountIn);
            IAerodromeRouter.Route[] memory routes = new IAerodromeRouter.Route[](1);
            routes[0] = IAerodromeRouter.Route({ from: tokenB, to: tokenA, stable: false, factory: aeroVolatileFactory });
            aeroRouter.swapExactTokensForTokens(amountIn, minOut, routes, address(this), deadline);
        } else if (dexId == 2) {
            IERC20(tokenB).safeApprove(address(dexBRouter), amountIn);
            address[] memory path = new address[](2);
            path[0] = tokenB; path[1] = tokenA;
            dexBRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
        } else if (dexId == 3) {
            if (address(aeroRouter) != address(0)) {
                IERC20(tokenB).safeApprove(address(aeroRouter), amountIn);
                IAerodromeRouter.Route[] memory routes = new IAerodromeRouter.Route[](1);
                routes[0] = IAerodromeRouter.Route({ from: tokenB, to: tokenA, stable: true, factory: aeroStableFactory });
                aeroRouter.swapExactTokensForTokens(amountIn, minOut, routes, address(this), deadline);
            } else {
                require(address(dexDRouter) != address(0), "dexD not set");
                IERC20(tokenB).safeApprove(address(dexDRouter), amountIn);
                address[] memory path = new address[](2);
                path[0] = tokenB; path[1] = tokenA;
                dexDRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
            }
        } else if (dexId == 4) {
            IERC20(tokenB).safeApprove(address(dexCRouter), amountIn);
            address[] memory path = new address[](2);
            path[0] = tokenB; path[1] = tokenA;
            dexCRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
        } else {
            // dexId == 5: Maverick V2
            require(address(maverickRouter) != address(0), "maverick not set");
            require(maverickPool != address(0), "maverick pool not set");
            IERC20(tokenB).safeApprove(address(maverickRouter), amountIn);
            maverickRouter.exactInputSingle(tokenB, tokenA, maverickPool, address(this), deadline, amountIn, minOut, "");
        }
    }

    // ── Admin ────────────────────────────────────────────────────────────────

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
