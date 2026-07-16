package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	configVersion    = 6
	configDirectory  = "codex-quota-router"
	configFilename   = "config.dat"
	maxGatewayTokens = 20000
	maxOAuthTokenLen = 128 << 10

	strategyPriority       = "priority"
	strategyRoundRobin     = "round_robin"
	strategyLeastUsed      = "least_used"
	strategyHighestBalance = "highest_balance"
	strategyLowestBalance  = "lowest_balance"

	newAPIAuthAPIKey      = "api_key"
	newAPIAuthPassword    = "password"
	newAPIAuthAccessToken = "access_token"

	accountAuthAPIKey     = "api_key"
	accountAuthCodexOAuth = "codex_oauth"

	accountProviderAuto            = "auto"
	accountProviderNewAPI          = "new_api"
	accountProviderSub2API         = "sub2api"
	accountProviderOpenAIResponses = "openai_responses"
	accountProviderCodexOAuth      = "codex_oauth"

	quotaResetPeriodNever   = "never"
	quotaResetPeriodDaily   = "daily"
	quotaResetPeriodWeekly  = "weekly"
	quotaResetPeriodMonthly = "monthly"
	quotaResetPeriodCustom  = "custom"

	quotaResetHour  = "hour"
	quotaResetDay   = "day"
	quotaResetWeek  = "week"
	quotaResetMonth = "month"

	defaultQuotaResetTimezone = "Asia/Shanghai"
)

type accountConfig struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	AuthType            string `json:"authType,omitempty"`
	Provider            string `json:"provider,omitempty"`
	BaseURL             string `json:"baseUrl,omitempty"`
	APIKey              string `json:"apiKey,omitempty"`
	CodexAccessToken    string `json:"codexAccessToken,omitempty"`
	CodexRefreshToken   string `json:"codexRefreshToken,omitempty"`
	CodexIDToken        string `json:"codexIdToken,omitempty"`
	CodexAccountID      string `json:"codexAccountId,omitempty"`
	CodexEmail          string `json:"codexEmail,omitempty"`
	CodexExpiresAt      string `json:"codexExpiresAt,omitempty"`
	CodexPlanType       string `json:"codexPlanType,omitempty"`
	DisableCodexCredits bool   `json:"disableCodexCredits,omitempty"`
	QuotaResetPeriod    string `json:"quotaResetPeriod,omitempty"`
	QuotaResetTimezone  string `json:"quotaResetTimezone,omitempty"`
	QuotaResetEvery     int    `json:"quotaResetEvery,omitempty"`
	QuotaResetUnit      string `json:"quotaResetUnit,omitempty"`
	QuotaResetAnchorAt  string `json:"quotaResetAnchorAt,omitempty"`
	NewAPIAuthMode      string `json:"newApiAuthMode,omitempty"`
	NewAPIUsername      string `json:"newApiUsername,omitempty"`
	NewAPIUserID        int    `json:"newApiUserId,omitempty"`
	NewAPISecret        string `json:"newApiSecret,omitempty"`
	Enabled             bool   `json:"enabled"`
	Revision            int    `json:"revision"`
	Verified            bool   `json:"verified"`
	BlockedReason       string `json:"blockedReason,omitempty"`
}

type gatewayTokenConfig struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Token     string `json:"token"`
	CreatedAt string `json:"createdAt,omitempty"`
}

type storedConfig struct {
	Version           int                  `json:"version"`
	Accounts          []accountConfig      `json:"accounts"`
	Strategy          string               `json:"strategy"`
	AllowInsecureHTTP bool                 `json:"allowInsecureHttp"`
	GatewayTokens     []gatewayTokenConfig `json:"gatewayTokens,omitempty"`
	GatewayToken      string               `json:"gatewayToken,omitempty"`
	AllowPublicAccess bool                 `json:"allowPublicAccess"`
	PublicBaseURL     string               `json:"publicBaseUrl,omitempty"`
	AllowPublicAdmin  bool                 `json:"allowPublicAdmin"`
	AdminPasswordHash string               `json:"adminPasswordHash,omitempty"`
	LastSwitchReason  string               `json:"lastSwitchReason,omitempty"`
	LastSwitchAt      string               `json:"lastSwitchAt,omitempty"`
}

type legacyUpstream struct {
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey"`
}

type legacyStoredConfig struct {
	Version            int            `json:"version"`
	Primary            legacyUpstream `json:"primary"`
	Backup             legacyUpstream `json:"backup"`
	TestModel          string         `json:"testModel"`
	AllowInsecureHTTP  bool           `json:"allowInsecureHttp"`
	ForceBackup        bool           `json:"forceBackup"`
	PrimaryExhausted   bool           `json:"primaryExhausted"`
	PrimaryUnavailable bool           `json:"primaryUnavailable"`
	PrimaryVerified    bool           `json:"primaryVerified"`
	LastSwitchReason   string         `json:"lastSwitchReason,omitempty"`
	LastSwitchAt       string         `json:"lastSwitchAt,omitempty"`
	GatewayToken       string         `json:"gatewayToken"`
}

