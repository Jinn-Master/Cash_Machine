// scripts/deploy_base.js
// Deploy ArbitrageExecutor to Base Mainnet
// Usage: npx hardhat run scripts/deploy_base.js --network base

const { ethers } = require("hardhat");

// Base Mainnet addresses
const ADDRESSES = {
  aavePool:            "0xA238Dd80C259a72e81d7e4664a9801593F98d1c5",
  uniV3Router:         "0x2626664c2603336E57B271c5C0b26F421741e481",
  aeroRouter:          "0xcF77a3Ba9A5CA399B7c97c74d54e5b1Beb874E43",
  aeroVolatileFactory: "0x420DD381b31aEf6683db6B902084cB0FFECe40Da",
  aeroStableFactory:   "0x420DD381b31aEf6683db6B902084cB0FFECe40Da",
  dexBRouter:          "0x327Df1E6de05895d2ab08513aaDD9313Fe505d86", // BaseSwap
  dexCRouter:          "0xaaa3b1F1bd7BCc97fD1917c18ADE665C5D31F066", // SwapBased
  dexDRouter:          ethers.ZeroAddress, // not used on Base
  maverickRouter:      "0x5eDEd0d7E76C563FF081Ca01D9d12D6B404e2E9f",
  minProfit:           ethers.parseUnits("1", 6), // 1 USDC minimum profit
};

async function main() {
  const [deployer] = await ethers.getSigners();
  console.log("Deploying ArbitrageExecutor to BASE MAINNET");
  console.log("Deployer:", deployer.address);
  console.log("Balance:", ethers.formatEther(await ethers.provider.getBalance(deployer.address)), "ETH");

  const Factory = await ethers.getContractFactory("ArbitrageExecutor");
  const contract = await Factory.deploy(
    ADDRESSES.aavePool,
    ADDRESSES.uniV3Router,
    ADDRESSES.aeroRouter,
    ADDRESSES.aeroVolatileFactory,
    ADDRESSES.aeroStableFactory,
    ADDRESSES.dexBRouter,
    ADDRESSES.dexCRouter,
    ADDRESSES.dexDRouter,
    ADDRESSES.maverickRouter,
    ADDRESSES.minProfit,
  );
  await contract.waitForDeployment();

  const addr = await contract.getAddress();
  console.log("\n✅ ArbitrageExecutor deployed to Base:", addr);
  console.log("Add to .env: ARBITRAGE_CONTRACT_ADDRESS=" + addr);
  console.log("\nVerify with:");
  console.log(`npx hardhat verify --network base ${addr} \\`);
  console.log(`  "${ADDRESSES.aavePool}" "${ADDRESSES.uniV3Router}" "${ADDRESSES.aeroRouter}" \\`);
  console.log(`  "${ADDRESSES.aeroVolatileFactory}" "${ADDRESSES.aeroStableFactory}" \\`);
  console.log(`  "${ADDRESSES.dexBRouter}" "${ADDRESSES.dexCRouter}" "${ADDRESSES.dexDRouter}" \\`);
  console.log(`  "${ADDRESSES.maverickRouter}" "${ADDRESSES.minProfit}"`);
}

main().catch((err) => { console.error(err); process.exit(1); });
