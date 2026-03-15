package rwa

// bots/rwa/rwa.go
//
// RWA Oracle Lag Bot — Real World Asset Rate Arbitrage
//
// ── The Opportunity ──────────────────────────────────────────────────────────
//
// Tokenized real-world assets (RWAs) on Base have TWO prices:
//
//   1. NAV Price (oracle): the actual underlying asset value, updated when
//      the off-chain asset moves. For T-bills: updated daily at ~4pm ET.
//      For rate-sensitive assets: updated when the Fed/ECB moves rates.
//
//   2. DEX Market Price: what the token actually trades for on Aerodrome,
//      Uniswap V3, etc. This lags the NAV by minutes to hours.
//
// The gap between NAV and DEX price is your edge. It arises because:
//   - Liquidity providers don't watch macro feeds
//   - Rebalancing bots are slow to react to NAV updates
//   - The DEX orderbook for RWAs is thin ($200k–$2M typically)
//
// ── Monitored Assets ──────────────────────────────────────────────────────────
//
//   bIB01 (Backed Finance): tokenized 0-3 month US T-bill ETF
//     • Oracle: Backed Finance API (daily NAV) + Chainlink Proof of Reserve
//     • DEX: Aerodrome and UniV3 pools on Base
//     • Typical lag: 5-60 minutes after NAV update
//
//   OUSG (Ondo Finance): tokenized BlackRock US Treasury fund
//     • Oracle: Ondo's own oracle (daily update at 4pm ET)
//     • DEX: limited Base liquidity — larger but less frequent spread
//
//   cbBTC/WBTC spread: Coinbase's cbBTC vs wrapped BTC
//     • Oracle: both track BTC/USD Chainlink feed
//     • DEX: peg deviation on low-liquidity pools = arb
//     • This is the highest-frequency opportunity in this category
//
// ── Macro Feed Architecture ───────────────────────────────────────────────────
//
// We poll two off-chain sources:
//
//   1. FRED API (Federal Reserve Economic Data — free, no key needed for
//      most endpoints): Fed Funds Rate, Treasury yields
//      Endpoint: https://fred.stlouisfed.org/graph/fredgraph.csv?id=DFF
//
//   2. Backed Finance API: live NAV for bIB01 and other Backed tokens
//      Endpoint: https://backed.fi/api/tokens (no auth required)
//
// When off-chain NAV diverges from on-chain DEX price by > threshold:
//   → We emit an RWAOracleEvent to state.Global.RWAOracleCh
//   → The arb bot receives it and executes a flash-loan swap
//
// ── Trade Logic ──────────────────────────────────────────────────────────────
//
// If NAV > DEX price (DEX is underpriced):
//   Direction: LONG_ASSET — buy on DEX, hold until oracle updates DEX price
//   OR: buy on DEX, sell on a protocol that prices at NAV (like Ondo's redemption)
//
// If NAV < DEX price (DEX is overpriced):
//   Direction: SHORT_ASSET — borrow via Aave (at stale price), sell on DEX
//   This is riskier — only execute when discrepancy > 1.5%
//
// Minimum discrepancy threshold: 0.50% (covers DEX fees + slippage)

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"

	"github.com/yourname/money-printer/core/config"
	"github.com/yourname/money-printer/core/logger"
	"github.com/yourname/money-printer/core/state"
)

// ── RWA asset definition ──────────────────────────────────────────────────────

type RWAAsset struct {
	Symbol       string
	Addr         common.Address
	Decimals     int
	NAVSource    string  // "backed", "ondo", "chainlink", "fred-derived"
	MinSpreadPct float64 // minimum spread to trade
	MaxTradeUSD  float64 // maximum position size
	Description  string
}

// All monitored RWA assets on Base
var rwaAssets = []RWAAsset{
	{
		Symbol:       "bIB01",
		Addr:         common.HexToAddress("0xCA30c93B02514f86d5C86a6e375E3A330B435Fb5"),
		Decimals:     18,
		NAVSource:    "backed",
		MinSpreadPct: 0.50,
		MaxTradeUSD:  5_000,
		Description:  "Backed Finance tokenized 0-3M US T-bill ETF",
	},
	{
		Symbol:       "OUSG",
		Addr:         common.HexToAddress("0x1B19C19393e2d034D8Ff31ff34c81252FcBbe39B"),
		Decimals:     18,
		NAVSource:    "ondo",
		MinSpreadPct: 0.75,
		MaxTradeUSD:  2_000,
		Description:  "Ondo Finance tokenized BlackRock Treasury fund",
	},
	{
		Symbol:       "cbBTC",
		Addr:         common.HexToAddress(config.AddrCBBTC),
		Decimals:     8,
		NAVSource:    "chainlink", // tracks BTC/USD — use for cbBTC/WBTC spread
		MinSpreadPct: 0.30,
		MaxTradeUSD:  10_000,
		Description:  "Coinbase cbBTC — peg deviation vs WBTC",
	},
}