type accountInput struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	AuthType            string  `json:"authType"`
	Provider            *string `json:"provider"`
	BaseURL             string  `json:"baseUrl"`
	APIKey              string  `json:"apiKey"`
	QuotaResetPeriod    string  `json:"quotaResetPeriod"`
	QuotaResetTimezone  string  `json:"quotaResetTimezone"`
	QuotaResetEvery     int     `json:"quotaResetEvery"`
	QuotaResetUnit      string  `json:"quotaResetUnit"`
	QuotaResetAnchorAt  string  `json:"quotaResetAnchorAt"`
	NewAPIAuthMode      string  `json:"newApiAuthMode"`
	NewAPIUsername      string  `json:"newApiUsername"`
	NewAPIUserID        int     `json:"newApiUserId"`
	NewAPISecret        string  `json:"newApiSecret"`
	DisableCodexCredits bool    `json:"disableCodexCredits"`
	Enabled             *bool   `json:"enabled"`
	ClearAPIKey         bool    `json:"clearApiKey"`
	ClearNewAPISecret   bool    `json:"clearNewApiSecret"`
}

func defaultConfigPath() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("CODEX_QUOTA_ROUTER_CONFIG")); configured != "" {
		return filepath.Abs(configured)
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, configDirectory, configFilename), nil
}

func randomToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func randomAccountID() (string, error) {
	data := make([]byte, 12)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "acc_" + base64.RawURLEncoding.EncodeToString(data), nil
}

