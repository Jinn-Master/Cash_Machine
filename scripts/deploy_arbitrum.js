// scripts/deploy_arbitrum.js
// Deploy ArbitrageExecutor to Arbitrum One
// Usage: npx hardhat run scripts/deploy_arbitrum.js --network arbitrum

const { ethers } = require("hardhat");

// Arbitrum One addresses
const ADDRESSES = {
  aavePool:            "0x794a61358D6845594F94dc1DB02A252b5b4814aD",
  uniV3Router:         "0x68b3465833fb72A70ecDF485E0e4C7bD8665Fc45",
  aeroRouter:          ethers.ZeroAddress,  // Aerodrome is Base-only
  aeroVolatileFactory: ethers.ZeroAddress,
  aeroStableFactory:   ethers.ZeroAddress,
  dexBRouter:          "0xc873fEcbd354f5A56E00E710B90EF4201db2448d", // Camelot
  dexCRouter:          "0x1b02dA8Cb0d097eB8D57A175b88c7D8b47997506", // SushiSwap
  dexDRouter:          ethers.ZeroAddress, // GMX wrapper — set if needed
  maverickRouter:      "0x5eDEd0d7E76C563FF081Ca01D9d12D6B404e2E9f",
  minProfit:           ethers.parseUnits("1", 6), // 1 USDC minimum profit
};

async function main() {
  const [deployer] = await ethers.getSigners();
  console.log("Deploying ArbitrageExecutor to ARBITRUM ONE");
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
  console.log("\n✅ ArbitrageExecutor deployed to Arbitrum:", addr);
  console.log("Add to .env: ARBITRUM_CONTRACT_ADDRESS=" + addr);
  console.log("\nVerify with:");
  console.log(`npx hardhat verify --network arbitrum ${addr} \\`);
  console.log(`  "${ADDRESSES.aavePool}" "${ADDRESSES.uniV3Router}" "${ethers.ZeroAddress}" \\`);
  console.log(`  "${ethers.ZeroAddress}" "${ethers.ZeroAddress}" \\`);
  console.log(`  "${ADDRESSES.dexBRouter}" "${ADDRESSES.dexCRouter}" "${ADDRESSES.dexDRouter}" \\`);
  console.log(`  "${ADDRESSES.maverickRouter}" "${ADDRESSES.minProfit}"`);
}

main().catch((err) => { console.error(err); process.exit(1); });
