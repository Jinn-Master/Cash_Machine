package main

// cmd/printer/main.go
//
// Money Printer — Master Entrypoint
//
// Starts all bots in the correct order, wires shared state, and provides
// graceful shutdown on SIGINT/SIGTERM.
//
// Bot startup order:
//   1. Shared state (in-memory, no startup needed — already initialised)
//   2. Chain client connection + chain ID verification
//   3. Gas price monitor (feeds state.Global.SetGasPrice)
//   4. Factory watcher (begins detecting new tokens immediately)
//   5. Arb bot — Tier1 (WebSocket event-driven), Tier2 (polled), hot pairs
//   6. Scalper bot
//   7. Dust collector
//   8. Oracle lag bot
//   9. Solver event monitor
//  10. Staking bot
//  11. Treasury
//  12. Oversight monitor (last — monitors everything above)

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/joho/godotenv"

	"github.com/Jinn-Master/Cash_Machine/bots/dust"
	"github.com/Jinn-Master/Cash_Machine/bots/lpmigrator"
	"github.com/Jinn-Master/Cash_Machine/bots/oracle"
	"github.com/Jinn-Master/Cash_Machine/bots/rwa"
	"github.com/Jinn-Master/Cash_Machine/bots/scalper"
	"github.com/Jinn-Master/Cash_Machine/bots/solver"
	"github.com/Jinn-Master/Cash_Machine/bots/staking"
	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/health"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
	"github.com/Jinn-Master/Cash_Machine/core/state"
	"github.com/Jinn-Master/Cash_Machine/core/treasury"
	"github.com/Jinn-Master/Cash_Machine/oversight/monitor"
)