func newGatewayTokenConfig(name string, now time.Time) (gatewayTokenConfig, error) {
	idData := make([]byte, 12)
	if _, err := rand.Read(idData); err != nil {
		return gatewayTokenConfig{}, err
	}
	token, err := randomToken()
	if err != nil {
		return gatewayTokenConfig{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "网关 Token"
	}
	return gatewayTokenConfig{
		ID:   "tok_" + base64.RawURLEncoding.EncodeToString(idData),
		Name: name, Token: token, CreatedAt: now.UTC().Format(time.RFC3339),
	}, nil
}

func loadConfig(path string) (storedConfig, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return storedConfig{}, false, nil
	}
	if err != nil {
		return storedConfig{}, false, err
	}
	plain, err := unprotectConfig(data)
	if err != nil {
		return storedConfig{}, false, err
	}
	var metadata struct {
		Version  int             `json:"version"`
		Accounts json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(plain, &metadata); err != nil {
		return storedConfig{}, false, err
	}
	if metadata.Version > configVersion {
		return storedConfig{}, false, fmt.Errorf("配置版本 %d 高于当前程序支持的版本 %d", metadata.Version, configVersion)
	}
	if metadata.Version >= configVersion || metadata.Accounts != nil {
		var cfg storedConfig
		if err := json.Unmarshal(plain, &cfg); err != nil {
			return storedConfig{}, false, err
		}
		_ = os.Chmod(path, 0o600)
		return cfg, true, nil
	}
	var legacy legacyStoredConfig
	if err := json.Unmarshal(plain, &legacy); err != nil {
		return storedConfig{}, false, err
	}
	cfg := migrateLegacyConfig(legacy)
	if err := normalizeAndValidateConfig(&cfg); err != nil {
		return storedConfig{}, false, err
	}
	if metadata.Version == 5 && configVersion == 6 {
		if err := backupConfigVersion(path, metadata.Version); err != nil {
			return storedConfig{}, false, err
		}
	}
	if err := saveConfig(path, cfg); err != nil {
		return storedConfig{}, false, err
	}
	return cfg, true, nil
}

func migrateLegacyConfig(legacy legacyStoredConfig) storedConfig {
	primary := accountConfig{
		ID: "primary", Name: "主渠道", BaseURL: legacy.Primary.BaseURL, APIKey: legacy.Primary.APIKey,
		Enabled: true, Revision: 1, Verified: legacy.PrimaryVerified,
	}
	if legacy.PrimaryExhausted {
		primary.BlockedReason = "quota"
	} else if legacy.PrimaryUnavailable {
		primary.BlockedReason = "unauthorized"
	}
	backup := accountConfig{
		ID: "backup", Name: "备用渠道", BaseURL: legacy.Backup.BaseURL, APIKey: legacy.Backup.APIKey,
		Enabled: true, Revision: 1,
	}
	accounts := make([]accountConfig, 0, 2)
	appendAccount := func(account accountConfig) {
		if strings.TrimSpace(account.BaseURL) != "" || strings.TrimSpace(account.APIKey) != "" {
			accounts = append(accounts, account)
		}
	}
	if legacy.ForceBackup {
		primary.Enabled = false
		appendAccount(backup)
		appendAccount(primary)
	} else {
		appendAccount(primary)
		appendAccount(backup)
	}
	return storedConfig{
		Version:           configVersion,
		Accounts:          accounts,
		Strategy:          strategyPriority,
		AllowInsecureHTTP: legacy.AllowInsecureHTTP,
		GatewayTokens: func() []gatewayTokenConfig {
			if strings.TrimSpace(legacy.GatewayToken) == "" {
				return nil
			}
			return []gatewayTokenConfig{{ID: "gateway_legacy", Name: "默认 Token", Token: legacy.GatewayToken}}
		}(),
		LastSwitchReason: legacy.LastSwitchReason,
		LastSwitchAt:     legacy.LastSwitchAt,
	}
}

func saveConfig(path string, cfg storedConfig) error {
	return saveProtectedJSON(path, cfg)
}

func backupConfigVersion(path string, version int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	backupPath := fmt.Sprintf("%s.v%d.bak", path, version)
	backup, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		info, statErr := os.Lstat(backupPath)
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("配置备份路径不是普通文件: %s", backupPath)
		}
		existing, readErr := os.ReadFile(backupPath)
		if readErr != nil {
			return readErr
		}
		plain, unprotectErr := unprotectConfig(existing)
		if unprotectErr != nil {
			return fmt.Errorf("已有配置备份无效: %w", unprotectErr)
		}
		var metadata struct {
			Version int `json:"version"`
		}
		if jsonErr := json.Unmarshal(plain, &metadata); jsonErr != nil || metadata.Version != version {
			return fmt.Errorf("已有配置备份不是有效的 v%d 配置", version)
		}
		return nil
	}
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(backupPath)
		}
	}()
	written, err := backup.Write(data)
	if err != nil {
		_ = backup.Close()
		return err
	}
	if written != len(data) {
		_ = backup.Close()
		return errors.New("配置备份写入不完整")
	}
	if err := backup.Sync(); err != nil {
		_ = backup.Close()
		return err
	}
	if err := backup.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func saveProtectedJSON(path string, value any) error {
	plain, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	plain = append(plain, '\n')
	data, err := protectConfig(plain)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if filepath.Base(dir) == configDirectory {
		_ = os.Chmod(dir, 0o700)
	}
	temporary, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceFile(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func (a *application) writeConfig(cfg storedConfig) error {
	persist := a.persistConfig
	if persist == nil {
		persist = saveConfig
	}
	return persist(a.configPath, cfg)
}

func cloneConfig(cfg storedConfig) storedConfig {
	cloned := cfg
	cloned.Accounts = append([]accountConfig(nil), cfg.Accounts...)
	cloned.GatewayTokens = append([]gatewayTokenConfig(nil), cfg.GatewayTokens...)
	return cloned
}

func normalizeAndValidateConfig(cfg *storedConfig) error {
	sourceVersion := cfg.Version
	cfg.Version = configVersion
	cfg.GatewayToken = strings.TrimSpace(cfg.GatewayToken)
	if len(cfg.GatewayTokens) == 0 && cfg.GatewayToken != "" {
		cfg.GatewayTokens = []gatewayTokenConfig{{ID: "gateway_legacy", Name: "默认 Token", Token: cfg.GatewayToken}}
	}
	cfg.GatewayToken = ""
	if len(cfg.GatewayTokens) > maxGatewayTokens {
		return fmt.Errorf("网关 Token 数量不能超过 %d", maxGatewayTokens)
	}
	seenTokenIDs := make(map[string]bool, len(cfg.GatewayTokens))
	seenTokenValues := make(map[string]bool, len(cfg.GatewayTokens))
	for index := range cfg.GatewayTokens {
		token := &cfg.GatewayTokens[index]
		token.ID = strings.TrimSpace(token.ID)
		token.Name = strings.TrimSpace(token.Name)
		token.Token = strings.TrimSpace(token.Token)
		if token.ID == "" || len(token.ID) > 128 || seenTokenIDs[token.ID] {
			return fmt.Errorf("网关 Token ID 无效或重复")
		}
		if token.Name == "" {
			token.Name = fmt.Sprintf("网关 Token %d", index+1)
		}
		if len(token.Name) > 100 {
			return fmt.Errorf("网关 Token 名称不能超过 100 个字节")
		}
		if token.Token == "" || seenTokenValues[token.Token] {
			return fmt.Errorf("网关 Token 内容不能为空或重复")
		}
		seenTokenIDs[token.ID] = true
		seenTokenValues[token.Token] = true
	}
	cfg.PublicBaseURL = strings.TrimRight(strings.TrimSpace(cfg.PublicBaseURL), "/")
	if cfg.PublicBaseURL != "" {
		parsed, err := url.Parse(cfg.PublicBaseURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" ||
			parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("公网地址必须是仅包含协议、主机和可选端口的完整 HTTPS 地址")
		}
	}
	if cfg.AllowPublicAccess && cfg.PublicBaseURL == "" {
		return fmt.Errorf("允许公网访问时必须填写公网 HTTPS 地址")
	}
	if cfg.AllowPublicAccess && !validAdminPasswordHash(cfg.AdminPasswordHash) {
		return fmt.Errorf("允许公网访问时必须设置有效的管理员密码")
	}
	if cfg.AllowPublicAdmin {
		if !cfg.AllowPublicAccess {
			return fmt.Errorf("允许公网管理前必须先允许公网访问")
		}
	} else if cfg.AdminPasswordHash != "" && !validAdminPasswordHash(cfg.AdminPasswordHash) {
		return fmt.Errorf("管理员密码摘要无效")
	}
	cfg.Strategy = strings.TrimSpace(strings.ToLower(cfg.Strategy))
	if cfg.Strategy == "" {
		cfg.Strategy = strategyPriority
	}
	if !validStrategy(cfg.Strategy) {
		return fmt.Errorf("策略必须是 priority、round_robin、least_used、quota_reset、highest_balance 或 lowest_balance")
	}
	seen := make(map[string]bool, len(cfg.Accounts))
	for index := range cfg.Accounts {
		account := &cfg.Accounts[index]
		account.ID = strings.TrimSpace(account.ID)
		account.Name = strings.TrimSpace(account.Name)
		account.AuthType = normalizeAccountAuthType(account.AuthType)
		rawProvider := strings.TrimSpace(account.Provider)
		if account.AuthType == accountAuthCodexOAuth {
			account.Provider = ""
		} else if sourceVersion < 6 && rawProvider == "" && legacyAccountUsesNewAPIAuth(*account) {
			account.Provider = accountProviderNewAPI
		} else {
			account.Provider = normalizeAccountProvider(rawProvider)
		}
		account.BaseURL = strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
		account.APIKey = strings.TrimSpace(account.APIKey)
		account.CodexAccessToken = strings.TrimSpace(account.CodexAccessToken)
		account.CodexRefreshToken = strings.TrimSpace(account.CodexRefreshToken)
		account.CodexIDToken = strings.TrimSpace(account.CodexIDToken)
		account.CodexAccountID = strings.TrimSpace(account.CodexAccountID)
		account.CodexEmail = strings.TrimSpace(account.CodexEmail)
		account.CodexExpiresAt = strings.TrimSpace(account.CodexExpiresAt)
		account.CodexPlanType = strings.TrimSpace(account.CodexPlanType)
		account.BlockedReason = strings.TrimSpace(strings.ToLower(account.BlockedReason))
		if account.ID == "" {
			return fmt.Errorf("账号 ID 不能为空")
		}
		if len(account.ID) > 128 {
			return fmt.Errorf("账号 ID 过长")
		}
		if seen[account.ID] {
			return fmt.Errorf("账号 ID 重复：%s", account.ID)
		}
		seen[account.ID] = true
		if account.Name == "" {
			account.Name = fmt.Sprintf("账号 %d", index+1)
		}
		if account.Revision < 1 {
			account.Revision = 1
		}
		if !validAccountAuthType(account.AuthType) {
			return fmt.Errorf("账号 %s 的登录方式无效", account.Name)
		}
		if account.BlockedReason != "" && account.BlockedReason != "quota" && account.BlockedReason != "unauthorized" &&
			account.BlockedReason != "restricted" {
			return fmt.Errorf("账号 %s 的阻塞原因无效", account.Name)
		}
		if account.AuthType == accountAuthCodexOAuth {
			account.BaseURL = ""
			account.APIKey = ""
			clearQuotaReset(account)
			account.NewAPIAuthMode = newAPIAuthAPIKey
			account.NewAPIUsername = ""
			account.NewAPIUserID = 0
			account.NewAPISecret = ""
			if err := validateCodexAuthFields(*account); err != nil {
				return fmt.Errorf("账号 %s：%w", account.Name, err)
			}
			continue
		}
		account.DisableCodexCredits = false
		if !validAPIKeyProvider(account.Provider) {
			return fmt.Errorf("账号 %s 的 Provider 无效", account.Name)
		}
		clearCodexAuth(account)
		if !providerAllowsNewAPIAuth(account.Provider) {
			account.NewAPIAuthMode = newAPIAuthAPIKey
			account.NewAPIUsername = ""
			account.NewAPIUserID = 0
			account.NewAPISecret = ""
		}
		rawAuthMode := strings.TrimSpace(strings.ToLower(account.NewAPIAuthMode))
		if !validNewAPIAuthMode(rawAuthMode) {
			return fmt.Errorf("账号 %s 的 New API 余额认证方式无效", account.Name)
		}
		account.NewAPIAuthMode = normalizeNewAPIAuthMode(rawAuthMode)
		account.NewAPIUsername = strings.TrimSpace(account.NewAPIUsername)
		if account.NewAPIAuthMode == newAPIAuthAccessToken {
			account.NewAPISecret = normalizeBearerToken(account.NewAPISecret)
		}
		switch account.NewAPIAuthMode {
		case newAPIAuthAPIKey:
			account.NewAPIUsername = ""
			account.NewAPIUserID = 0
			account.NewAPISecret = ""
			if err := normalizeQuotaReset(account); err != nil {
				return fmt.Errorf("账号 %s：%w", account.Name, err)
			}
		case newAPIAuthPassword:
			clearQuotaReset(account)
			account.NewAPIUserID = 0
			if account.NewAPIUsername == "" || account.NewAPISecret == "" {
				return fmt.Errorf("账号 %s 的 New API 用户名和密码不能为空", account.Name)
			}
		case newAPIAuthAccessToken:
			clearQuotaReset(account)
			account.NewAPIUsername = ""
			if account.NewAPIUserID <= 0 || account.NewAPISecret == "" {
				return fmt.Errorf("账号 %s 的 New API 用户 ID 和 Access Token 不能为空", account.Name)
			}
		}
		if account.BaseURL == "" {
			continue
		}
		if err := validateBaseURL(account.Name, account.BaseURL, cfg.AllowInsecureHTTP); err != nil {
			return err
		}
	}
	return nil
}

func validateBaseURL(name, value string, allowInsecureHTTP bool) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s URL 必须是完整的 http:// 或 https:// 地址", name)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s URL 不能包含用户信息、查询参数或片段", name)
	}
	if parsed.Scheme == "http" && !allowInsecureHTTP {
		return fmt.Errorf("%s 使用明文 HTTP，需先明确允许不安全 HTTP", name)
	}
	return nil
}

