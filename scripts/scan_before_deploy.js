// scripts/scan_before_deploy.js
// Run before any deployment to verify all addresses, liquidity, and pool existence
// Usage: npx hardhat run scripts/scan_before_deploy.js --network base

const { ethers } = require("hardhat");

const USDC  = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913";
const WETH  = "0x4200000000000000000000000000000000000006";
const DAI   = "0x50c5725949A6F0c72E6C4a641F24049A917DB0Cb";

const ADDRESSES = {
  // Flash loan providers — check USDC balance
  flashProviders: [
    { name: "Morpho Blue",     addr: "0xBBBBBbbBBb9cC5e90e3b3Af64bdAF62C37EEFFCb" },
    { name: "Aave V3",         addr: "0xA238Dd80C259a72e81d7e4664a9801593F98d1c5" },
    { name: "Balancer Vault",  addr: "0xBA12222222228d8Ba445958a75a0704d566BF2C8" },
    { name: "Compound V3",     addr: "0xb125E6687d4313864e53df431d5425969c15Eb2" },
    { name: "Moonwell USDC",   addr: "0xEdc817A28E8B93B03976FBd4a3dDBc9f7D176c22" },
    { name: "Seamless USDC",   addr: "0x0568A3aEB8E78262dEFf861C4e4735b9c0D4B671" },
  ],

  // DEX routers — verify they have code
  routers: [
    { name: "UniV3 Router",    addr: "0x2626664c2603336E57B271c5C0b26F421741e481" },
    { name: "UniV3 Quoter",    addr: "0x3d4e44Eb1374240CE5F1B871ab261CD16335B76a" },
    { name: "Aero Router",     addr: "0xcF77a3Ba9A5CA399B7c97c74d54e5b1Beb874E43" },
    { name: "Aero Factory",    addr: "0x420DD381b31aEf6683db6B902084cB0FFECe40Da" },
    { name: "BaseSwap Router", addr: "0x327Df1E6de05895d2ab08513aaDD9313Fe505d86" },
    { name: "SwapBased Router",addr: "0xaaa3b1F1bd7BCc97fD1917c18ADE665C5D31F066" },
    { name: "Maverick Router", addr: "0x5edeD0D7E76C563FF081ca01d9d12d6b404e2e9f" },
  ],

  // UniV3 pools — check they exist and have liquidity
  uniV3Pools: [
    { name: "USDC/WETH 0.05%", addr: "0xd0b53D9277642d899DF5C87A3966A349A798F224" },
    { name: "USDC/WETH 0.01%", addr: "0xb4CB800910B228ED3d0834cF79D697127BBB00e5" },
    { name: "USDC/WETH 0.3%",  addr: "0x6c561B446416E1A00E8E93E221854d6eA4171372" },
  ],

  // Aerodrome pools
  aeroPools: [
    { name: "Aero USDC/WETH volatile", addr: "0xcDAC0d6c6C59727a65F871236188350531885C43" },
    { name: "Aero USDC/WETH stable",   addr: "0x3548029694fbB241D45FB24Ba0cd9c9d4E745f16" },
    { name: "Aero USDC/DAI stable",    addr: "0x67b00B46FA4f4F24c03855c5C8013C0B938B3eEc" },
  ],
};

const ERC20_ABI = ["function balanceOf(address) view returns (uint256)", "function decimals() view returns (uint8)"];
const POOL_ABI  = ["function liquidity() view returns (uint128)"];

