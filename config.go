package config

// core/config/config.go
//
// Single source of truth for all addresses, DEX IDs, tuning constants,
// and pair definitions used by every bot in the Money Printer system.

import "github.com/ethereum/go-ethereum/common"

// ── Chain ─────────────────────────────────────────────────────────────────────

const BaseChainID = 8453

// ── DEX IDs ───────────────────────────────────────────────────────────────────
// These must match the ArbitrageExecutor.sol contract exactly.

const (
	DexUniV3      = uint8(0)
	DexAerodrome  = uint8(1)
	DexBaseSwap   = uint8(2)
	DexAeroStable = uint8(3)
	DexSwapBased  = uint8(4)
	DexMaverick   = uint8(5) // NEW: Maverick V2 — directional liquidity bins
	DexCount      = 6
)

var DexNames = [DexCount]string{
	"UniV3",
	"AeroVolatile",
	"BaseSwap",
	"AeroStable",
	"SwapBased",
	"MaverickV2",
}

// ── Contract Addresses — Base Mainnet (chainID 8453) ─────────────────────────

var (
	// Flash loan provider
	AavePool = common.HexToAddress("0xA238Dd80C259a72e81d7e4664a9801593F98d1c5")

	// Uniswap V3
	UniV3Quoter  = common.HexToAddress("0x3d4e44Eb1374240CE5F1B136aa68B6a572e6f586")
	UniV3Router  = common.HexToAddress("0x2626664c2603336E57B271c5C0b26F421741e481")
	UniV3Factory = common.HexToAddress("0x33128a8fC17869897dcE68Ed026d694621f6FDfD")

	// Aerodrome V2
	AeroRouter  = common.HexToAddress("0xcF77a3Ba9A5CA399B7c97c74d54e5b1Beb874E43")
	AeroFactory = common.HexToAddress("0x420DD381b31aEf6683db6B902084cB0FFECe40Da")
	AeroQuoter  = common.HexToAddress("0x254cF9E1E6e233aa1AC962CB9B05b2cfeAaE15b0")

	// BaseSwap (V2-compatible)
	BaseswapRouter  = common.HexToAddress("0x327Df1E6de05895d2ab08513aaDD9313Fe505d86")
	BaseswapFactory = common.HexToAddress("0xFDa619b6d20975be80A10332cD39b9a4b0FAa8BB")

	// SwapBased (V2-compatible)
	SwapBasedRouter  = common.HexToAddress("0xaaa3b1F1bd7BCc97fD1917c18ADE665C5D31F066")
	SwapBasedFactory = common.HexToAddress("0x04C9f118d21e8B767D2e50C946f0cC9F6C367300")

	// Aerodrome stable
	AeroStableFactory = common.HexToAddress("0x420DD381b31aEf6683db6B902084cB0FFECe40Da")

	// Maverick V2 — directional liquidity bins, Base mainnet
	// Maverick V2 uses a PoolInformation contract for quoting
	MaverickV2Factory    = common.HexToAddress("0x0A11A9D84945b4868bcE5b17d84849059C01a35B")
	MaverickV2Quoter     = common.HexToAddress("0x6eBF4D8b42b27bbBF0a8cFc0781a0D63494FBB18")
	MaverickV2Router     = common.HexToAddress("0x5eDEd0d7E76C563FF081Ca01D9d12D6B404e2E9f")
	MaverickV2PoolLens   = common.HexToAddress("0x6ceB5f2eBEAe76cB35CC5a4cB6aAfe29e7fD5eD5")

	// Chainlink price feeds on Base (for oracle-lag bot)
	ChainlinkETHUSD  = common.HexToAddress("0x71041dddad3595F9CEd3DcCFBe3D1F4b0a16Bb70")
	ChainlinkBTCUSD  = common.HexToAddress("0xCCADC697c55bbB68dc5bCdf8d3CBe83CdD4E071E")

	// Pyth Network on Base (for oracle-lag bot)
	PythOracle = common.HexToAddress("0x8250f4aF4B972684F7b336503E2D6dFeDeB1487a")

	// 1inch router on Base (for solver settling events)
	OneInchRouter = common.HexToAddress("0x1111111254EEB25477B68fb85Ed929f73A960582")

	// CoW Protocol settlement (for solver events)
	CowSettlement = common.HexToAddress("0x9008D19f58AAbD9eD0D60971565AA8510560ab41")
)