func baseURLMovesToDifferentOrigin(previous, next string) bool {
	previous = strings.TrimSpace(previous)
	next = strings.TrimSpace(next)
	if next == "" || previous == next {
		return false
	}
	previousOrigin, previousOK := normalizedOrigin(previous)
	nextOrigin, nextOK := normalizedOrigin(next)
	if !nextOK {
		return false
	}
	return !previousOK || previousOrigin != nextOrigin
}

func normalizedOrigin(value string) (string, bool) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	port := parsed.Port()
	if port == "" {
		if scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	return scheme + "://" + strings.ToLower(parsed.Hostname()) + ":" + port, true
}

func normalizeAccountAuthType(authType string) string {
	authType = strings.TrimSpace(strings.ToLower(authType))
	if authType == "" {
		return accountAuthAPIKey
	}
	return authType
}

func validAccountAuthType(authType string) bool {
	return authType == accountAuthAPIKey || authType == accountAuthCodexOAuth
}

func normalizeAccountProvider(provider string) string {
	provider = strings.TrimSpace(strings.ToLower(provider))
	if provider == "" {
		return accountProviderAuto
	}
	return provider
}

func validAPIKeyProvider(provider string) bool {
	return provider == accountProviderAuto || provider == accountProviderNewAPI ||
		provider == accountProviderSub2API || provider == accountProviderOpenAIResponses
}

