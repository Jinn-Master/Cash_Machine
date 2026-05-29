// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "forge-std/Test.sol";
import "../contracts/ArbitrageExecutor.sol";

interface IUniswapV3Pool {
    function slot0() external view returns (
        uint160 sqrtPriceX96, int24 tick, uint16 observationIndex,
        uint16 observationCardinality, uint16 observationCardinalityNext,
        uint8 feeProtocol, bool unlocked
    );
}

contract ArbTest is Test {
    ArbitrageExecutor public executor;
    IERC20 public usdc = IERC20(0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913);
    IERC20 public weth = IERC20(0x4200000000000000000000000000000000000006);

    function setUp() public {
        executor = new ArbitrageExecutor(
            0xBA12222222228d8Ba445958a75a0704d566BF2C8,
            0x2626664c2603336E57B271c5C0b26F421741e481,
            0xcF77a3Ba9A5CA399B7c97c74d54e5b1Beb874E43,
            0x420DD381b31aEf6683db6B902084cB0FFECe40Da,
            0x420DD381b31aEf6683db6B902084cB0FFECe40Da,
            0x327Df1E6de05895d2ab08513aaDD9313Fe505d86,
            0xaaa3b1F1bd7BCc97fD1917c18ADE665C5D31F066,
            0xaaa3b1F1bd7BCc97fD1917c18ADE665C5D31F066,
            0x5edeD0D7E76C563FF081ca01d9d12d6b404e2e9f,
            5000000
        );
    }

    function testContractDeployment() public {
        assertTrue(address(executor) != address(0));
        assertEq(executor.owner(), address(this));
        assertEq(executor.minProfit(), 5000000);
        assertEq(address(executor.balancer()), 0xBA12222222228d8Ba445958a75a0704d566BF2C8);
        assertEq(address(executor.uniV3Router()), 0x2626664c2603336E57B271c5C0b26F421741e481);
        assertEq(address(executor.aeroRouter()), 0xcF77a3Ba9A5CA399B7c97c74d54e5b1Beb874E43);
    }

    function testPriceFeed() public {
        (uint160 sqrtPriceX96,,,,,,) = IUniswapV3Pool(0xd0b53D9277642d899DF5C87A3966A349A798F224).slot0();
        assertTrue(sqrtPriceX96 > 0, "sqrtPriceX96 should be > 0");

        uint256 price = (uint256(sqrtPriceX96) * 1e18) / (2**96);
        price = (price * price) / 1e18;
        price = price * 1e12;

        assertTrue(price > 1000e18 && price < 10000e18, "WETH price should be $1000-$10000");
    }

    function testAllDexRoutersSet() public view {
        assertTrue(address(executor.balancer()) != address(0));
        assertTrue(address(executor.uniV3Router()) != address(0));
        assertTrue(address(executor.aeroRouter()) != address(0));
        assertTrue(address(executor.dexBRouter()) != address(0));
        assertTrue(address(executor.dexCRouter()) != address(0));
        assertTrue(address(executor.dexDRouter()) != address(0));
        assertTrue(address(executor.maverickRouter()) != address(0));
    }
}
