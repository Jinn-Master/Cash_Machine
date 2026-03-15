# 💸 Money Printer

A multi-bot arbitrage and yield system for Base blockchain. Squeezes profit from
flash-loan arbitrage, new token scalping, dust collection, oracle lag, solver
settlement events, and automated staking — all running from a shared core with
AI-assisted oversight and automated financial reporting.

---

## System Overview

```
money-printer/
├── core/
│   ├── config/      All addresses, DEX IDs, tuning constants
│   ├── state/       Shared in-memory state bus (all bots read/write here)
│   ├── chain/       RPC helpers, DEX quote functions, Maverick V2
│   ├── math/        Pricing math, spread calculations
│   ├── logger/      Structured logging
│   ├── scanner/     DexScreener + factory event watcher
│   └── treasury/    Profit distribution, wallet management
│
├── bots/
│   ├── arb/         Bot 1+2: Flash loan arb across 6 DEXs (UniV3, Aero, BaseSwap,
│   │                         AeroStable, SwapBased, Maverick V2)
│   ├── scalper/     Bot 3: Buy new tokens before 2nd DEX listing
│   ├── dust/        Bot 4: Collect small token balances when gas is cheap
│   ├── oracle/      Bot 5: Oracle lag detection (Chainlink/Pyth vs DEX spot)
│   ├── solver/      Bot 6: Post-settlement arb after CoW/1inch large trades
│   └── staking/     Bot 7: Manage staking pool across Moonwell/Seamless/Aerodrome
│
├── oversight/
│   ├── monitor/     Log watcher, error detection, auto-restart, AI fix proposals
│   ├── ai/          Claude API integration
│   └── reports/     Daily/weekly/monthly email reports
│
├── cmd/
│   ├── printer/     Main entrypoint — starts all bots
│   └── scanner/     Standalone pair discovery tool
│
└── contracts/
    └── ArbitrageExecutor.sol   Flash loan contract (deploy to Base)
```

---

## Prerequisites

### Server (AWS recommended)
- **Instance**: m6id.2xlarge (8 vCPU, 32GB RAM, 2TB NVMe) — ~$250/mo
- **OS**: Ubuntu 22.04 LTS
- **Same VPC as your Base node** for private IP connection

### Base Node (same VPC, separate instance)
- **Instance**: i4i.xlarge or m6id.2xlarge
- **Storage**: 2TB+ NVMe
- Sync time with snapshot: 4–8 hours

### Required Accounts / Services
- [x] SendGrid account (free tier works) — for email reports
- [x] Telegram bot (@BotFather) — for alerts
- [x] Anthropic API key — for AI oversight
- [x] 4 Ethereum wallets — bot, spending, overhead, staking
- [x] Visa card linked to spending + overhead wallets (e.g. Coinbase card)
- [x] GitHub private repository

---

## Step 1: Base Node Setup

```bash
# On your node instance
sudo apt update && sudo apt install -y docker.io docker-compose git

git clone https://github.com/base-org/node
cd node

# Configure
cp .env.mainnet .env
nano .env
# Set:
#   OP_NODE_L1_ETH_RPC=https://eth-mainnet.g.alchemy.com/v2/YOUR_KEY
#   OP_NODE_L1_BEACON=https://eth-beacon-chain.com/YOUR_KEY

# Download snapshot (saves 4+ hours of sync)
# See: https://docs.base.org/chain/snapshots
# Download to ./geth/chaindata/

docker compose up -d

# Monitor sync progress (takes 4–8h with snapshot)
docker compose logs -f node
```

**Open security group** (AWS): allow port 8546 TCP from bot instance private IP only.

---

## Step 2: Deploy ArbitrageExecutor.sol

```bash
# Install Foundry
curl -L https://foundry.paradigm.xyz | bash
foundryup

# Deploy to Base mainnet
cd contracts/

forge create ArbitrageExecutor \
  --rpc-url https://mainnet.base.org \
  --private-key $PRIVATE_KEY \
  --constructor-args \
    0xA238Dd80C259a72e81d7e4664a9801593F98d1c5 \  # Aave pool
    0x2626664c2603336E57B271c5C0b26F421741e481 \  # UniV3 router
    0xcF77a3Ba9A5CA399B7c97c74d54e5b1Beb874E43 \  # Aero router
    0x420DD381b31aEf6683db6B902084cB0FFECe40Da \  # Aero volatile factory
    0x420DD381b31aEf6683db6B902084cB0FFECe40Da \  # Aero stable factory
    0x327Df1E6de05895d2ab08513aaDD9313Fe505d86 \  # BaseSwap router
    0xaaa3b1F1bd7BCc97fD1917c18ADE665C5D31F066 \  # SwapBased router
    5000000                                        # min profit (0.005 USDC)

# Save the deployed contract address — you'll need it in .env
# Example output: Deployed to: 0xYOUR_CONTRACT_ADDRESS
```