func effectiveAccountProvider(account accountConfig) string {
	if normalizeAccountAuthType(account.AuthType) == accountAuthCodexOAuth {
		return accountProviderCodexOAuth
	}
	return normalizeAccountProvider(account.Provider)
}

func providerAllowsNewAPIAuth(provider string) bool {
	provider = normalizeAccountProvider(provider)
	return provider == accountProviderAuto || provider == accountProviderNewAPI
}

func legacyAccountUsesNewAPIAuth(account accountConfig) bool {
	mode := strings.TrimSpace(strings.ToLower(account.NewAPIAuthMode))
	return mode == newAPIAuthPassword || mode == newAPIAuthAccessToken ||
		strings.TrimSpace(account.NewAPIUsername) != "" || account.NewAPIUserID > 0 ||
		strings.TrimSpace(account.NewAPISecret) != ""
}

func clearCodexAuth(account *accountConfig) {
	account.CodexAccessToken = ""
	account.CodexRefreshToken = ""
	account.CodexIDToken = ""
	account.CodexAccountID = ""
	account.CodexEmail = ""
	account.CodexExpiresAt = ""
	account.CodexPlanType = ""
}

func validateCodexAuthFields(account accountConfig) error {
	values := []string{
		account.CodexAccessToken, account.CodexRefreshToken, account.CodexIDToken,
		account.CodexAccountID, account.CodexEmail, account.CodexExpiresAt, account.CodexPlanType,
	}
	for _, value := range values {
		if len(value) > maxOAuthTokenLen {
			return errors.New("Codex OAuth 凭据过长")
		}
	}
	configured := account.CodexAccessToken != "" || account.CodexRefreshToken != "" ||
		account.CodexIDToken != "" || account.CodexAccountID != ""
	if !configured {
		return nil
	}
	if account.CodexAccessToken == "" || account.CodexRefreshToken == "" || account.CodexAccountID == "" {
		return errors.New("Codex OAuth 凭据不完整，请重新登录")
	}
	if account.CodexExpiresAt != "" {
		if _, err := time.Parse(time.RFC3339, account.CodexExpiresAt); err != nil {
			return errors.New("Codex OAuth 过期时间无效")
		}
	}
	return nil
}

