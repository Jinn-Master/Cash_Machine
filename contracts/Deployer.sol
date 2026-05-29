// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "../contracts/ArbitrageExecutor.sol";

// Minimal deployer — no forge-std dependency
contract Deployer {
    ArbitrageExecutor public executor;

    constructor() {
        executor = new ArbitrageExecutor(
            0xBA12222222228d8Ba445958a75a0704d566BF2C8,  // _balancer (Balancer Vault)
            0x2626664c2603336E57B271c5C0b26F421741e481,  // _uniV3Router
            0xcF77a3Ba9A5CA399B7c97c74d54e5b1Beb874E43,  // _aeroRouter
            0x420DD381b31aEf6683db6B902084cB0FFECe40Da,  // _aeroVolatileFactory
            0x420DD381b31aEf6683db6B902084cB0FFECe40Da,  // _aeroStableFactory
            0x327Df1E6de05895d2ab08513aaDD9313Fe505d86,  // _dexBRouter (BaseSwap)
            0xaaa3b1F1bd7BCc97fD1917c18ADE665C5D31F066,  // _dexCRouter (SwapBased)
            0xaaa3b1F1bd7BCc97fD1917c18ADE665C5D31F066,  // _dexDRouter (unused, reuse SwapBased)
            0x5edeD0D7E76C563FF081ca01d9d12d6b404e2e9f,  // _maverickRouter
            5000000                                         // _minProfit (0.005 USDC)
        );
    }
}