Alternatively, deploy via Remix IDE (remix.ethereum.org):
1. Open ArbitrageExecutor.sol, compile with Solidity 0.8.19
2. Connect MetaMask to Base mainnet
3. Deploy with constructor args above
4. Save the address

---

## Step 3: Bot Server Setup

```bash
# On your bot instance
sudo apt update && sudo apt install -y golang-go git make

# Clone your private repo
git clone git@github.com:yourusername/money-printer.git
cd money-printer

# Install Go dependencies
go mod tidy
```

---

## Step 4: Configure Environment

```bash
cp .env.example .env
nano .env
```

```bash
# .env — fill in every value

# ── RPC ──────────────────────────────────────────────────────────────────────
# Use your private node IP (same VPC) for lowest latency
BASE_WS_URL=ws://10.0.1.50:8546          # YOUR Base node private IP
# Fallback public RPC (for scanner only, not bot):
# BASE_WS_URL=wss://base-mainnet.g.alchemy.com/v2/YOUR_KEY

# ── Wallets ───────────────────────────────────────────────────────────────────
PRIVATE_KEY=0xYOUR_BOT_WALLET_PRIVATE_KEY
ARBITRAGE_CONTRACT_ADDRESS=0xYOUR_DEPLOYED_CONTRACT

# Treasury wallets (receive automatic USDC distributions)
SPENDING_WALLET=0xYOUR_SPENDING_WALLET    # Linked to your Visa card
OVERHEAD_WALLET=0xYOUR_OVERHEAD_WALLET   # Server costs Visa card
STAKING_WALLET=0xYOUR_STAKING_WALLET     # Staking pool wallet

# ── Alerts & Reporting ────────────────────────────────────────────────────────
TELEGRAM_BOT_TOKEN=123456:ABC-your-token  # From @BotFather
TELEGRAM_CHAT_ID=your-chat-id            # From @userinfobot
SENDGRID_API_KEY=SG.your-key             # sendgrid.com free tier
REPORT_EMAIL=you@yourmail.com

# ── AI Oversight ──────────────────────────────────────────────────────────────
CLAUDE_API_KEY=sk-ant-your-key           # anthropic.com

# ── Paths ─────────────────────────────────────────────────────────────────────
PROJECT_DIR=/home/ubuntu/money-printer
LOG_DIR=/home/ubuntu/money-printer/logs
```

---

## Step 5: Build

```bash
cd /home/ubuntu/money-printer

# Build all binaries
make build

# Or individually:
go build -o money-printer ./cmd/printer
go build -o arb-scanner  ./cmd/scanner
```

---

## Step 6: Systemd Services

### Main bot service

```bash
sudo nano /etc/systemd/system/money-printer.service
```

```ini
[Unit]
Description=Money Printer — Base Chain Bot System
After=network.target
Wants=network-online.target

[Service]
Type=simple
User=ubuntu
WorkingDirectory=/home/ubuntu/money-printer
EnvironmentFile=/home/ubuntu/money-printer/.env
ExecStart=/home/ubuntu/money-printer/money-printer
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=money-printer

# Safety: restart if it uses > 4GB RAM (memory leak protection)
MemoryMax=4G
MemorySwapMax=0

[Install]
WantedBy=multi-user.target
```

### Enable and start

```bash
sudo systemctl daemon-reload
sudo systemctl enable money-printer
sudo systemctl start money-printer

# Check status
sudo systemctl status money-printer

# View logs live
sudo journalctl -u money-printer -f
```

### Allow oversight monitor to restart the service without sudo password

```bash
sudo visudo
# Add this line:
ubuntu ALL=(ALL) NOPASSWD: /bin/systemctl restart money-printer
```

---

## Step 7: Cron Jobs (pair discovery)

```bash
crontab -e
```

Add these 3 lines:

```bash
# Ultra-new token scan: every hour (catches listings within 60 mins)
0 * * * * /home/ubuntu/money-printer/scan-pairs.sh --ultra-new --patch --restart >> /home/ubuntu/money-printer/logs/scan-ultra.log 2>&1

# New token scan: every 4 hours (48h window)
0 */4 * * * /home/ubuntu/money-printer/scan-pairs.sh --new-only --patch --restart >> /home/ubuntu/money-printer/logs/scan-new.log 2>&1

# Daily full scan: 6am UTC (refresh full pair list)
0 6 * * * /home/ubuntu/money-printer/scan-pairs.sh --patch --restart >> /home/ubuntu/money-printer/logs/scan-daily.log 2>&1
```

---

## Step 8: Telegram Bot Setup (5 minutes)

1. Open Telegram, message `@BotFather`
2. Send `/newbot` → follow prompts → copy the token
3. Message `@userinfobot` → copy your chat ID
4. Add both to `.env`
5. Test: `sudo systemctl restart money-printer` — you should get a startup message

---

## Step 9: Email Reports Setup (SendGrid)

1. Create free account at sendgrid.com
2. Settings → API Keys → Create API Key → copy it
3. Add to `.env` as `SENDGRID_API_KEY`
4. Add your email as `REPORT_EMAIL`
5. Reports send at 8am UTC daily, Sundays weekly, 1st of month monthly

---

## Step 10: Visa Card Setup

Connect your spending and overhead wallets to a crypto Visa card:

**Options:**
- **Coinbase Card** (US) — link your Coinbase account, fund from wallet
- **Crypto.com Visa** — direct wallet link, cashback on spending
- **Gnosis Pay** (EU) — direct on-chain Visa, no intermediary

The treasury bot sends USDC to these wallets automatically on each 6-hour distribution cycle.

---

## Treasury Distribution

### Phase 1 (until $100k working capital)
| Destination | % | Purpose |
|---|---|---|
| Bot wallet (reinvest) | 50% | Grows working capital, reduces flash loan need |
| Spending wallet (Visa) | 25% | Your personal spending |
| Overhead wallet (Visa) | 15% | Server costs ($250/mo node + $50/mo monitoring) |
| Staking pool | 10% | Emergency fund + yield |

### Phase 2 (after $100k working capital)
| Destination | % | Purpose |
|---|---|---|
| Bot wallet (reinvest) | 40% | Maintain working capital |
| Spending wallet | 30% | More spending allocation |
| Overhead wallet | 20% | Expanded infrastructure |
| Staking pool | 10% | Continued emergency fund growth |

### Working Capital Effect
Once the bot wallet holds $100k+ USDC:
- Flash loan requirement drops (borrow less, pay less 0.05% fee per trade)
- Can self-fund smaller trades entirely (zero flash loan fee)
- Staking pool becomes meaningful emergency fund ($10k+)

---

## AI Oversight System

### What Claude does automatically:
- Monitors log files every 2 seconds for ERROR/CRITICAL patterns
- Sends immediate Telegram alert on any error
- Auto-restarts the service on critical failures (no code change)
- After 2+ crashes: reads logs, diagnoses root cause, proposes fix

### What requires YOUR approval:
- Any code change (you receive email with proposed diff)
- Major parameter changes (thresholds, trade sizes)
- New pair additions beyond what the scanner auto-detects

### Approving a fix:
Reply to the oversight email with "APPROVE" — the system will:
1. Apply the diff to the source file
2. Rebuild the binary
3. Restart the service
4. Send you confirmation

### To reject:
Reply with "REJECT" — no changes made, system continues as-is.

---

## 7 Bots — What Each Does

| Bot | Strategy | Risk Level | Expected Contribution |
|---|---|---|---|
| Arb (flash loan) | 6-DEX spread arbitrage | Low | 50-60% of profit |
| Scalper | New token pre-listing buy | Medium | 10-20% of profit |
| Dust | Small balance conversion | Very Low | 1-3% of profit |
| Oracle Lag | Chainlink vs spot price gap | Low-Medium | 3-8% (Phase 2) |
| Solver | Post-CoW/1inch settlement arb | Low | 3-8% of profit |
| LP-Migrator | Burn+Mint migration detection | Low-Medium | 10-20% of profit |
| RWA Oracle Lag | NAV vs DEX price for T-bills/RWAs | Low | 5-15% of profit |
| Staking | Yield on idle USDC | Very Low | Passive yield |
| Factory Watcher | Feeds all bots with new pairs | N/A — infrastructure | Enables scalper + arb |