func normalizeQuotaReset(account *accountConfig) error {
	account.QuotaResetPeriod = strings.TrimSpace(strings.ToLower(account.QuotaResetPeriod))
	account.QuotaResetTimezone = strings.TrimSpace(account.QuotaResetTimezone)
	account.QuotaResetUnit = strings.TrimSpace(strings.ToLower(account.QuotaResetUnit))
	account.QuotaResetAnchorAt = strings.TrimSpace(account.QuotaResetAnchorAt)
	if account.QuotaResetPeriod == "" {
		if account.QuotaResetEvery > 0 {
			account.QuotaResetPeriod = quotaResetPeriodCustom
		} else if account.QuotaResetEvery < 0 {
			return errors.New("额度重置周期必须是 1 到 100000")
		} else {
			account.QuotaResetPeriod = quotaResetPeriodNever
		}
	}
	switch account.QuotaResetPeriod {
	case quotaResetPeriodNever:
		clearQuotaReset(account)
		return nil
	case quotaResetPeriodDaily, quotaResetPeriodWeekly, quotaResetPeriodMonthly:
		account.QuotaResetEvery = 0
		account.QuotaResetUnit = ""
		account.QuotaResetAnchorAt = ""
		if account.QuotaResetTimezone == "" {
			account.QuotaResetTimezone = defaultQuotaResetTimezone
		}
		if _, err := time.LoadLocation(account.QuotaResetTimezone); err != nil {
			return errors.New("额度重置时区无效")
		}
		return nil
	case quotaResetPeriodCustom:
	default:
		return errors.New("额度重置规则必须是 never、daily、weekly、monthly 或 custom")
	}
	if account.QuotaResetEvery <= 0 || account.QuotaResetEvery > 100000 {
		return errors.New("额度重置周期必须是 1 到 100000")
	}
	if account.QuotaResetUnit != quotaResetHour && account.QuotaResetUnit != quotaResetDay &&
		account.QuotaResetUnit != quotaResetWeek && account.QuotaResetUnit != quotaResetMonth {
		return errors.New("额度重置单位必须是 hour、day、week 或 month")
	}
	if account.QuotaResetUnit == quotaResetMonth {
		if account.QuotaResetTimezone == "" {
			account.QuotaResetTimezone = defaultQuotaResetTimezone
		}
		if _, err := time.LoadLocation(account.QuotaResetTimezone); err != nil {
			return errors.New("额度重置时区无效")
		}
	} else {
		account.QuotaResetTimezone = ""
		if _, err := quotaResetInterval(account.QuotaResetEvery, account.QuotaResetUnit); err != nil {
			return errors.New("额度重置周期超出程序支持范围")
		}
	}
	if _, err := parseQuotaResetAnchorAt(account.QuotaResetAnchorAt); err != nil {
		return errors.New("额度重置锚点必须是 RFC3339 时间")
	}
	return nil
}

func clearQuotaReset(account *accountConfig) {
	account.QuotaResetPeriod = quotaResetPeriodNever
	account.QuotaResetTimezone = ""
	account.QuotaResetEvery = 0
	account.QuotaResetUnit = ""
	account.QuotaResetAnchorAt = ""
}

func validStrategy(strategy string) bool {
	return strategy == strategyPriority || strategy == strategyRoundRobin ||
		strategy == strategyLeastUsed || strategy == strategyQuotaReset || isBalanceStrategy(strategy)
}

func isBalanceStrategy(strategy string) bool {
	return strategy == strategyHighestBalance || strategy == strategyLowestBalance
}

func normalizeNewAPIAuthMode(mode string) string {
	mode = strings.TrimSpace(strings.ToLower(mode))
	if mode == "" {
		return newAPIAuthAPIKey
	}
	return mode
}

func validNewAPIAuthMode(mode string) bool {
	return mode == "" || mode == newAPIAuthAPIKey || mode == newAPIAuthPassword || mode == newAPIAuthAccessToken
}

func normalizeAccountInput(input *accountInput) error {
	input.ID = strings.TrimSpace(input.ID)
	input.Name = strings.TrimSpace(input.Name)
	input.AuthType = normalizeAccountAuthType(input.AuthType)
	if !validAccountAuthType(input.AuthType) {
		return fmt.Errorf("账号登录方式无效")
	}
	if input.Provider != nil {
		provider := strings.TrimSpace(strings.ToLower(*input.Provider))
		if input.AuthType == accountAuthCodexOAuth {
			if provider == "" {
				provider = accountProviderCodexOAuth
			}
			if provider != accountProviderCodexOAuth {
				return fmt.Errorf("Codex OAuth 账号的 Provider 必须是 codex_oauth")
			}
		} else if !validAPIKeyProvider(provider) {
			return fmt.Errorf("API Key 账号必须选择有效的 Provider")
		}
		input.Provider = &provider
	}
	input.BaseURL = strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	input.APIKey = strings.TrimSpace(input.APIKey)
	input.QuotaResetPeriod = strings.TrimSpace(strings.ToLower(input.QuotaResetPeriod))
	input.QuotaResetTimezone = strings.TrimSpace(input.QuotaResetTimezone)
	input.QuotaResetUnit = strings.TrimSpace(strings.ToLower(input.QuotaResetUnit))
	input.QuotaResetAnchorAt = strings.TrimSpace(input.QuotaResetAnchorAt)
	input.NewAPIUsername = strings.TrimSpace(input.NewAPIUsername)
	if input.AuthType == accountAuthCodexOAuth {
		input.BaseURL = ""
		input.APIKey = ""
		input.QuotaResetPeriod = quotaResetPeriodNever
		input.QuotaResetTimezone = ""
		input.QuotaResetEvery = 0
		input.QuotaResetUnit = ""
		input.QuotaResetAnchorAt = ""
		input.NewAPIAuthMode = newAPIAuthAPIKey
		input.NewAPIUsername = ""
		input.NewAPIUserID = 0
		input.NewAPISecret = ""
		input.ClearAPIKey = false
		input.ClearNewAPISecret = false
		return nil
	}
	input.DisableCodexCredits = false
	mode := strings.TrimSpace(strings.ToLower(input.NewAPIAuthMode))
	if !validNewAPIAuthMode(mode) {
		return fmt.Errorf("New API 余额认证方式无效")
	}
	input.NewAPIAuthMode = normalizeNewAPIAuthMode(mode)
	if input.NewAPIAuthMode == newAPIAuthAccessToken {
		input.NewAPISecret = normalizeBearerToken(input.NewAPISecret)
	}
	quotaReset := accountConfig{
		QuotaResetPeriod: input.QuotaResetPeriod, QuotaResetTimezone: input.QuotaResetTimezone,
		QuotaResetEvery: input.QuotaResetEvery, QuotaResetUnit: input.QuotaResetUnit,
		QuotaResetAnchorAt: input.QuotaResetAnchorAt,
	}
	if input.NewAPIAuthMode == newAPIAuthAPIKey {
		if err := normalizeQuotaReset(&quotaReset); err != nil {
			return err
		}
	} else {
		clearQuotaReset(&quotaReset)
	}
	input.QuotaResetPeriod = quotaReset.QuotaResetPeriod
	input.QuotaResetTimezone = quotaReset.QuotaResetTimezone
	input.QuotaResetEvery = quotaReset.QuotaResetEvery
	input.QuotaResetUnit = quotaReset.QuotaResetUnit
	input.QuotaResetAnchorAt = quotaReset.QuotaResetAnchorAt
	return nil
}