// ── Token Addresses ───────────────────────────────────────────────────────────

const (
	AddrUSDC  = "0x833589fCD6eDb6E08f4c7C32D4f71b54bdA02913"
	AddrWETH  = "0x4200000000000000000000000000000000000006"
	AddrCBBTC = "0xcbB7C0000aB88B473b1f5aFd9ef808440eed33Bf"
	AddrAERO  = "0x940181a94A35A4569E4529A3CDfB74e38FD98631"
	AddrDAI   = "0x50c5725949A6F0c72E6C4a641F24049A917DB0Cb"
	AddrUSDbC = "0xd9aAEc86B65D86f6A7B5B1b0c42FFA531710b6CA" // bridged USDC (legacy)
)

// ── Tuning ────────────────────────────────────────────────────────────────────

const (
	// Arb bot
	LowCapPollInterval  = 15    // seconds — Tier2 pairs
	HotPairPollInterval = 5     // seconds — factory hot-loaded pairs
	MinProfitPct        = 0.55  // minimum spread % to evaluate
	SlippagePct         = 0.5
	FlashFeeBPS         = 5     // Aave 0.05%
	DeadlineBuffer      = 60    // seconds added to block.timestamp
	ProfitBufferPct     = 1.20
	CooldownSeconds     = 20
	ReconnectDelaySec   = 5
	PriorityFeeGwei     = 1
	GasLimitBuffer      = 1.25
	GasLimitBufferArb   = 1.35  // higher for 3-leg flash loan txs

	// Scalper bot
	ScalperMaxPositionUSDC = 100.0  // max 100 USDC per scalp
	ScalperMinProfitUSDC   = 0.50   // min 50 cents net profit to execute
	ScalperMaxHoldSec      = 300    // force-sell after 5 min regardless
	ScalperHoneypotTimeout = 30     // seconds to wait for sell simulation

	// Dust collector
	DustMinValueUSD     = 0.10  // minimum token value to bother collecting
	DustMaxGasCostUSD   = 0.05  // skip if gas cost > this per dust token
	DustGasPriceMaxGwei = 0.05  // only run dust collection when gas is this cheap or cheaper

	// Oracle lag
	OracleLagMinBPS  = 30  // minimum 0.30% lag between on-chain and oracle price to trade
	OracleLagTimeout = 8   // seconds — oracle updates within this usually, so act fast

	// Treasury distribution (Phase 1: before $100k working capital)
	TreasuryReinvestPct    = 50  // % reinvested into bots
	TreasurySpendingPct    = 25  // % to spending wallet (Visa)
	TreasuryOverheadPct    = 15  // % to overhead wallet (Visa)
	TreasuryStakingPct     = 10  // % to staking pool

	// Treasury distribution (Phase 2: after $100k working capital)
	TreasuryP2ReinvestPct  = 40
	TreasuryP2SpendingPct  = 30
	TreasuryP2OverheadPct  = 20
	TreasuryP2StakingPct   = 10

	WorkingCapitalTarget   = 100_000 // USD — threshold to switch to Phase 2

	// Staking
	StakingMinDeposit    = 10.0  // minimum USDC to deposit
	StakingRebalanceHour = 3     // UTC hour to run staking rebalance (low gas)

	// Reporting
	ReportEmailHour = 8 // UTC hour to send daily report
)

// ── Token Pair ────────────────────────────────────────────────────────────────

type TokenPair struct {
	SymA      string
	AddrA     common.Address
	DecA      int
	SymB      string
	AddrB     common.Address
	DecB      int
	UniV3Fee  uint32
	TradeSize uint64
}

var TradeSizes = map[string]uint64{
	"USDC": 200_000_000,               // 200 USDC
	"WETH": 50_000_000_000_000_000,    // 0.05 WETH
}

var TradeSizesNew = map[string]uint64{
	"USDC": 50_000_000,               // 50 USDC — for new/unproven tokens
	"WETH": 10_000_000_000_000_000,   // 0.01 WETH
}

func tradeSize(sym string, dec int) uint64 {
	if s, ok := TradeSizes[sym]; ok {
		return s
	}
	result := uint64(10)
	for i := 0; i < dec; i++ {
		result *= 10
	}
	return result
}

