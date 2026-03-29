package treasury

// core/treasury/treasury.go

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/Jinn-Master/Cash_Machine/core/config"
	"github.com/Jinn-Master/Cash_Machine/core/logger"
	"github.com/Jinn-Master/Cash_Machine/core/state"
)

type WalletRole string

const (
	WalletBot      WalletRole = "bot"
	WalletSpending WalletRole = "spending"
	WalletOverhead WalletRole = "overhead"
	WalletStaking  WalletRole = "staking"
)

type Distribution struct {
	ProfitUSD    float64
	BotPct       int
	SpendingPct  int
	OverheadPct  int
	StakingPct   int
	BotAmt       float64
	SpendingAmt  float64
	OverheadAmt  float64
	StakingAmt   float64
	ProcessedAt  time.Time
	Phase        int
}

type Treasury struct {
	client         *ethclient.Client
	privKey        *ecdsa.PrivateKey
	botWallet      common.Address
	spendingWallet common.Address
	overheadWallet common.Address
	stakingWallet  common.Address
	chainID        *big.Int

	mu               sync.Mutex
	pendingUSD       float64
	totalDistributed float64
	distributions    []Distribution
	phase            int
}

func New(
	client *ethclient.Client,
	privKey *ecdsa.PrivateKey,
	botWallet common.Address,
	spendingWallet common.Address,
	overheadWallet common.Address,
	stakingWallet common.Address,
	chainID *big.Int,
) *Treasury {
	return &Treasury{
		client:         client,
		privKey:        privKey,
		botWallet:      botWallet,
		spendingWallet: spendingWallet,
		overheadWallet: overheadWallet,
		stakingWallet:  stakingWallet,
		chainID:        chainID,
		phase:          1,
	}
}

func (t *Treasury) Run(ctx context.Context) {
	log := logger.Log
	log.Info("💰 treasury started",
		"bot_wallet",      t.botWallet.Hex(),
		"spending_wallet", t.spendingWallet.Hex(),
		"overhead_wallet", t.overheadWallet.Hex(),
		"staking_wallet",  t.stakingWallet.Hex(),
	)

	distributeTicker := time.NewTicker(6 * time.Hour)
	defer distributeTicker.Stop()

	wcTicker := time.NewTicker(1 * time.Hour)
	defer wcTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case ev := <-state.Global.ProfitEventCh:
			t.mu.Lock()
			t.pendingUSD += ev.ProfitUSD
			t.mu.Unlock()
			log.Debug("treasury: profit received",
				"bot",           ev.BotName,
				"profit_usd",    fmt.Sprintf("$%.4f", ev.ProfitUSD),
				"pending_total", fmt.Sprintf("$%.4f", t.pendingUSD),
			)

		case <-distributeTicker.C:
			t.mu.Lock()
			pending := t.pendingUSD
			t.mu.Unlock()
			if pending >= 1.0 {
				t.distribute(ctx, pending)
			}

		case <-wcTicker.C:
			t.updateWorkingCapital(ctx)
		}
	}
}

func (t *Treasury) distribute(ctx context.Context, profitUSD float64) {
	log := logger.Log

	t.mu.Lock()
	defer t.mu.Unlock()

	phase := t.phase
	var botPct, spendingPct, overheadPct, stakingPct int
	if phase == 1 {
		botPct      = config.TreasuryReinvestPct
		spendingPct = config.TreasurySpendingPct
		overheadPct = config.TreasuryOverheadPct
		stakingPct  = config.TreasuryStakingPct
	} else {
		botPct      = config.TreasuryP2ReinvestPct
		spendingPct = config.TreasuryP2SpendingPct
		overheadPct = config.TreasuryP2OverheadPct
		stakingPct  = config.TreasuryP2StakingPct
	}

	dist := Distribution{
		ProfitUSD:   profitUSD,
		BotPct:      botPct,
		SpendingPct: spendingPct,
		OverheadPct: overheadPct,
		StakingPct:  stakingPct,
		BotAmt:      profitUSD * float64(botPct) / 100,
		SpendingAmt: profitUSD * float64(spendingPct) / 100,
		OverheadAmt: profitUSD * float64(overheadPct) / 100,
		StakingAmt:  profitUSD * float64(stakingPct) / 100,
		ProcessedAt: time.Now(),
		Phase:       phase,
	}

	log.Info("💸 treasury: distributing profit",
		"total_usd", fmt.Sprintf("$%.4f", profitUSD),
		"phase",     phase,
		"reinvest",  fmt.Sprintf("$%.4f (%.0f%%)", dist.BotAmt, float64(botPct)),
		"spending",  fmt.Sprintf("$%.4f (%.0f%%)", dist.SpendingAmt, float64(spendingPct)),
		"overhead",  fmt.Sprintf("$%.4f (%.0f%%)", dist.OverheadAmt, float64(overheadPct)),
		"staking",   fmt.Sprintf("$%.4f (%.0f%%)", dist.StakingAmt, float64(stakingPct)),
	)

	if dist.SpendingAmt >= 0.50 {
		if err := t.transferUSDC(ctx, t.spendingWallet, dist.SpendingAmt); err != nil {
			log.Error("treasury: spending wallet transfer failed", "err", err)
			state.Global.Alert("error", "treasury", "Spending wallet transfer failed", err.Error())
		}
	}

	if dist.OverheadAmt >= 0.50 {
		if err := t.transferUSDC(ctx, t.overheadWallet, dist.OverheadAmt); err != nil {
			log.Error("treasury: overhead wallet transfer failed", "err", err)
		}
	}

	if dist.StakingAmt >= config.StakingMinDeposit {
		if err := t.transferUSDC(ctx, t.stakingWallet, dist.StakingAmt); err != nil {
			log.Error("treasury: staking wallet transfer failed", "err", err)
		}
	}

	t.pendingUSD = 0
	t.totalDistributed += profitUSD
	t.distributions = append(t.distributions, dist)
	if len(t.distributions) > 1000 {
		t.distributions = t.distributions[len(t.distributions)-1000:]
	}
}