func normalizeBearerToken(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 7 && strings.EqualFold(value[:7], "Bearer ") {
		return strings.TrimSpace(value[7:])
	}
	return value
}

func applyNewAPIAuthInput(account *accountConfig, previous accountConfig, input accountInput) error {
	mode := input.NewAPIAuthMode
	if mode == newAPIAuthAPIKey {
		account.NewAPIAuthMode = mode
		account.NewAPIUsername = ""
		account.NewAPIUserID = 0
		account.NewAPISecret = ""
		return nil
	}

	previousMode := normalizeNewAPIAuthMode(previous.NewAPIAuthMode)
	identifierChanged := previousMode != mode
	switch mode {
	case newAPIAuthPassword:
		identifierChanged = identifierChanged || previous.NewAPIUsername != input.NewAPIUsername
		account.NewAPIUsername = input.NewAPIUsername
		account.NewAPIUserID = 0
	case newAPIAuthAccessToken:
		identifierChanged = identifierChanged || previous.NewAPIUserID != input.NewAPIUserID
		account.NewAPIUsername = ""
		account.NewAPIUserID = input.NewAPIUserID
	}

	secret := input.NewAPISecret
	if input.ClearNewAPISecret {
		secret = ""
	} else if secret == "" && !identifierChanged {
		secret = previous.NewAPISecret
	}
	if secret == "" {
		if mode == newAPIAuthPassword {
			return fmt.Errorf("New API 用户名或密码变化时必须重新填写密码")
		}
		return fmt.Errorf("New API 用户 ID 或认证方式变化时必须重新填写 Access Token")
	}
	account.NewAPIAuthMode = mode
	account.NewAPISecret = secret
	return nil
}

func providerForAccountInput(previous accountConfig, input accountInput) (string, error) {
	if input.AuthType == accountAuthCodexOAuth {
		return "", nil
	}
	previousIsAPIKey := previous.ID != "" && normalizeAccountAuthType(previous.AuthType) == accountAuthAPIKey
	provider := ""
	if input.Provider == nil {
		if !previousIsAPIKey {
			return "", errors.New("新 API Key 账号必须选择 Provider")
		}
		provider = normalizeAccountProvider(previous.Provider)
	} else {
		provider = *input.Provider
	}
	if provider == accountProviderAuto {
		if !previousIsAPIKey || normalizeAccountProvider(previous.Provider) != accountProviderAuto {
			return "", errors.New("auto 仅用于兼容旧账号，请选择实际 Provider")
		}
		if input.APIKey != "" {
			return "", errors.New("填写或更换 API Key 时必须选择实际 Provider")
		}
	}
	return provider, nil
}