func NewPair(symA, addrA string, decA int, symB, addrB string, decB int, fee uint32) TokenPair {
	return TokenPair{
		SymA: symA, AddrA: common.HexToAddress(addrA), DecA: decA,
		SymB: symB, AddrB: common.HexToAddress(addrB), DecB: decB,
		UniV3Fee:  fee,
		TradeSize: tradeSize(symA, decA),
	}
}

// ── Tier 1 Pairs — event-driven ───────────────────────────────────────────────

var Tier1Pairs = []TokenPair{
	NewPair("USDC", AddrUSDC, 6, "WETH", AddrWETH, 18, 500),
}

// ── Tier 2 Pairs — polled ─────────────────────────────────────────────────────

var Tier2Pairs = []TokenPair{
	NewPair("USDC", AddrUSDC, 6, "AERO",  AddrAERO,  18, 3000),
	NewPair("WETH", AddrWETH, 18, "cbBTC", AddrCBBTC, 8, 500),
}

// ── Factories watched by FactoryWatcher ───────────────────────────────────────
// Used at runtime — not hardcoded in factory_watcher.go to allow easy extension.

type FactoryConfig struct {
	Name  string
	Addr  common.Address
	IsV3  bool
	DexID uint8
}

var WatchedFactories = []FactoryConfig{
	{Name: "UniswapV3",   Addr: UniV3Factory,    IsV3: true,  DexID: DexUniV3},
	{Name: "Aerodrome",   Addr: AeroFactory,     IsV3: false, DexID: DexAerodrome},
	{Name: "BaseSwap",    Addr: BaseswapFactory, IsV3: false, DexID: DexBaseSwap},
	{Name: "SwapBased",   Addr: SwapBasedFactory,IsV3: false, DexID: DexSwapBased},
	{Name: "MaverickV2",  Addr: MaverickV2Factory,IsV3: false,DexID: DexMaverick},
}

// ── Staking protocols on Base ─────────────────────────────────────────────────

type StakingProtocol struct {
	Name    string
	Addr    common.Address
	Token   common.Address // deposit token
	APYHint float64        // rough current APY hint — updated by staking bot
}

var StakingProtocols = []StakingProtocol{
	{
		Name:    "Aerodrome veAERO",
		Addr:    common.HexToAddress("0xeBf418Fe2512e7E6bd9b87a8F0f294aCDC67e6B4"),
		Token:   common.HexToAddress(AddrAERO),
		APYHint: 15.0,
	},
	{
		Name:    "Moonwell USDC",
		Addr:    common.HexToAddress("0xEdc817A28E8B93B03976FBd4a3dDBc9f7D176c22"),
		Token:   common.HexToAddress(AddrUSDC),
		APYHint: 6.0,
	},
	{
		Name:    "Seamless USDC",
		Addr:    common.HexToAddress("0x0568A3aEB8E78262dEFf861C4e4735b9c0D4B671"),
		Token:   common.HexToAddress(AddrUSDC),
		APYHint: 5.5,
	},
}
// ── LP-Migrator tuning ────────────────────────────────────────────────────────

const (
	LPMigratorMinBurnUSD    = 10_000 // minimum USD value of LP burn to track
	LPMigratorMaxBlockDelta = 20     // max blocks between Burn and Mint to confirm migration
	LPMigratorPendingExpiry = 60     // seconds before a pending burn expires unmatched
)

// ── RWA Assets on Base ────────────────────────────────────────────────────────

const (
	AddrBIB01 = "0xCA30c93B02514f86d5C86a6e375E3A330B435Fb5" // Backed Finance bIB01
	AddrOUSG  = "0x1B19C19393e2d034D8Ff31ff34c81252FcBbe39B" // Ondo OUSG

	// RWA Oracle tuning
	RWAMinDiscrepancyPct  = 0.50  // minimum NAV vs DEX gap % to trade
	RWAMaxNAVAgeHours     = 25    // don't trade on NAV older than this (>1 day = weekend)
	RWAMaxPositionUSD     = 5_000 // max USDC per RWA trade
	RWAPollIntervalMin    = 5     // minutes between NAV fetches
	RWAFredPollIntervalH  = 1     // hours between Fed rate fetches
)

// FRED API URL — no key required for public data series
const FredFFRURL = "https://fred.stlouisfed.org/graph/fredgraph.csv?id=DFF"

// Backed Finance API
const BackedAPIURL = "https://backed.fi/api/tokens"

// Ondo Finance API
const OndoAPIURL = "https://api.ondo.finance/v1/funds"
