// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

library SafeERC20 {
    function safeTransfer(IERC20 token, address to, uint256 amount) internal {
        (bool ok, bytes memory data) = address(token).call(abi.encodeWithSelector(token.transfer.selector, to, amount));
        require(ok && (data.length == 0 || abi.decode(data, (bool))), "ST failed");
    }
    function safeApprove(IERC20 token, address spender, uint256 amount) internal {
        // Reset approval to 0 first (required by USDC and some other tokens)
        (bool s1,) = address(token).call(abi.encodeWithSelector(token.approve.selector, spender, 0));
        require(s1, "SA reset");
        (bool s2, bytes memory data) = address(token).call(abi.encodeWithSelector(token.approve.selector, spender, amount));
        require(s2 && (data.length == 0 || abi.decode(data, (bool))), "SA failed");
    }
}

interface IERC20 {
    function transfer(address to, uint256 amount) external returns (bool);
    function approve(address spender, uint256 amount) external returns (bool);
    function balanceOf(address account) external view returns (uint256);
}

interface IV3Router {
    struct ExactInputSingleParams {
        address tokenIn; address tokenOut; uint24 fee; address recipient;
        uint256 deadline; uint256 amountIn; uint256 amountOutMinimum; uint160 sqrtPriceLimitX96;
    }
    function exactInputSingle(ExactInputSingleParams calldata p) external payable returns (uint256);
}

interface IV2Router {
    function swapExactTokensForTokens(uint amountIn, uint amountOutMin, address[] calldata path, address to, uint deadline) external returns (uint[] memory);
}

interface IAerodromeRouter {
    struct Route { address from; address to; bool stable; address factory; }
    function swapExactTokensForTokens(uint amountIn, uint amountOutMin, Route[] calldata routes, address to, uint deadline) external returns (uint[] memory);
}

interface IMaverickV2Router {
    function exactInputSingle(address tokenIn, address tokenOut, address pool, address recipient, uint256 deadline, uint256 amountIn, uint256 amountOutMinimum, bytes calldata data) external returns (uint256);
}

// Balancer V2 Vault — flash loans are free on Base
interface IBalancerVault {
    function flashLoan(
        address recipient,
        address[] calldata tokens,
        uint256[] calldata amounts,
        bytes calldata userData
    ) external;
}

// Balancer flash loan callback
interface IFlashLoanRecipient {
    function receiveFlashLoan(
        address[] calldata tokens,
        uint256[] calldata amounts,
        uint256[] calldata feeAmounts,
        bytes calldata userData
    ) external;
}