async function main() {
  const provider = ethers.provider;
  const usdc = new ethers.Contract(USDC, ERC20_ABI, provider);

  console.log("\n══════════════════════════════════════════════════");
  console.log("  CASH MACHINE — PRE-DEPLOYMENT SCAN");
  console.log("══════════════════════════════════════════════════\n");

  // ── 1. Flash loan provider liquidity ─────────────────────────────────────
  console.log("📊 FLASH LOAN PROVIDER USDC LIQUIDITY:");
  let bestProvider = { name: "", bal: 0n };
  for (const p of ADDRESSES.flashProviders) {
    try {
      const bal = await usdc.balanceOf(p.addr);
      const balF = parseFloat(ethers.formatUnits(bal, 6));
      const flag = balF > 100_000 ? "✅" : balF > 1_000 ? "⚠️ " : "❌";
      console.log(`  ${flag} ${p.name.padEnd(20)} ${balF.toLocaleString().padStart(20)} USDC`);
      if (bal > bestProvider.bal) bestProvider = { name: p.name, bal };
    } catch (e) {
      console.log(`  ❌ ${p.name.padEnd(20)} ERROR: ${e.message}`);
    }
  }
  console.log(`\n  🏆 Best provider: ${bestProvider.name} (${parseFloat(ethers.formatUnits(bestProvider.bal, 6)).toLocaleString()} USDC)\n`);

  // ── 2. Router contract existence ──────────────────────────────────────────
  console.log("🔌 ROUTER CONTRACT EXISTENCE:");
  for (const r of ADDRESSES.routers) {
    const code = await provider.getCode(r.addr);
    const exists = code !== "0x";
    console.log(`  ${exists ? "✅" : "❌"} ${r.name.padEnd(20)} ${r.addr}`);
  }

  // ── 3. UniV3 pool liquidity ───────────────────────────────────────────────
  console.log("\n💧 UNISWAP V3 POOL LIQUIDITY:");
  for (const p of ADDRESSES.uniV3Pools) {
    try {
      const pool = new ethers.Contract(p.addr, POOL_ABI, provider);
      const liq = await pool.liquidity();
      const usdcBal = await usdc.balanceOf(p.addr);
      const usdcF = parseFloat(ethers.formatUnits(usdcBal, 6));
      const flag = usdcF > 10_000 ? "✅" : usdcF > 1_000 ? "⚠️ " : "❌";
      console.log(`  ${flag} ${p.name.padEnd(25)} USDC: ${usdcF.toLocaleString().padStart(12)}  liq: ${liq.toString()}`);
    } catch (e) {
      console.log(`  ❌ ${p.name.padEnd(25)} ERROR: ${e.message}`);
    }
  }

  // ── 4. Aerodrome pool liquidity ───────────────────────────────────────────
  console.log("\n🌬️  AERODROME POOL USDC BALANCE:");
  for (const p of ADDRESSES.aeroPools) {
    try {
      const usdcBal = await usdc.balanceOf(p.addr);
      const usdcF = parseFloat(ethers.formatUnits(usdcBal, 6));
      const flag = usdcF > 10_000 ? "✅" : usdcF > 1_000 ? "⚠️ " : "❌";
      console.log(`  ${flag} ${p.name.padEnd(30)} USDC: ${usdcF.toLocaleString().padStart(12)}`);
    } catch (e) {
      console.log(`  ❌ ${p.name.padEnd(30)} ERROR: ${e.message}`);
    }
  }

  // ── 5. Deployer balance check ─────────────────────────────────────────────
  const [deployer] = await ethers.getSigners();
  const ethBal = await provider.getBalance(deployer.address);
  const ethF = parseFloat(ethers.formatEther(ethBal));
  console.log("\n💰 DEPLOYER:");
  console.log(`  Address: ${deployer.address}`);
  console.log(`  ETH bal: ${ethF.toFixed(6)} ETH ${ethF >= 0.01 ? "✅" : "❌ (need at least 0.01 ETH for deploy)"}`);

  // ── 6. Summary ────────────────────────────────────────────────────────────
  console.log("\n══════════════════════════════════════════════════");
  console.log("  RECOMMENDATION:");
  const morphoBal = await usdc.balanceOf("0xBBBBBbbBBb9cC5e90e3b3Af64bdAF62C37EEFFCb");
  const morphoF = parseFloat(ethers.formatUnits(morphoBal, 6));
  if (morphoF > 100_000) {
    console.log("  ✅ Use Morpho Blue for flash loans");
    console.log(`     Available: ${morphoF.toLocaleString()} USDC (FREE — 0% fee)`);
  }
  console.log("══════════════════════════════════════════════════\n");
}

main().catch((err) => { console.error(err); process.exit(1); });
