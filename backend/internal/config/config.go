package config

import (
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

const (
	defaultTimezone            = "Asia/Shanghai"
	defaultERC20Contract       = "0x1c7D4B196Cb0C7B01d743Fbc6116a902379C7238"
	defaultPlatformWalletPath  = "m/44'/60'/0'/0/0"
	defaultGasTopupMaxWei      = "5000000000000000"
	defaultPlatformMinETHWei   = "10000000000000000"
	maximumERC20ScanBatchSize  = 1000
	maximumGasSafetyBasisPoint = 10000
)

// ERC20Config 描述单个 Sepolia ERC-20 资产及其扫描、归集和 Gas 补充参数。
type ERC20Config struct {
	Enabled         bool
	ContractAddress string
	Symbol          string
	Decimals        uint8
	ScanStartBlock  *uint64
	ScanBatchSize   uint64
	Confirmations   uint64
	SweepEnabled    bool
	SweepInterval   time.Duration
	GasSafetyBPS    uint64
	GasTopupMaxWei  *big.Int
}

// PlatformWalletConfig 描述平台热钱包派生路径和 ETH 余额告警阈值。
type PlatformWalletConfig struct {
	HotWalletPath    string
	MinETHBalanceWei *big.Int
}

type Config struct {
	AppEnv                 string
	HTTPAddr               string
	Timezone               *time.Location
	DatabaseURL            string
	CustodyKeyStoreFile    string
	CustodyPassword        string
	SepoliaRPCURL          string
	SepoliaRPCFallbackURL  string
	SepoliaRPCTimeout      time.Duration
	SepoliaRPCMaxAttempts  int
	SepoliaRPCBaseDelay    time.Duration
	SepoliaRPCMaxDelay     time.Duration
	SepoliaScanStartBlock  *uint64
	SepoliaConfirmations   uint64
	SepoliaScanInterval    time.Duration
	SepoliaScanBatchSize   uint64
	SepoliaExplorerBaseURL string
	ERC20                  ERC20Config
	PlatformWallet         PlatformWalletConfig
}

// Load 从环境变量加载并校验应用配置。
func Load() (Config, error) {
	return load(true)
}

// LoadPreflight 从环境变量加载只读预检配置，不要求托管密钥文件和密码。
func LoadPreflight() (Config, error) {
	return load(false)
}

// load 加载环境变量，并按运行场景决定是否校验托管密钥配置。
func load(requireCustody bool) (Config, error) {
	timezoneName := envOrDefault("APP_TIMEZONE", defaultTimezone)
	timezone, err := time.LoadLocation(timezoneName)
	if err != nil {
		return Config{}, fmt.Errorf("APP_TIMEZONE 配置无效：%w", err)
	}
	scanStartBlock, err := optionalUint64("SEPOLIA_SCAN_START_BLOCK")
	if err != nil {
		return Config{}, err
	}
	erc20Enabled, err := boolOrDefault("ERC20_ENABLED", false)
	if err != nil {
		return Config{}, err
	}
	erc20ScanStartBlock, err := optionalUint64("ERC20_SCAN_START_BLOCK")
	if err != nil {
		return Config{}, err
	}
	erc20SweepEnabled, err := boolOrDefault("ERC20_SWEEP_ENABLED", false)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		AppEnv:                 envOrDefault("APP_ENV", "development"),
		HTTPAddr:               envOrDefault("HTTP_ADDR", ":8080"),
		Timezone:               timezone,
		DatabaseURL:            strings.TrimSpace(os.Getenv("DATABASE_URL")),
		CustodyKeyStoreFile:    strings.TrimSpace(os.Getenv("CUSTODY_KEYSTORE_FILE")),
		CustodyPassword:        os.Getenv("CUSTODY_KEYSTORE_PASSWORD"),
		SepoliaRPCURL:          strings.TrimSpace(os.Getenv("SEPOLIA_RPC_URL")),
		SepoliaRPCFallbackURL:  strings.TrimSpace(os.Getenv("SEPOLIA_RPC_FALLBACK_URL")),
		SepoliaRPCTimeout:      durationOrDefault("SEPOLIA_RPC_TIMEOUT", 10*time.Second),
		SepoliaRPCMaxAttempts:  intOrDefault("SEPOLIA_RPC_MAX_ATTEMPTS", 3),
		SepoliaRPCBaseDelay:    durationOrDefault("SEPOLIA_RPC_BASE_DELAY", 250*time.Millisecond),
		SepoliaRPCMaxDelay:     durationOrDefault("SEPOLIA_RPC_MAX_DELAY", 3*time.Second),
		SepoliaScanStartBlock:  scanStartBlock,
		SepoliaConfirmations:   uint64OrDefault("SEPOLIA_CONFIRMATIONS", 3),
		SepoliaScanInterval:    durationOrDefault("SEPOLIA_SCAN_INTERVAL", 12*time.Second),
		SepoliaScanBatchSize:   uint64OrDefault("SEPOLIA_SCAN_BATCH_SIZE", 20),
		SepoliaExplorerBaseURL: strings.TrimRight(envOrDefault("SEPOLIA_EXPLORER_BASE_URL", "https://sepolia.etherscan.io"), "/"),
		ERC20: ERC20Config{
			Enabled:         erc20Enabled,
			ContractAddress: strings.TrimSpace(envOrDefault("ERC20_CONTRACT_ADDRESS", defaultERC20Contract)),
			Symbol:          strings.TrimSpace(envOrDefault("ERC20_SYMBOL", "USDC")),
			Decimals:        uint8OrDefault("ERC20_DECIMALS", 6),
			ScanStartBlock:  erc20ScanStartBlock,
			ScanBatchSize:   uint64OrDefault("ERC20_SCAN_BATCH_SIZE", 100),
			Confirmations:   uint64OrDefault("ERC20_CONFIRMATIONS", 3),
			SweepEnabled:    erc20SweepEnabled,
			SweepInterval:   durationOrDefault("ERC20_SWEEP_INTERVAL", 15*time.Second),
			GasSafetyBPS:    uint64OrDefault("ERC20_GAS_SAFETY_BPS", 2000),
			GasTopupMaxWei:  bigIntOrDefault("ERC20_GAS_TOPUP_MAX_WEI", defaultGasTopupMaxWei),
		},
		PlatformWallet: PlatformWalletConfig{
			HotWalletPath:    envOrDefault("PLATFORM_HOT_WALLET_PATH", defaultPlatformWalletPath),
			MinETHBalanceWei: bigIntOrDefault("PLATFORM_MIN_ETH_BALANCE_WEI", defaultPlatformMinETHWei),
		},
	}

	if err := cfg.validate(requireCustody); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate 校验启动所需的配置是否完整。
func (c Config) Validate() error {
	return c.validate(true)
}

// validate 校验公共配置，并按运行场景决定是否要求托管密钥。
func (c Config) validate(requireCustody bool) error {
	missing := make([]string, 0, 4)
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if requireCustody {
		if c.CustodyKeyStoreFile == "" {
			missing = append(missing, "CUSTODY_KEYSTORE_FILE")
		}
		if c.CustodyPassword == "" {
			missing = append(missing, "CUSTODY_KEYSTORE_PASSWORD")
		}
	}
	if c.SepoliaRPCURL == "" {
		missing = append(missing, "SEPOLIA_RPC_URL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("缺少必需配置：%s", strings.Join(missing, ", "))
	}
	if c.Timezone == nil {
		return errors.New("必须配置时区")
	}
	if c.SepoliaRPCTimeout <= 0 || c.SepoliaRPCMaxAttempts <= 0 || c.SepoliaRPCBaseDelay < 0 || c.SepoliaRPCMaxDelay < c.SepoliaRPCBaseDelay {
		return errors.New("Sepolia RPC 重试配置无效")
	}
	if c.SepoliaConfirmations == 0 || c.SepoliaScanInterval <= 0 || c.SepoliaScanBatchSize == 0 || c.SepoliaScanBatchSize > 100 {
		return errors.New("Sepolia 充值扫描配置无效")
	}
	explorerURL, err := url.Parse(c.SepoliaExplorerBaseURL)
	if err != nil || explorerURL.Scheme != "https" || explorerURL.Host == "" {
		return errors.New("Sepolia 区块浏览器地址无效")
	}
	if c.ERC20.SweepEnabled && !c.ERC20.Enabled {
		return errors.New("启用 ERC-20 归集前必须先启用 ERC-20")
	}
	if !c.ERC20.Enabled {
		return nil
	}
	if !common.IsHexAddress(c.ERC20.ContractAddress) || common.HexToAddress(c.ERC20.ContractAddress) == (common.Address{}) {
		return errors.New("ERC-20 合约地址无效")
	}
	if !isTokenSymbol(c.ERC20.Symbol) {
		return errors.New("ERC-20 symbol 必须是 2 到 12 位大写字母或数字")
	}
	if c.ERC20.Decimals == 0 || c.ERC20.Decimals > 18 {
		return errors.New("ERC-20 decimals 必须在 1 到 18 之间")
	}
	if c.ERC20.Confirmations == 0 || c.ERC20.ScanBatchSize == 0 || c.ERC20.ScanBatchSize > maximumERC20ScanBatchSize {
		return errors.New("ERC-20 充值扫描配置无效")
	}
	if c.ERC20.SweepInterval <= 0 {
		return errors.New("ERC-20 归集间隔必须大于零")
	}
	if c.ERC20.GasSafetyBPS == 0 || c.ERC20.GasSafetyBPS > maximumGasSafetyBasisPoint {
		return errors.New("ERC-20 Gas 安全余量必须在 1 到 10000 个基点之间")
	}
	if c.ERC20.GasTopupMaxWei == nil || c.ERC20.GasTopupMaxWei.Sign() <= 0 {
		return errors.New("ERC-20 单次 Gas 补充上限必须是正整数")
	}
	if c.PlatformWallet.HotWalletPath != defaultPlatformWalletPath {
		return errors.New("平台热钱包派生路径必须为 m/44'/60'/0'/0/0")
	}
	if c.PlatformWallet.MinETHBalanceWei == nil || c.PlatformWallet.MinETHBalanceWei.Sign() <= 0 {
		return errors.New("平台热钱包 ETH 余额阈值必须是正整数")
	}
	return nil
}

// SafeSummary 返回不会泄露密码和连接凭据的配置摘要。
func (c Config) SafeSummary() map[string]string {
	return map[string]string{
		"app_env":               c.AppEnv,
		"http_addr":             c.HTTPAddr,
		"timezone":              c.Timezone.String(),
		"keystore_file":         c.CustodyKeyStoreFile,
		"database":              configured(c.DatabaseURL),
		"sepolia_rpc":           configured(c.SepoliaRPCURL),
		"sepolia_rpc_fallback":  configured(c.SepoliaRPCFallbackURL),
		"sepolia_rpc_timeout":   c.SepoliaRPCTimeout.String(),
		"sepolia_confirmations": strconv.FormatUint(c.SepoliaConfirmations, 10),
		"sepolia_scan_interval": c.SepoliaScanInterval.String(),
		"sepolia_scan_batch":    strconv.FormatUint(c.SepoliaScanBatchSize, 10),
		"sepolia_explorer":      c.SepoliaExplorerBaseURL,
		"erc20_enabled":         strconv.FormatBool(c.ERC20.Enabled),
		"erc20_contract":        c.ERC20.ContractAddress,
		"erc20_symbol":          c.ERC20.Symbol,
		"erc20_decimals":        strconv.FormatUint(uint64(c.ERC20.Decimals), 10),
		"erc20_sweep_enabled":   strconv.FormatBool(c.ERC20.SweepEnabled),
		"platform_wallet_path":  c.PlatformWallet.HotWalletPath,
	}
}

// envOrDefault 读取环境变量，空值时返回默认值。
func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// configured 将敏感配置转换为不包含原值的状态文本。
func configured(value string) string {
	if value == "" {
		return "缺失"
	}
	return "已配置"
}

// durationOrDefault 读取时长配置，格式错误时返回默认值，由 Validate 负责最终校验范围。
func durationOrDefault(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0
	}
	return parsed
}

// intOrDefault 读取整数配置，格式错误时返回零，由 Validate 负责最终校验范围。
func intOrDefault(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}

// boolOrDefault 读取布尔配置，格式错误时返回中文错误。
func boolOrDefault(key string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s 配置必须是布尔值", key)
	}
	return parsed, nil
}

// uint64OrDefault 读取无符号整数配置，格式错误时返回零并交由 Validate 拒绝。
func uint64OrDefault(key string, fallback uint64) uint64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// uint8OrDefault 读取八位无符号整数，格式错误时返回零并交由 Validate 拒绝。
func uint8OrDefault(key string, fallback uint8) uint8 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseUint(value, 10, 8)
	if err != nil {
		return 0
	}
	return uint8(parsed)
}

// bigIntOrDefault 读取十进制大整数，格式错误时返回 nil 并交由 Validate 拒绝。
func bigIntOrDefault(key, fallback string) *big.Int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		value = fallback
	}
	parsed, ok := new(big.Int).SetString(value, 10)
	if !ok {
		return nil
	}
	return parsed
}

// isTokenSymbol 校验 Token symbol 只包含大写字母和数字。
func isTokenSymbol(value string) bool {
	if len(value) < 2 || len(value) > 12 {
		return false
	}
	for _, character := range value {
		if (character < 'A' || character > 'Z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

// optionalUint64 读取可选无符号整数，空值表示继续使用数据库检查点。
func optionalUint64(key string) (*uint64, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%s 配置必须是非负整数", key)
	}
	return &parsed, nil
}
