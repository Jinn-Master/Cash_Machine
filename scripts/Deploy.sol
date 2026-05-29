// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import "forge-std/Script.sol";
import "../contracts/ArbitrageExecutor.sol";

contract DeployScript is Script {
    function run() external {
        uint256 deployerPrivateKey = vm.envUint("PRIVATE_KEY");

        vm.startBroadcast(deployerPrivateKey);

        ArbitrageExecutor executor = new ArbitrageExecutor(
            0xBA12222222228d8Ba445958a75a0704d566BF2C8,  // Balancer Vault
            0x2626664c2603336E57B271c5C0b26F421741e481,  // UniV3 Router
            0xcF77a3Ba9A5CA399B7c97c74d54e5b1Beb874E43,  // Aerodrome Router
            0x420DD381b31aEf6683db6B902084cB0FFECe40Da,  // Aero Volatile Factory
            0x420DD381b31aEf6683db6B902084cB0FFECe40Da,  // Aero Stable Factory
            0x327Df1E6de05895d2ab08513aaDD9313Fe505d86,  // BaseSwap Router
            0xaaa3b1F1bd7BCc97fD1917c18ADE665C5D31F066,  // SwapBased Router
            0x5eDEd0d7E76C563FF081Ca01D9d12D6B404e2E9f,  // Maverick V2 Router
            5000000                                         // minProfit (0.005 USDC)
        );

        console.log("ArbitrageExecutor deployed at:", address(executor));
        console.log("Owner:", executor.owner());
        console.log("MinProfit:", executor.minProfit());

        vm.stopBroadcast();
    }
}
