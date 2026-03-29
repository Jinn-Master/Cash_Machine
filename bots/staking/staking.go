package staking

// bots/staking/staking.go
//
// Staking Bot
//
// Manages the staking pool — the emergency fund / yield generator.
//
// Responsibilities:
//   1. Receive USDC deposits from treasury (when staking allocation >= $10)
//   2. Allocate across best-yielding protocols (Moonwell, Seamless, Aerodrome veAERO)
//   3. Monitor yields — rebalance weekly or when yield differential > 1%
//   4. Track total staked value and report to shared state
//   5. Emergency: on critical alert, can liquidate staking positions to refund bot wallet
//   6. Claude API monitors and optimises staking protocol selection
//
// Current protocols on Base:
//   - Moonwell USDC: ~6% APY, very liquid (best for emergency fund)
//   - Seamless USDC: ~5.5% APY, liquid
//   - Aerodrome veAERO: ~15% APY but illiquid (4-year lock) — only for surplus

import (
	"context"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
	"github.com/Jinn-Master/Cash_Machine/core/state"
)

type StakingBot struct {
	client        *ethclient.Client
	stakingWallet common.Address

	// Track balances per protocol
	balances map[string]float64 // protocol name → USD value
	totalStakedUSD float64
}

func New(client *ethclient.Client, stakingWallet common.Address) *StakingBot {
	return &StakingBot{
		client:        client,
		stakingWallet: stakingWallet,
		balances:      make(map[string]float64),
	}
}

func (s *StakingBot) Run(ctx context.Context) {
	log := logger.Log
	log.Info("🏦 staking bot started", "wallet", s.stakingWallet.Hex())

	// Rebalance check every hour
	rebalanceTicker := time.NewTicker(1 * time.Hour)
	defer rebalanceTicker.Stop()

	// Daily yield harvest
	harvestTicker := time.NewTicker(24 * time.Hour)
	defer harvestTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-rebalanceTicker.C:
			s.checkRebalance(ctx)

		case <-harvestTicker.C:
			s.harvestYield(ctx)
		}
	}
}

// Deposit handles a new USDC deposit into the staking pool.
// Called by treasury when staking allocation is ready.
func (s *StakingBot) Deposit(ctx context.Context, amountUSD float64) error {
	log := logger.Log
	log.Info("🏦 staking: new deposit received",
		"amount_usd", fmt.Sprintf("$%.4f", amountUSD),
	)

	// Strategy: prefer liquid protocols until total staked > $10k
	// After $10k: can diversify into higher-yield but less liquid options
	protocol := s.selectBestProtocol(amountUSD)

	log.Info("🏦 staking: depositing",
		"protocol", protocol.Name,
		"apy_hint", fmt.Sprintf("%.1f%%", protocol.APYHint),
	)

	// TODO: execute actual deposit via protocol-specific ABI
	// This requires implementing depositUSDC(amount) for each protocol

	s.balances[protocol.Name] += amountUSD
	s.totalStakedUSD += amountUSD

	return nil
}

// selectBestProtocol picks the best protocol for a given deposit size.
func (s *StakingBot) selectBestProtocol(amountUSD float64) config.StakingProtocol {
	// Emergency fund principle: keep 80%+ in liquid protocols
	// Liquid = can withdraw in same transaction
	liquidStaked := s.balances["Moonwell USDC"] + s.balances["Seamless USDC"]
	totalStaked := s.totalStakedUSD

	liquidRatio := 0.0
	if totalStaked > 0 {
		liquidRatio = liquidStaked / totalStaked
	}

	// If liquid ratio < 80%, prioritise liquid protocols
	if liquidRatio < 0.80 {
		for _, p := range config.StakingProtocols {
			if p.Name == "Moonwell USDC" {
				return p
			}
		}
	}

	// Otherwise: pick highest APY available
	best := config.StakingProtocols[0]
	for _, p := range config.StakingProtocols {
		if p.APYHint > best.APYHint {
			best = p
		}
	}
	return best
}

func (s *StakingBot) checkRebalance(ctx context.Context) {
	log := logger.Log

	if s.totalStakedUSD < config.StakingMinDeposit {
		return
	}

	// Check if any protocol's APY has shifted significantly
	// (Real implementation: fetch live APY from protocol contracts)
	log.Debug("staking: rebalance check",
		"total_staked", fmt.Sprintf("$%.2f", s.totalStakedUSD),
	)

	// Update working capital context
	state.Global.Alert("warn", "staking",
		fmt.Sprintf("Staking pool: $%.2f", s.totalStakedUSD),
		"Rebalance check complete",
	)
}

func (s *StakingBot) harvestYield(ctx context.Context) {
	log := logger.Log
	// Estimate yield since last harvest
	// Real: call protocol reward contracts and claim
	log.Info("🌾 staking: harvesting yield",
		"total_staked", fmt.Sprintf("$%.2f", s.totalStakedUSD),
	)
}

// EmergencyWithdraw liquidates staking positions and sends funds to bot wallet.
// Called by monitor on critical failure.
func (s *StakingBot) EmergencyWithdraw(ctx context.Context, targetWallet common.Address) error {
	log := logger.Log
	log.Warn("🚨 EMERGENCY WITHDRAW triggered",
		"from", s.stakingWallet.Hex(),
		"to",   targetWallet.Hex(),
		"total_staked", fmt.Sprintf("$%.2f", s.totalStakedUSD),
	)
	// TODO: implement emergency withdrawal from each protocol
	return nil
}