func main() {
	logger.Init("money_printer.log")
	log := logger.Log

	log.Info("💸 ══════════════════════════════════════════")
	log.Info("💸   MONEY PRINTER — Starting up")
	log.Info("💸   Base Chain Arbitrage System v1.0")
	log.Info("💸 ══════════════════════════════════════════")

	// ── Load environment ──────────────────────────────────────────────────────
	if err := godotenv.Load(); err != nil {
		log.Warn(".env not found, using environment variables")
	}

	required := map[string]string{
		"BASE_WS_URL":                  os.Getenv("BASE_WS_URL"),
		"ARBITRAGE_CONTRACT_ADDRESS":   os.Getenv("ARBITRAGE_CONTRACT_ADDRESS"),
		"PRIVATE_KEY":                  os.Getenv("PRIVATE_KEY"),
		"SPENDING_WALLET":              os.Getenv("SPENDING_WALLET"),
		"OVERHEAD_WALLET":              os.Getenv("OVERHEAD_WALLET"),
		"STAKING_WALLET":               os.Getenv("STAKING_WALLET"),
	}
	for k, v := range required {
		if v == "" {
			log.Error("missing required env var", "key", k)
			os.Exit(1)
		}
	}

	// Optional — system works without these but features are degraded
	telegramToken  := os.Getenv("TELEGRAM_BOT_TOKEN")
	telegramChat   := os.Getenv("TELEGRAM_CHAT_ID")
	sendgridKey    := os.Getenv("SENDGRID_API_KEY")
	reportEmail    := os.Getenv("REPORT_EMAIL")
	claudeAPIKey   := os.Getenv("CLAUDE_API_KEY")
	projectDir     := os.Getenv("PROJECT_DIR")
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}
	logDir := os.Getenv("LOG_DIR")
	if logDir == "" {
		logDir = projectDir + "/logs"
	}
	os.MkdirAll(logDir, 0755)

	// ── Private key ───────────────────────────────────────────────────────────
	privKeyHex := os.Getenv("PRIVATE_KEY")
	if len(privKeyHex) >= 2 && privKeyHex[:2] == "0x" {
		privKeyHex = privKeyHex[2:]
	}
	privKey, err := crypto.HexToECDSA(privKeyHex)
	if err != nil {
		log.Error("invalid PRIVATE_KEY", "err", err)
		os.Exit(1)
	}
	botWallet := crypto.PubkeyToAddress(privKey.PublicKey)

	// ── Chain client ──────────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := ethclient.DialContext(ctx, os.Getenv("BASE_WS_URL"))
	if err != nil {
		log.Error("RPC connect failed", "err", err)
		os.Exit(1)
	}
	defer client.Close()

	chainID, err := client.ChainID(ctx)
	if err != nil {
		log.Error("chainID fetch failed", "err", err)
		os.Exit(1)
	}
	if chainID.Cmp(big.NewInt(config.BaseChainID)) != 0 {
		log.Error("wrong chain — expected Base mainnet 8453", "got", chainID)
		os.Exit(1)
	}

	bal, _ := client.BalanceAt(ctx, botWallet, nil)
	ethBal := new(big.Float).Quo(new(big.Float).SetInt(bal), new(big.Float).SetFloat64(1e18))
	ethBalFloat, _ := ethBal.Float64()

	log.Info("✅ connected to Base mainnet",
		"wallet",  botWallet.Hex(),
		"eth_bal", fmt.Sprintf("%.6f ETH", ethBalFloat),
	)

	if ethBalFloat < 0.01 {
		log.Warn("⚠️  low ETH balance — may not cover gas", "balance", ethBalFloat)
	}

	// ── Gas price monitor ─────────────────────────────────────────────────────
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				gasPrice, err := client.SuggestGasPrice(ctx)
				if err == nil {
					gwei := float64(gasPrice.Uint64()) / 1e9
					state.Global.SetGasPrice(gwei)
				}
			}
		}
	}()

	// ── Token price updater ───────────────────────────────────────────────────
	// Fetches real WETH/USD price from Uniswap V3 slot0 every 30 seconds.
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				price, err := chain.FetchWETHPrice(ctx, client)
				if err != nil {
					log.Warn("WETH price fetch failed", "err", err)
					continue
				}
				state.Global.SetPrice(
					common.HexToAddress(config.AddrWETH),
					state.PriceUpdate{
						TokenAddr: common.HexToAddress(config.AddrWETH),
						TokenSym:  "WETH",
						PriceUSD:  price,
						Source:    "uniV3-slot0",
						UpdatedAt: time.Now(),
					},
				)
			}
		}
	}()

	// ── Hot token pruner (hourly cleanup) ─────────────────────────────────────
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pruned := state.Global.PruneHotTokens(24 * time.Hour)
				if pruned > 0 {
					log.Debug("pruned stale hot tokens", "count", pruned)
				}
			}
		}
	}()

	log.Info("🚀 starting all bots...")

	// ── Bot 3: Dust collector ─────────────────────────────────────────────────
	dustBot := dust.New(client, botWallet, privKey)
	go dustBot.Run(ctx)
	log.Info("✅ Bot 3: Dust Collector — started")

	// ── Bot 4: Oracle lag ─────────────────────────────────────────────────────
	oracleBot := oracle.New(client)
	go oracleBot.Run(ctx)
	log.Info("✅ Bot 4: Oracle Lag Monitor — started")

	// ── Bot 5: Solver events ──────────────────────────────────────────────────
	solverBot := solver.New(client)
	go solverBot.Run(ctx)
	log.Info("✅ Bot 5: Solver Event Monitor — started")

	// ── Bot 6a: LP-Migrator ──────────────────────────────────────────────────
	lpMigBot, err := lpmigrator.New(client)
	if err != nil {
		log.Error("failed to create LP-Migrator bot", "err", err)
		os.Exit(1)
	}
	go lpMigBot.Run(ctx)
	log.Info("✅ Bot 6a: LP-Migrator — started")

	// ── Bot 6b: RWA Oracle Lag ────────────────────────────────────────────────
	rwaBot := rwa.New(client)
	go rwaBot.Run(ctx)
	log.Info("✅ Bot 6b: RWA Oracle Lag — started")

	// ── Bot 6: Scalper ────────────────────────────────────────────────────────
	scalperBot := scalper.New(client, privKey, botWallet, chainID)
	go scalperBot.Run(ctx)
	log.Info("✅ Bot 6: Scalper — started")

	// ── Bot 7: Staking ────────────────────────────────────────────────────────
	stakingWallet := common.HexToAddress(os.Getenv("STAKING_WALLET"))
	stakingBot := staking.New(client, stakingWallet)
	go stakingBot.Run(ctx)
	log.Info("✅ Bot 7: Staking — started")

	// ── Treasury ──────────────────────────────────────────────────────────────
	spendingWallet := common.HexToAddress(os.Getenv("SPENDING_WALLET"))
	overheadWallet := common.HexToAddress(os.Getenv("OVERHEAD_WALLET"))
	stakingWallet  := common.HexToAddress(os.Getenv("STAKING_WALLET"))
	treas := treasury.New(
		client, privKey, botWallet,
		spendingWallet,
		overheadWallet,
		stakingWallet,
		chainID,
	)
	go treas.Run(ctx)
	log.Info("✅ Treasury — started")

	// ── Health check endpoint ────────────────────────────────────────────────
	healthServer := health.New(client, botWallet.Hex(), 8080)
	healthServer.Start(ctx)
	log.Info("✅ HealthCheck — :8080/health :8080/ready")

	// ── Oversight monitor (last) ──────────────────────────────────────────────
	mon := monitor.New(monitor.Config{
		LogDir:         logDir,
		ProjectDir:     projectDir,
		TelegramToken:  telegramToken,
		TelegramChatID: telegramChat,
		SendGridKey:    sendgridKey,
		ReportEmail:    reportEmail,
		ClaudeAPIKey:   claudeAPIKey,
		ServiceName:    "money-printer",
	}, treas)
	go mon.Run(ctx)
	log.Info("✅ Oversight Monitor — started")

	log.Info("💸 ══════════════════════════════════════════")
	log.Info("💸   All systems GO — Money Printer is live")
	log.Info("💸   Bots: Arb | Scalper | Dust | Oracle")
	log.Info("💸         Solver | Staking | Treasury")
	log.Info("💸         LP-Migrator | RWA Oracle Lag")
	log.Info("💸   Oversight: Monitor | AI | Reports")
	log.Info("💸 ══════════════════════════════════════════")

	// ── NOTE: Arb bot (Bot 1+2) is in the separate arb-watcher binary ─────────
	// The arb bot runs as its own systemd service and shares data via state.Global
	// in-process. In a production multi-binary deployment, state would be shared
	// via Redis or a local Unix socket. For single-binary deployment, import
	// the arb bot package here.
	//
	// See README.md → "Single vs Multi-Binary Deployment" for details.

	<-ctx.Done()
	log.Info("💸 Money Printer shutting down gracefully...")
}