// ── NAV data structures ───────────────────────────────────────────────────────

type NAVData struct {
	Symbol    string
	NAV       float64
	UpdatedAt time.Time
	Source    string
}

// Backed Finance API response
type backedAPIResponse struct {
	Data []struct {
		Symbol string  `json:"symbol"`
		Name   string  `json:"name"`
		NAV    float64 `json:"nav"`
		UpdatedAt string `json:"updatedAt"`
	} `json:"data"`
}

// Ondo Finance API response
type ondoAPIResponse struct {
	Funds []struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
		Date   string `json:"date"`
	} `json:"funds"`
}

// ── RWABot ────────────────────────────────────────────────────────────────────

type RWABot struct {
	client     *ethclient.Client
	httpClient *http.Client

	mu       sync.RWMutex
	navCache map[string]NAVData // symbol → latest NAV

	// Fed Funds Rate (used to estimate T-bill yield baseline)
	fedFundsRate float64
	fedUpdatedAt time.Time
}

func New(client *ethclient.Client) *RWABot {
	return &RWABot{
		client:     client,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		navCache:   make(map[string]NAVData),
	}
}

// Run starts the RWA oracle lag bot.
func (bot *RWABot) Run(ctx context.Context) {
	log := logger.Log
	log.Info("📈 RWA oracle lag bot started",
		"assets", len(rwaAssets),
	)

	// Initial fetch
	bot.fetchAllNAVs(ctx)
	bot.fetchFedRate(ctx)

	// Poll NAVs every 5 minutes
	navTicker := time.NewTicker(5 * time.Minute)
	defer navTicker.Stop()

	// Poll Fed rate every hour
	fedTicker := time.NewTicker(1 * time.Hour)
	defer fedTicker.Stop()

	// Compare NAV vs DEX price every 60 seconds
	compareTicker := time.NewTicker(60 * time.Second)
	defer compareTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("RWA bot shutting down")
			return

		case <-navTicker.C:
			go bot.fetchAllNAVs(ctx)

		case <-fedTicker.C:
			go bot.fetchFedRate(ctx)

		case <-compareTicker.C:
			go bot.compareAllAssets(ctx)
		}
	}
}

// ── NAV fetchers ──────────────────────────────────────────────────────────────

func (bot *RWABot) fetchAllNAVs(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); bot.fetchBackedNAVs(ctx) }()
	go func() { defer wg.Done(); bot.fetchOndoNAVs(ctx) }()
	wg.Wait()
}

// fetchBackedNAVs fetches NAVs for Backed Finance tokens (bIB01, etc.)
func (bot *RWABot) fetchBackedNAVs(ctx context.Context) {
	log := logger.Log

	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://backed.fi/api/tokens", nil)
	if err != nil {
		return
	}

	resp, err := bot.httpClient.Do(req)
	if err != nil {
		log.Warn("backed.fi API failed", "err", err)
		return
	}
	defer resp.Body.Close()

	var apiResp backedAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		log.Warn("backed.fi decode failed", "err", err)
		return
	}

	bot.mu.Lock()
	defer bot.mu.Unlock()
	for _, token := range apiResp.Data {
		updatedAt, _ := time.Parse(time.RFC3339, token.UpdatedAt)
		bot.navCache[token.Symbol] = NAVData{
			Symbol:    token.Symbol,
			NAV:       token.NAV,
			UpdatedAt: updatedAt,
			Source:    "backed",
		}
		log.Debug("NAV updated", "symbol", token.Symbol, "nav", token.NAV)
	}
}

// fetchOndoNAVs fetches NAVs for Ondo Finance tokens (OUSG, etc.)
func (bot *RWABot) fetchOndoNAVs(ctx context.Context) {
	log := logger.Log

	// Ondo's price API
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://api.ondo.finance/v1/funds", nil)
	if err != nil {
		return
	}

	resp, err := bot.httpClient.Do(req)
	if err != nil {
		log.Warn("Ondo API failed", "err", err)
		return
	}
	defer resp.Body.Close()

	var apiResp ondoAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		log.Warn("Ondo decode failed", "err", err)
		return
	}

	bot.mu.Lock()
	defer bot.mu.Unlock()
	for _, fund := range apiResp.Funds {
		price, err := strconv.ParseFloat(fund.Price, 64)
		if err != nil {
			continue
		}
		date, _ := time.Parse("2006-01-02", fund.Date)
		bot.navCache[fund.Symbol] = NAVData{
			Symbol:    fund.Symbol,
			NAV:       price,
			UpdatedAt: date,
			Source:    "ondo",
		}
		log.Debug("NAV updated", "symbol", fund.Symbol, "nav", price)
	}
}

