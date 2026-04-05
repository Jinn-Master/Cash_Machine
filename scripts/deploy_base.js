const { ethers } = require("hardhat");

const ADDRESSES = {
  balancer:            "0xBA12222222228d8Ba445958a75a0704d566BF2C8", // Balancer Vault — $126K USDC, 0% fee
  uniV3Router:         "0x2626664c2603336E57B271c5C0b26F421741e481",
  aeroRouter:          "0xcF77a3Ba9A5CA399B7c97c74d54e5b1Beb874E43",
  aeroVolatileFactory: "0x420DD381b31aEf6683db6B902084cB0FFECe40Da",
  aeroStableFactory:   "0x420DD381b31aEf6683db6B902084cB0FFECe40Da",
  dexBRouter:          "0x327Df1E6de05895d2ab08513aaDD9313Fe505d86",
  dexCRouter:          "0xaaa3b1F1bd7BCc97fD1917c18ADE665C5D31F066",
  dexDRouter:          ethers.ZeroAddress,
  maverickRouter:      "0x5edeD0D7E76C563FF081ca01d9d12d6b404e2e9f",
  minProfit:           0n,
};

async function main() {
  const [deployer] = await ethers.getSigners();
  console.log("Deploying ArbitrageExecutor (Balancer) to BASE MAINNET");
  console.log("Deployer:", deployer.address);
  console.log("Balance:", ethers.formatEther(await ethers.provider.getBalance(deployer.address)), "ETH");

  const Factory = await ethers.getContractFactory("ArbitrageExecutor");
  const contract = await Factory.deploy(
    ADDRESSES.balancer, ADDRESSES.uniV3Router, ADDRESSES.aeroRouter,
    ADDRESSES.aeroVolatileFactory, ADDRESSES.aeroStableFactory,
    ADDRESSES.dexBRouter, ADDRESSES.dexCRouter, ADDRESSES.dexDRouter,
    ADDRESSES.maverickRouter, ADDRESSES.minProfit,
  );
  await contract.waitForDeployment();
  const addr = await contract.getAddress();
  console.log("\n✅ ArbitrageExecutor deployed to Base:", addr);
  console.log("Add to .env: ARBITRAGE_CONTRACT_ADDRESS=" + addr);
}

main().catch((err) => { console.error(err); process.exit(1); });