---


## LP-Migrator Bot

Watches all V2 DEX factories simultaneously for large `Burn` events (LP removal).
When a burn removes > $10k of liquidity, it registers as a "pending migration."

If a `Mint` event for the same token appears on a *different* DEX within 20 blocks (~40s),
the migration is confirmed and an `LPMigrationEvent` is broadcast to all bots.

**Why this is profitable:**
- The burned DEX now has very thin liquidity — any trade there has enormous slippage
- The new DEX has fresh liquidity priced at wherever the migrator set it
- The gap between the two prices is the arb window — typically 4-20 seconds before
  other bots discover the new pool

**Pre-signal:** The burn alone (before the mint) is already actionable. The arb bot
starts polling the token immediately, and the scalper bot puts it on watchlist.
This gives you a 4-40 second head start on the confirmed migration.

**False positives:** Large burns that aren't migrations (just LP withdrawal) do NOT
produce a correlating Mint on another DEX, so they expire from the pending map after
60 seconds with no action taken.

## RWA Oracle Lag Bot

Monitors tokenized real-world assets on Base for price discrepancies between:
- **NAV Price**: the actual underlying asset value (fetched from Backed Finance API, Ondo API, FRED)
- **DEX Price**: what the token trades for on Base DEXs (Aerodrome, UniV3)

**Monitored assets:**
- `bIB01` (Backed Finance): tokenized 0-3 month US T-bill ETF — NAV updates daily at 4pm ET
- `OUSG` (Ondo Finance): tokenized BlackRock Treasury fund — daily NAV update
- `cbBTC` vs `WBTC`: peg deviation monitoring

**Predictive mode:** When the Fed Funds Rate changes (fetched from FRED — free, no key
needed), the bot calculates the implied change to T-bill NAV before Backed Finance
updates their oracle. This gives a 1-24 hour predictive edge on bIB01 pricing.

**Trade direction:**
- NAV > DEX price: buy on DEX, hold until oracle catches up (LONG_ASSET)
- NAV < DEX price: borrow on Aave at stale price, sell on DEX (SHORT_ASSET — higher threshold)

## Maverick V2 Integration

Maverick V2 uses directional liquidity bins — a fundamentally different AMM design where
LPs can add liquidity that only provides depth in one price direction. This creates:

- **Bin drift**: when price moves, the active bin shifts, leaving thin liquidity on the other side
- **Persistent mispricings**: the bin structure takes time to rebalance vs standard AMMs
- **Unique spread combinations**: Maverick V2 ↔ Aerodrome is a spread pair that few bots watch

The Maverick V2 quoter uses `calculateSwap(pool, tokenAIn, amountIn)` — a view call, no gas.

To add Maverick V2 pairs to the arb bot, the scanner will automatically detect them when
a Maverick V2 pool is created (watched via factory events).

---

## Monitoring

### Live log view
```bash
sudo journalctl -u money-printer -f
```

### Check all bot status
```bash
# Summary of recent activity
sudo journalctl -u money-printer --since "1 hour ago" | grep -E "(OPPORTUNITY|confirmed|ERROR|hot-loading)"
```

### Manual scan
```bash
# Quick scan (no RPC)
make scan-ultra

# Full scan with live quotes
make scan-ultra-live
```

### Check working capital
```bash
# USDC balance of bot wallet
cast call 0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913 \
  "balanceOf(address)(uint256)" \
  YOUR_BOT_WALLET \
  --rpc-url https://mainnet.base.org
```

---

## Tuning Guide

After 2 weeks of live data, review these settings in `core/config/config.go`:

| Parameter | Default | Lower if... | Raise if... |
|---|---|---|---|
| `MinProfitPct` | 0.55% | Execution rate < 1/hour | Revert rate > 30% |
| `CooldownSeconds` | 20s | Missing opportunities on same pair | Same pair executing twice at loss |
| `LowCapPollInterval` | 15s | Slow reaction on new pairs | RPC rate limits hit |
| `HotPairPollInterval` | 5s | Missing hot pair windows | Too many quote calls |
| `ScalperMaxPositionUSDC` | $100 | New tokens frequently rug | Honeypot filters working well |
| `DustGasPriceMaxGwei` | 0.05 | Dust never collects | Gas costs eating dust value |