func (t *Treasury) transferUSDC(ctx context.Context, to common.Address, amountUSD float64) error {
	amountWei := new(big.Int).SetUint64(uint64(amountUSD * 1_000_000))
	log := logger.Log

	transferSel := []byte{0xa9, 0x05, 0x9c, 0xbb}
	toPadded   := common.LeftPadBytes(to.Bytes(), 32)
	amtPadded  := common.LeftPadBytes(amountWei.Bytes(), 32)
	data := append(transferSel, toPadded...)
	data  = append(data, amtPadded...)

	usdcAddr := common.HexToAddress(config.AddrUSDC)

	log.Info("📤 USDC transfer (TODO: broadcast)",
		"to",     to.Hex(),
		"amount", fmt.Sprintf("$%.4f USDC", amountUSD),
		"usdc",   usdcAddr.Hex(),
	)

	// TODO: wire in BuildAndSendTx once nonce manager is implemented
	_ = data
	return nil
}

func (t *Treasury) updateWorkingCapital(ctx context.Context) {
	log := logger.Log

	balInput := append(
		[]byte{0x70, 0xa0, 0x82, 0x31},
		common.LeftPadBytes(t.botWallet.Bytes(), 32)...,
	)
	usdcAddr := common.HexToAddress(config.AddrUSDC)
	out, err := t.client.CallContract(ctx, ethereum.CallMsg{
		To: &usdcAddr, Data: balInput,
	}, nil)
	if err != nil {
		return
	}

	balWei := new(big.Int).SetBytes(out)
	balUSD := float64(balWei.Uint64()) / 1_000_000

	state.Global.SetWorkingCapital(balUSD)

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.phase == 1 && balUSD >= config.WorkingCapitalTarget {
		t.phase = 2
		log.Info("🎉 PHASE 2 UNLOCKED",
			"balance",   fmt.Sprintf("$%.2f", balUSD),
			"new_split", "40/30/20/10",
		)
		state.Global.Alert("warn", "treasury",
			"Phase 2 unlocked — distribution ratios updated",
			fmt.Sprintf("Working capital: $%.2f", balUSD),
		)
	}

	log.Debug("treasury: working capital updated",
		"balance_usd", fmt.Sprintf("$%.2f", balUSD),
		"phase",       t.phase,
	)
}

func (t *Treasury) Summary() map[string]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()

	totalByDest := map[string]float64{
		"reinvested": 0,
		"spending":   0,
		"overhead":   0,
		"staking":    0,
	}
	for _, d := range t.distributions {
		totalByDest["reinvested"] += d.BotAmt
		totalByDest["spending"]   += d.SpendingAmt
		totalByDest["overhead"]   += d.OverheadAmt
		totalByDest["staking"]    += d.StakingAmt
	}

	return map[string]interface{}{
		"phase":              t.phase,
		"total_distributed":  t.totalDistributed,
		"pending":            t.pendingUSD,
		"distribution_count": len(t.distributions),
		"by_destination":     totalByDest,
		"working_capital":    state.Global.WorkingCapital(),
	}
}
