# =============================================================================
#  Cash Machine — Makefile
#  Usage: make <target>
# =============================================================================

BINARY_PRINTER := money-printer
BINARY_ARB     := arb-bot
MODULE         := github.com/Jinn-Master/Cash_Machine

.PHONY: all build build-printer build-arb clean tidy test \
        deploy-base deploy-base-sep deploy-arb deploy-arb-sep \
        compile verify-base verify-arb install-hardhat run lint

# ── Go binaries ───────────────────────────────────────────────────────────────

all: tidy build

build: build-printer build-arb

build-printer:
	@echo "Building money-printer..."
	go build -o $(BINARY_PRINTER) ./cmd/printer/
	@echo "✅ $(BINARY_PRINTER) ready"

build-arb:
	@echo "Building arb-bot..."
	go build -o $(BINARY_ARB) ./cmd/arb/
	@echo "✅ $(BINARY_ARB) ready"

tidy:
	go mod tidy

clean:
	rm -f $(BINARY_PRINTER) $(BINARY_ARB)
	rm -rf artifacts cache

test:
	go test ./... -v

lint:
	golangci-lint run ./...

# ── Solidity / Hardhat ────────────────────────────────────────────────────────

install-hardhat:
	npm install

compile:
	npx hardhat compile

deploy-base:
	npx hardhat run scripts/deploy_base.js --network base

deploy-base-sep:
	npx hardhat run scripts/deploy_base.js --network baseSepolia

deploy-arb:
	npx hardhat run scripts/deploy_arbitrum.js --network arbitrum

deploy-arb-sep:
	npx hardhat run scripts/deploy_arbitrum.js --network arbitrumSepolia

verify-base:
	@echo "Run: npx hardhat verify --network base <CONTRACT_ADDRESS> <CONSTRUCTOR_ARGS>"

verify-arb:
	@echo "Run: npx hardhat verify --network arbitrum <CONTRACT_ADDRESS> <CONSTRUCTOR_ARGS>"

# ── Run (requires .env) ───────────────────────────────────────────────────────

run: build
	./$(BINARY_PRINTER) &
	./$(BINARY_ARB) &
	@echo "✅ Both bots running. Use 'make stop' to stop."

stop:
	pkill -f $(BINARY_PRINTER) || true
	pkill -f $(BINARY_ARB) || true

# ── Help ──────────────────────────────────────────────────────────────────────

help:
	@echo ""
	@echo "  make build          — Build both Go binaries"
	@echo "  make compile        — Compile Solidity contracts (Hardhat)"
	@echo "  make deploy-base    — Deploy contract to Base mainnet"
	@echo "  make deploy-arb     — Deploy contract to Arbitrum One"
	@echo "  make deploy-base-sep — Deploy to Base Sepolia (testnet)"
	@echo "  make deploy-arb-sep  — Deploy to Arbitrum Sepolia (testnet)"
	@echo "  make run            — Build and run both bots"
	@echo "  make stop           — Kill running bots"
	@echo "  make tidy           — go mod tidy"
	@echo "  make clean          — Remove built binaries"
	@echo ""