---

## Updating & Deploying Changes

```bash
# After making code changes:
cd /home/ubuntu/money-printer
git pull
make build
sudo systemctl restart money-printer

# The AI oversight system does this automatically when you approve a fix
```

---

## Emergency Procedures

### Bot is draining wallet (runaway execution)
```bash
sudo systemctl stop money-printer
# Review logs:
sudo journalctl -u money-printer --since "30 minutes ago"
# Fix config or code, then:
sudo systemctl start money-printer
```

### Contract got exploited
```bash
# Call owner-only withdraw function:
cast send YOUR_CONTRACT_ADDRESS "withdraw(address,uint256)" \
  YOUR_SAFE_WALLET 0xFFFFFFFF \
  --private-key $PRIVATE_KEY \
  --rpc-url https://mainnet.base.org
```

### Staking emergency withdrawal
```bash
# Oversight monitor calls EmergencyWithdraw automatically on critical alerts
# Manual trigger:
# (implement in staking bot as a CLI flag)
```

---

## File Checklist

All files you need before deploying:

```
money-printer/
├── core/config/config.go           ✅ All addresses + tuning
├── core/state/state.go             ✅ Shared state bus
├── core/chain/maverick.go          ✅ Maverick V2 quotes
├── core/chain/chain.go             ✅ RPC helpers (from arb-watcher-base)
├── core/math/math.go               ✅ Pricing math (from arb-watcher-base)
├── core/logger/logger.go           ✅ Logger (from arb-watcher-base)
├── core/treasury/treasury.go       ✅ Profit distribution
├── bots/arb/arb.go                 ✅ Flash loan arb (from arb-watcher-base)
├── bots/arb/factory_watcher.go     ✅ Live factory event watcher
├── bots/scalper/scalper.go         ✅ New token scalper + honeypot detection
├── bots/dust/dust.go               ✅ Dust collector
├── bots/oracle/oracle.go           ✅ Oracle lag detection
├── bots/solver/solver.go           ✅ Solver settling event monitor
├── bots/staking/staking.go         ✅ Staking pool management
├── bots/lpmigrator/lpmigrator.go   ✅ LP migration Burn+Mint detector
├── bots/rwa/rwa.go                 ✅ RWA NAV vs DEX oracle lag bot
├── oversight/monitor/monitor.go    ✅ AI oversight + reports
├── cmd/printer/main.go             ✅ Master entrypoint
├── cmd/scanner/main.go             ✅ Pair discovery scanner
├── contracts/ArbitrageExecutor.sol ✅ Flash loan contract
├── scan-pairs.sh                   ✅ 3-tier cron scanner script
├── Makefile                        ✅ Build targets
├── go.mod                          ✅ Go module
└── .env.example                    ✅ Environment template
```

### Files to copy from arb-watcher-base (not duplicated above):
```
core/chain/chain.go      ← internal/chain/chain.go
core/math/math.go        ← internal/math/math.go
core/logger/logger.go    ← internal/logger/logger.go
bots/arb/arb.go          ← internal/arb/arb.go
bots/arb/factory_watcher.go ← internal/arb/factory_watcher.go
contracts/ArbitrageExecutor.sol ← ArbitrageExecutor.sol
```

---

## Expected Performance

| Scenario | Monthly Estimate |
|---|---|
| Quiet market, tuning phase | $200–$500 |
| Normal market, calibrated | $1,000–$3,000 |
| Volatile / new token launches | $3,000–$10,000 |
| Bull market sustained | $2,000–$6,000/mo avg |

The biggest variable is pair discovery quality — the scanner and factory watcher
together give you the earliest possible detection, but the human judgment of
which detected tokens to keep vs drop remains the top performance driver.

---

## GitHub Setup

```bash
# Initialize and push to your private repo
cd /home/ubuntu/money-printer
git init
git add .
git commit -m "Initial Money Printer system"
git remote add origin git@github.com:yourusername/money-printer.git
git push -u origin main
```

**Important**: Never commit `.env` — it's in `.gitignore`.

```bash
# .gitignore
.env
*.log
money-printer
arb-scanner
logs/
```

---

*Money Printer v1.0 — Base Chain Multi-Bot System*
*Built for maximum extraction with minimum oversight friction.*