// fetchFedRate fetches the current Fed Funds Rate from FRED (free, no key needed).
func (bot *RWABot) fetchFedRate(ctx context.Context) {
	log := logger.Log

	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://fred.stlouisfed.org/graph/fredgraph.csv?id=DFF", nil)
	if err != nil {
		return
	}

	resp, err := bot.httpClient.Do(req)
	if err != nil {
		log.Warn("FRED API failed", "err", err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	// CSV format: date,rate — take the last row
	r := csv.NewReader(strings.NewReader(string(body)))
	records, err := r.ReadAll()
	if err != nil || len(records) < 2 {
		return
	}

	lastRow := records[len(records)-1]
	if len(lastRow) < 2 {
		return
	}

	rate, err := strconv.ParseFloat(lastRow[1], 64)
	if err != nil {
		return
	}

	bot.mu.Lock()
	bot.fedFundsRate = rate
	bot.fedUpdatedAt = time.Now()
	bot.mu.Unlock()

	log.Info("📊 Fed Funds Rate updated",
		"rate", fmt.Sprintf("%.2f%%", rate),
		"date", lastRow[0],
	)

	// Check if rate change implies T-bill price movement
	// T-bill prices move inversely to rates:
	//   Rate up → T-bill price down → bIB01 NAV should fall
	//   Rate down → T-bill price up → bIB01 NAV should rise
	// If we have a recent NAV that hasn't priced in this rate change,
	// we have a predictive edge before the Backed oracle updates.
	bot.checkRateImplication(ctx, rate)
}

// checkRateImplication estimates whether a Fed rate change implies a
// mispricing in tokenized T-bills, before the oracle updates.
func (bot *RWABot) checkRateImplication(ctx context.Context, newRate float64) {
	log := logger.Log
	bot.mu.RLock()
	bib01Nav, hasBib01 := bot.navCache["bIB01"]
	bot.mu.RUnlock()

	if !hasBib01 {
		return
	}

	// If Fed rate updated but bIB01 NAV is from yesterday, we have a 24h edge
	navAge := time.Since(bib01Nav.UpdatedAt)
	if navAge < 12*time.Hour {
		return // NAV is fresh enough, no predictive edge
	}

	// T-bill duration ~0-3 months: rate sensitivity ≈ 0.25 (DV01)
	// A 25bp rate hike → ~0.0625% T-bill price decline
	// This is modest but if the DEX hasn't priced it in at all, it's tradeable
	impliedNAVChange := (newRate - (bib01Nav.NAV - 1.0) * 100) * -0.0025
	if impliedNAVChange == 0 {
		return
	}

	log.Info("📊 Predictive RWA signal: Fed rate implies NAV adjustment",
		"symbol",         "bIB01",
		"current_nav",    fmt.Sprintf("$%.4f", bib01Nav.NAV),
		"implied_change", fmt.Sprintf("%.4f%%", impliedNAVChange),
		"nav_age_hours",  fmt.Sprintf("%.1f", navAge.Hours()),
	)

	// Emit a low-confidence oracle event as a predictive signal
	select {
	case state.Global.RWAOracleCh <- state.RWAOracleEvent{
		AssetSym:       "bIB01",
		AssetAddr:      rwaAssets[0].Addr,
		OldNAV:         bib01Nav.NAV,
		NewNAV:         bib01Nav.NAV * (1 + impliedNAVChange/100),
		DEXPrice:       0, // will be filled in by compareAllAssets
		DiscrepancyPct: impliedNAVChange,
		Direction:      map[bool]string{true: "SHORT_ASSET", false: "LONG_ASSET"}[impliedNAVChange < 0],
		Source:         "FRED-predictive",
		DetectedAt:     time.Now(),
	}:
	default:
	}
}

// ── NAV vs DEX comparison ─────────────────────────────────────────────────────

func (bot *RWABot) compareAllAssets(ctx context.Context) {
	bot.mu.RLock()
	navSnapshot := make(map[string]NAVData, len(bot.navCache))
	for k, v := range bot.navCache {
		navSnapshot[k] = v
	}
	bot.mu.RUnlock()

	for _, asset := range rwaAssets {
		nav, hasNAV := navSnapshot[asset.Symbol]
		if !hasNAV {
			continue
		}

		// Get DEX price from shared state
		priceEv, hasDEXPrice := state.Global.GetPrice(asset.Addr)
		if !hasDEXPrice || priceEv.PriceUSD <= 0 {
			// Try to fetch it fresh
			dexPrice, err := bot.fetchDEXPrice(ctx, asset)
			if err != nil || dexPrice <= 0 {
				continue
			}
			priceEv.PriceUSD = dexPrice
			priceEv.TokenSym = asset.Symbol
		}

		bot.evaluateDiscrepancy(ctx, asset, nav, priceEv.PriceUSD)
	}
}

// evaluateDiscrepancy checks if a NAV vs DEX price gap is large enough to trade.
func (bot *RWABot) evaluateDiscrepancy(ctx context.Context, asset RWAAsset, nav NAVData, dexPrice float64) {
	log := logger.Log

	if nav.NAV <= 0 || dexPrice <= 0 {
		return
	}

	discrepancyPct := ((nav.NAV - dexPrice) / dexPrice) * 100

	// Ignore tiny discrepancies
	absDisc := discrepancyPct
	if absDisc < 0 {
		absDisc = -absDisc
	}
	if absDisc < asset.MinSpreadPct {
		log.Debug("RWA: discrepancy below threshold",
			"asset",       asset.Symbol,
			"nav",         fmt.Sprintf("$%.4f", nav.NAV),
			"dex",         fmt.Sprintf("$%.4f", dexPrice),
			"discrepancy", fmt.Sprintf("%.4f%%", discrepancyPct),
			"threshold",   fmt.Sprintf("%.2f%%", asset.MinSpreadPct),
		)
		return
	}

	// NAV age check — don't trade on stale NAV data (> 25 hours = weekend)
	navAge := time.Since(nav.UpdatedAt)
	if navAge > 25*time.Hour {
		log.Debug("RWA: NAV too stale", "asset", asset.Symbol,
			"age_hours", fmt.Sprintf("%.1f", navAge.Hours()))
		return
	}

	direction := "LONG_ASSET"
	if discrepancyPct < 0 {
		direction = "SHORT_ASSET"
		// Be more conservative on shorts — require higher threshold
		if absDisc < asset.MinSpreadPct*1.5 {
			return
		}
	}

	log.Info("💰 RWA ORACLE LAG DETECTED",
		"asset",       asset.Symbol,
		"nav",         fmt.Sprintf("$%.4f", nav.NAV),
		"dex_price",   fmt.Sprintf("$%.4f", dexPrice),
		"discrepancy", fmt.Sprintf("%.4f%%", discrepancyPct),
		"direction",   direction,
		"nav_source",  nav.Source,
		"nav_age",     navAge.Round(time.Minute),
	)

	ev := state.RWAOracleEvent{
		AssetSym:       asset.Symbol,
		AssetAddr:      asset.Addr,
		OldNAV:         nav.NAV,
		NewNAV:         nav.NAV,
		DEXPrice:       dexPrice,
		DiscrepancyPct: discrepancyPct,
		Direction:      direction,
		Source:         nav.Source,
		DetectedAt:     time.Now(),
	}

	select {
	case state.Global.RWAOracleCh <- ev:
	default:
		log.Warn("RWA: RWAOracleCh full")
	}

	state.Global.Alert("warn", "rwa",
		fmt.Sprintf("RWA opportunity: %s %.4f%% (%s)",
			asset.Symbol, discrepancyPct, direction),
		fmt.Sprintf("NAV: $%.4f | DEX: $%.4f | Source: %s",
			nav.NAV, dexPrice, nav.Source),
	)
}

// fetchDEXPrice fetches the current DEX price for an RWA asset via UniV3 quoter.
func (bot *RWABot) fetchDEXPrice(ctx context.Context, asset RWAAsset) (float64, error) {
	// Quote: 1 USDC → asset.Symbol and invert
	// This gives us price in USDC per unit of the asset
	// We reuse the chain-level quote via a simple RPC call

	usdcAddr := common.HexToAddress(config.AddrUSDC)
	_ = usdcAddr

	// For now: return cached price — full implementation calls chain.UniV3Quote
	// chain.UniV3Quote(ctx, bot.client, config.UniV3Quoter, usdcAddr, asset.Addr, 500, big.NewInt(1_000_000))
	return 0, nil
}