func mergeAccountInput(previous accountConfig, input accountInput, includeBalanceAuth bool) (accountConfig, error) {
	if err := normalizeAccountInput(&input); err != nil {
		return accountConfig{}, err
	}
	provider, err := providerForAccountInput(previous, input)
	if err != nil {
		return accountConfig{}, err
	}
	previousAuthType := normalizeAccountAuthType(previous.AuthType)
	authTypeChanged := previousAuthType != input.AuthType
	account := previous
	account.Name = input.Name
	account.AuthType = input.AuthType
	account.Provider = provider
	if input.Enabled != nil {
		account.Enabled = *input.Enabled
	}
	if input.AuthType == accountAuthCodexOAuth {
		account.BaseURL = ""
		account.APIKey = ""
		account.DisableCodexCredits = input.DisableCodexCredits
		clearQuotaReset(&account)
		account.NewAPIAuthMode = newAPIAuthAPIKey
		account.NewAPIUsername = ""
		account.NewAPIUserID = 0
		account.NewAPISecret = ""
		if authTypeChanged {
			clearCodexAuth(&account)
		}
		return account, nil
	}
	if authTypeChanged {
		clearCodexAuth(&account)
		account.BaseURL = ""
		account.APIKey = ""
		account.NewAPIAuthMode = newAPIAuthAPIKey
		account.NewAPIUsername = ""
		account.NewAPIUserID = 0
		account.NewAPISecret = ""
	}
	account.DisableCodexCredits = false
	originChanged := previousAuthType == accountAuthAPIKey && baseURLMovesToDifferentOrigin(previous.BaseURL, input.BaseURL)
	if previous.APIKey != "" && input.APIKey == "" && !input.ClearAPIKey && originChanged {
		return accountConfig{}, errors.New("Base URL 更换到不同来源时，必须重新填写 API Key 或明确清除旧 Key")
	}
	if includeBalanceAuth && providerAllowsNewAPIAuth(account.Provider) && previous.NewAPISecret != "" && input.NewAPISecret == "" &&
		!input.ClearNewAPISecret && input.NewAPIAuthMode != newAPIAuthAPIKey && originChanged {
		return accountConfig{}, errors.New("Base URL 更换到不同来源时，必须重新填写 New API 余额凭据或切换为仅 API Key")
	}

	account.BaseURL = input.BaseURL
	account.QuotaResetPeriod = input.QuotaResetPeriod
	account.QuotaResetTimezone = input.QuotaResetTimezone
	account.QuotaResetEvery = input.QuotaResetEvery
	account.QuotaResetUnit = input.QuotaResetUnit
	account.QuotaResetAnchorAt = input.QuotaResetAnchorAt
	if input.ClearAPIKey {
		account.APIKey = ""
	} else if input.APIKey != "" {
		account.APIKey = input.APIKey
	}
	if includeBalanceAuth {
		if providerAllowsNewAPIAuth(account.Provider) {
			if err := applyNewAPIAuthInput(&account, previous, input); err != nil {
				return accountConfig{}, err
			}
		} else {
			account.NewAPIAuthMode = newAPIAuthAPIKey
			account.NewAPIUsername = ""
			account.NewAPIUserID = 0
			account.NewAPISecret = ""
		}
	} else if !providerAllowsNewAPIAuth(account.Provider) {
		account.NewAPIAuthMode = newAPIAuthAPIKey
		account.NewAPIUsername = ""
		account.NewAPIUserID = 0
		account.NewAPISecret = ""
	}
	return account, nil
}

func newAPIAuthChanged(left, right accountConfig) bool {
	return normalizeNewAPIAuthMode(left.NewAPIAuthMode) != normalizeNewAPIAuthMode(right.NewAPIAuthMode) ||
		left.NewAPIUsername != right.NewAPIUsername || left.NewAPIUserID != right.NewAPIUserID ||
		left.NewAPISecret != right.NewAPISecret
}

func proxyAuthChanged(left, right accountConfig) bool {
	if normalizeAccountAuthType(left.AuthType) != normalizeAccountAuthType(right.AuthType) {
		return true
	}
	if effectiveAccountProvider(left) != effectiveAccountProvider(right) {
		return true
	}
	if normalizeAccountAuthType(left.AuthType) == accountAuthCodexOAuth {
		return left.CodexAccountID != right.CodexAccountID
	}
	return left.BaseURL != right.BaseURL || left.APIKey != right.APIKey
}

func quotaResetChanged(left, right accountConfig) bool {
	if normalizeQuotaReset(&left) != nil || normalizeQuotaReset(&right) != nil {
		return true
	}
	return left.QuotaResetPeriod != right.QuotaResetPeriod || left.QuotaResetTimezone != right.QuotaResetTimezone ||
		left.QuotaResetEvery != right.QuotaResetEvery || left.QuotaResetUnit != right.QuotaResetUnit ||
		left.QuotaResetAnchorAt != right.QuotaResetAnchorAt
}

func codexAuthenticated(account accountConfig) bool {
	return normalizeAccountAuthType(account.AuthType) == accountAuthCodexOAuth &&
		account.CodexAccessToken != "" && account.CodexRefreshToken != "" && account.CodexAccountID != ""
}

func accountConfigured(account accountConfig) bool {
	if normalizeAccountAuthType(account.AuthType) == accountAuthCodexOAuth {
		return codexAuthenticated(account)
	}
	return account.BaseURL != "" && account.APIKey != ""
}

func sameAccountRevision(current, expected accountConfig) bool {
	if current.ID != expected.ID || current.Revision != expected.Revision ||
		normalizeAccountAuthType(current.AuthType) != normalizeAccountAuthType(expected.AuthType) {
		return false
	}
	if normalizeAccountAuthType(current.AuthType) == accountAuthCodexOAuth {
		return current.CodexAccountID == expected.CodexAccountID &&
			current.DisableCodexCredits == expected.DisableCodexCredits
	}
	return current.BaseURL == expected.BaseURL && current.APIKey == expected.APIKey &&
		effectiveAccountProvider(current) == effectiveAccountProvider(expected) &&
		normalizeNewAPIAuthMode(current.NewAPIAuthMode) == normalizeNewAPIAuthMode(expected.NewAPIAuthMode) &&
		current.NewAPIUsername == expected.NewAPIUsername && current.NewAPIUserID == expected.NewAPIUserID &&
		current.NewAPISecret == expected.NewAPISecret
}