contract ArbitrageExecutor is IFlashLoanRecipient {
    using SafeERC20 for IERC20;

    address public owner;
    IBalancerVault    public balancer;
    IV3Router         public uniV3Router;
    IAerodromeRouter  public aeroRouter;
    IV2Router         public dexBRouter;
    IV2Router         public dexCRouter;
    IV2Router         public dexDRouter;
    IMaverickV2Router public maverickRouter;
    address public aeroVolatileFactory;
    address public aeroStableFactory;
    uint256 public minProfit;
    bool private _locked;

    // Callback state — stored in transient-like pattern (cleared after use)
    address private _cbTokenA;
    address private _cbTokenB;
    uint24  private _cbPoolFee;
    uint8   private _cbBuyDex;
    uint8   private _cbSellDex;
    uint256 private _cbMinAB;
    uint256 private _cbMinBA;
    uint256 private _cbDeadline;
    address private _cbMaverickPool;

    event ArbExecuted(address indexed tokenA, address indexed tokenB, uint8 buyDex, uint8 sellDex, uint256 loanAmount, uint256 profit);
    event Withdrawn(address indexed token, uint256 amount);
    event MinProfitUpdated(uint256 oldVal, uint256 newVal);
    event OwnershipTransferred(address indexed prev, address indexed next);

    modifier onlyOwner() { require(msg.sender == owner, "Not owner"); _; }
    modifier nonReentrant() { require(!_locked, "Reentrant"); _locked = true; _; _locked = false; }

    constructor(
        address _balancer,
        address _uniV3Router,
        address _aeroRouter,
        address _aeroVolatileFactory,
        address _aeroStableFactory,
        address _dexBRouter,
        address _dexCRouter,
        address _dexDRouter,
        address _maverickRouter,
        uint256 _minProfit
    ) {
        require(_balancer    != address(0), "bad balancer");
        require(_uniV3Router != address(0), "bad uni");
        require(_aeroRouter  != address(0), "bad aero");
        require(_dexBRouter  != address(0), "bad dexB");
        require(_dexCRouter  != address(0), "bad dexC");
        require(_dexDRouter  != address(0), "bad dexD");
        owner               = msg.sender;
        balancer            = IBalancerVault(_balancer);
        uniV3Router         = IV3Router(_uniV3Router);
        aeroRouter          = IAerodromeRouter(_aeroRouter);
        aeroVolatileFactory = _aeroVolatileFactory;
        aeroStableFactory   = _aeroStableFactory;
        dexBRouter          = IV2Router(_dexBRouter);
        dexCRouter          = IV2Router(_dexCRouter);
        dexDRouter          = IV2Router(_dexDRouter);
        maverickRouter      = IMaverickV2Router(_maverickRouter);
        minProfit           = _minProfit;
    }

    function flashArbitrage(
        address tokenA,
        address tokenB,
        uint256 loanAmount,
        uint24 poolFee,
        uint8 buyDex,
        uint8 sellDex,
        uint256 minAB,
        uint256 minBA,
        uint256 deadline,
        address maverickPool
    ) external onlyOwner nonReentrant {
        require(buyDex < 6, "bad buyDex");
        require(sellDex < 6, "bad sellDex");
        require(buyDex != sellDex, "same dex");
        require(deadline > block.timestamp, "expired");
        require(loanAmount > 0, "zero loan");
        _cbTokenA = tokenA; _cbTokenB = tokenB; _cbPoolFee = poolFee;
        _cbBuyDex = buyDex; _cbSellDex = sellDex;
        _cbMinAB = minAB; _cbMinBA = minBA; _cbDeadline = deadline;
        _cbMaverickPool = maverickPool;

        address[] memory tokens = new address[](1);
        uint256[] memory amounts = new uint256[](1);
        tokens[0] = tokenA;
        amounts[0] = loanAmount;
        balancer.flashLoan(address(this), tokens, amounts, "");
    }

    // Balancer callback — feeAmounts should be 0 on Base (Balancer removed fees),
    // but we validate to prevent deploy on chains where fees exist.
    function receiveFlashLoan(
        address[] calldata tokens,
        uint256[] calldata amounts,
        uint256[] calldata feeAmounts,
        bytes calldata
    ) external override {
        require(msg.sender == address(balancer), "not balancer");
        require(feeAmounts.length == 1, "bad fee len");
        require(feeAmounts[0] == 0, "flash loan has fees - wrong chain or vault");

        address tokenA = _cbTokenA; address tokenB = _cbTokenB;
        uint24 poolFee = _cbPoolFee; uint8 buyDex = _cbBuyDex; uint8 sellDex = _cbSellDex;
        uint256 minAB = _cbMinAB; uint256 minBA = _cbMinBA; uint256 deadline = _cbDeadline;
        address maverickPool = _cbMaverickPool;

        // Clear callback state to prevent replay
        _cbTokenA = address(0); _cbTokenB = address(0); _cbPoolFee = 0;
        _cbBuyDex = 0; _cbSellDex = 0; _cbMinAB = 0; _cbMinBA = 0;
        _cbDeadline = 0; _cbMaverickPool = address(0);

        uint256 loanAmount = amounts[0];
        uint256 fee = feeAmounts[0]; // validated as 0 above
        uint256 repay = loanAmount + fee;

        _swapAtoB(tokenA, tokenB, loanAmount, poolFee, buyDex, minAB, deadline, maverickPool);
        uint256 midBal = IERC20(tokenB).balanceOf(address(this));
        require(midBal > 0, "no tokenB");
        _swapBtoA(tokenB, tokenA, midBal, poolFee, sellDex, minBA, deadline, maverickPool);

        uint256 endBalance = IERC20(tokenA).balanceOf(address(this));
        require(endBalance >= repay + minProfit, "profit < min");

        // Repay Balancer
        IERC20(tokenA).safeTransfer(address(balancer), repay);
        emit ArbExecuted(tokenA, tokenB, buyDex, sellDex, loanAmount, endBalance - repay);
    }

    function _swapAtoB(address tokenA, address tokenB, uint256 amountIn, uint24 poolFee, uint8 dexId, uint256 minOut, uint256 deadline, address maverickPool) internal {
        require(block.timestamp <= deadline, "swap deadline expired");
        if (dexId == 0) {
            IERC20(tokenA).safeApprove(address(uniV3Router), amountIn);
            uniV3Router.exactInputSingle(IV3Router.ExactInputSingleParams({tokenIn: tokenA, tokenOut: tokenB, fee: poolFee, recipient: address(this), deadline: deadline, amountIn: amountIn, amountOutMinimum: minOut, sqrtPriceLimitX96: 0}));
        } else if (dexId == 1) {
            require(address(aeroRouter) != address(0), "aero not set");
            IERC20(tokenA).safeApprove(address(aeroRouter), amountIn);
            IAerodromeRouter.Route[] memory routes = new IAerodromeRouter.Route[](1);
            routes[0] = IAerodromeRouter.Route({from: tokenA, to: tokenB, stable: false, factory: aeroVolatileFactory});
            aeroRouter.swapExactTokensForTokens(amountIn, minOut, routes, address(this), deadline);
        } else if (dexId == 2) {
            IERC20(tokenA).safeApprove(address(dexBRouter), amountIn);
            address[] memory path = new address[](2); path[0] = tokenA; path[1] = tokenB;
            dexBRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
        } else if (dexId == 3) {
            require(address(aeroRouter) != address(0), "aero not set");
            IERC20(tokenA).safeApprove(address(aeroRouter), amountIn);
            IAerodromeRouter.Route[] memory routes = new IAerodromeRouter.Route[](1);
            routes[0] = IAerodromeRouter.Route({from: tokenA, to: tokenB, stable: true, factory: aeroStableFactory});
            aeroRouter.swapExactTokensForTokens(amountIn, minOut, routes, address(this), deadline);
        } else if (dexId == 4) {
            IERC20(tokenA).safeApprove(address(dexCRouter), amountIn);
            address[] memory path = new address[](2); path[0] = tokenA; path[1] = tokenB;
            dexCRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
        } else {
            require(dexId == 5, "bad dexId");
            require(address(maverickRouter) != address(0), "maverick not set");
            require(maverickPool != address(0), "maverick pool not set");
            IERC20(tokenA).safeApprove(address(maverickRouter), amountIn);
            maverickRouter.exactInputSingle(tokenA, tokenB, maverickPool, address(this), deadline, amountIn, minOut, "");
        }
    }

    function _swapBtoA(address tokenB, address tokenA, uint256 amountIn, uint24 poolFee, uint8 dexId, uint256 minOut, uint256 deadline, address maverickPool) internal {
        require(block.timestamp <= deadline, "swap deadline expired");
        if (dexId == 0) {
            IERC20(tokenB).safeApprove(address(uniV3Router), amountIn);
            uniV3Router.exactInputSingle(IV3Router.ExactInputSingleParams({tokenIn: tokenB, tokenOut: tokenA, fee: poolFee, recipient: address(this), deadline: deadline, amountIn: amountIn, amountOutMinimum: minOut, sqrtPriceLimitX96: 0}));
        } else if (dexId == 1) {
            require(address(aeroRouter) != address(0), "aero not set");
            IERC20(tokenB).safeApprove(address(aeroRouter), amountIn);
            IAerodromeRouter.Route[] memory routes = new IAerodromeRouter.Route[](1);
            routes[0] = IAerodromeRouter.Route({from: tokenB, to: tokenA, stable: false, factory: aeroVolatileFactory});
            aeroRouter.swapExactTokensForTokens(amountIn, minOut, routes, address(this), deadline);
        } else if (dexId == 2) {
            IERC20(tokenB).safeApprove(address(dexBRouter), amountIn);
            address[] memory path = new address[](2); path[0] = tokenB; path[1] = tokenA;
            dexBRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
        } else if (dexId == 3) {
            require(address(aeroRouter) != address(0), "aero not set");
            IERC20(tokenB).safeApprove(address(aeroRouter), amountIn);
            IAerodromeRouter.Route[] memory routes = new IAerodromeRouter.Route[](1);
            routes[0] = IAerodromeRouter.Route({from: tokenB, to: tokenA, stable: true, factory: aeroStableFactory});
            aeroRouter.swapExactTokensForTokens(amountIn, minOut, routes, address(this), deadline);
        } else if (dexId == 4) {
            IERC20(tokenB).safeApprove(address(dexCRouter), amountIn);
            address[] memory path = new address[](2); path[0] = tokenB; path[1] = tokenA;
            dexCRouter.swapExactTokensForTokens(amountIn, minOut, path, address(this), deadline);
        } else {
            require(dexId == 5, "bad dexId");
            require(address(maverickRouter) != address(0), "maverick not set");
            require(maverickPool != address(0), "maverick pool not set");
            IERC20(tokenB).safeApprove(address(maverickRouter), amountIn);
            maverickRouter.exactInputSingle(tokenB, tokenA, maverickPool, address(this), deadline, amountIn, minOut, "");
        }
    }

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
