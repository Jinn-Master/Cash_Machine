#!/usr/bin/env bash
# =============================================================================
#  Money Printer — Final Build & Deploy Script
#  Run on your t3.small after copying all files into place
#  Usage: bash build-and-deploy.sh
# =============================================================================
set -euo pipefail

REPO="/home/ec2-user/money-printer"
export PATH="/usr/local/go/bin:$HOME/.cargo/bin:$PATH"

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
log()  { echo -e "${GREEN}[+]${NC} $*"; }
warn() { echo -e "${YELLOW}[!]${NC} $*"; }
die()  { echo -e "${RED}[✗]${NC} $*"; exit 1; }

cd "$REPO"

# ── 1. Check .env exists and keys are set ────────────────────────────────────
[[ -f .env ]] || die ".env not found — copy .env.example and fill in your keys"
grep -q "YOUR_KEY_HERE" .env && die "Still has placeholder keys in .env — fill them in first"

# ── 2. Fix any remaining old module paths ────────────────────────────────────
log "Checking for stale import paths..."
OLD_PATHS=$(grep -r "arb-watcher-base\|arb-watcher/internal" --include="*.go" -l 2>/dev/null || true)
if [[ -n "$OLD_PATHS" ]]; then
  warn "Found stale import paths in: $OLD_PATHS"
  find . -name "*.go" -exec sed -i \
    's|yourname/arb-watcher-base/internal|yourname/money-printer/core|g' {} +
  find . -name "*.go" -exec sed -i \
    's|yourname/arb-watcher/internal|yourname/money-printer/core|g' {} +
  log "Fixed."
fi

# ── 3. Tidy dependencies ──────────────────────────────────────────────────────
log "Running go mod tidy..."
go mod tidy

# ── 4. Build ──────────────────────────────────────────────────────────────────
log "Building money-printer (main bot)..."
go build -o money-printer ./cmd/printer/
log "Building arb-bot..."
go build -o arb-bot ./cmd/arb/

# ── 5. Sudoers for monitor auto-restart ──────────────────────────────────────
log "Configuring sudoers for monitor auto-restart..."
echo "ec2-user ALL=(ALL) NOPASSWD: /bin/systemctl restart money-printer" \
  | sudo tee /etc/sudoers.d/money-printer > /dev/null
echo "ec2-user ALL=(ALL) NOPASSWD: /bin/systemctl restart arb-bot" \
  | sudo tee -a /etc/sudoers.d/money-printer > /dev/null
sudo chmod 440 /etc/sudoers.d/money-printer

# ── 6. Systemd service — money-printer ───────────────────────────────────────
log "Writing money-printer.service..."
sudo tee /etc/systemd/system/money-printer.service > /dev/null << EOF
[Unit]
Description=Money Printer — Base Chain Bot System
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=ec2-user
WorkingDirectory=$REPO
EnvironmentFile=$REPO/.env
ExecStart=$REPO/money-printer
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=money-printer
MemoryMax=1500M
MemorySwapMax=0

[Install]
WantedBy=multi-user.target
EOF

# ── 7. Systemd service — arb-bot ─────────────────────────────────────────────
log "Writing arb-bot.service..."
sudo tee /etc/systemd/system/arb-bot.service > /dev/null << EOF
[Unit]
Description=Money Printer — Arb Bot
After=network-online.target money-printer.service
Wants=network-online.target

[Service]
Type=simple
User=ec2-user
WorkingDirectory=$REPO
EnvironmentFile=$REPO/.env
ExecStart=$REPO/arb-bot
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=arb-bot
MemoryMax=500M
MemorySwapMax=0

[Install]
WantedBy=multi-user.target
EOF

# ── 8. Enable and start ───────────────────────────────────────────────────────
log "Enabling and starting services..."
sudo systemctl daemon-reload
sudo systemctl enable money-printer arb-bot
sudo systemctl restart money-printer
sleep 3
sudo systemctl restart arb-bot

# ── 9. Status check ───────────────────────────────────────────────────────────
echo ""
log "✅ Deployed! Status:"
sudo systemctl status money-printer --no-pager -l | tail -5
sudo systemctl status arb-bot --no-pager -l | tail -5

echo ""
log "Monitor with:"
echo "  journalctl -fu money-printer"
echo "  journalctl -fu arb-bot"
