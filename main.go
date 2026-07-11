package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	listenAddress      = "127.0.0.1:4000"
	applicationVersion = "0.2.1"
	configVersion      = 2
	configDirectory    = "codex-quota-router"
	configFilename     = "config.dat"
	maxAdminBody       = 1 << 20
	maxErrorBody       = 1 << 20
	maxProxyBody       = 128 << 20
	accountCooldown    = time.Minute
	balanceTTL         = 5 * time.Minute
	balanceAutoTTL     = time.Minute
	balanceRefreshTime = 8 * time.Second
	balanceWorkers     = 8
	upstreamTestTime   = 30 * time.Second
)

const (
	strategyPriority       = "priority"
	strategyRoundRobin     = "round_robin"
	strategyLeastUsed      = "least_used"
	strategyHighestBalance = "highest_balance"
)

//go:embed web/index.html
var indexHTML []byte

type accountConfig struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	BaseURL       string `json:"baseUrl"`
	APIKey        string `json:"apiKey"`
	Enabled       bool   `json:"enabled"`
	Revision      int    `json:"revision"`
	Verified      bool   `json:"verified"`
	BlockedReason string `json:"blockedReason,omitempty"`
}

type storedConfig struct {
	Version           int             `json:"version"`
	Accounts          []accountConfig `json:"accounts"`
	Strategy          string          `json:"strategy"`
	TestModel         string          `json:"testModel"`
	AllowInsecureHTTP bool            `json:"allowInsecureHttp"`
	GatewayToken      string          `json:"gatewayToken"`
	LastSwitchReason  string          `json:"lastSwitchReason,omitempty"`
	LastSwitchAt      string          `json:"lastSwitchAt,omitempty"`
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
	ID          string `json:"id"`
	Name        string `json:"name"`
	BaseURL     string `json:"baseUrl"`
	APIKey      string `json:"apiKey"`
	Enabled     *bool  `json:"enabled"`
	ClearAPIKey bool   `json:"clearApiKey"`
}

type saveRequest struct {
	Accounts           *[]accountInput `json:"accounts"`
	Strategy           *string         `json:"strategy"`
	TestModel          *string         `json:"testModel"`
	AllowInsecureHTTP  *bool           `json:"allowInsecureHttp"`
	RotateGatewayToken bool            `json:"rotateGatewayToken"`
}

type testRequest struct {
	AccountID         string        `json:"accountId"`
	Candidate         *accountInput `json:"candidate"`
	TestModel         string        `json:"testModel"`
	AllowInsecureHTTP *bool         `json:"allowInsecureHttp"`
}

type publicAccount struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	BaseURL       string `json:"baseUrl"`
	Enabled       bool   `json:"enabled"`
	Revision      int    `json:"revision"`
	Verified      bool   `json:"verified"`
	BlockedReason string `json:"blockedReason,omitempty"`
	KeyConfigured bool   `json:"keyConfigured"`
}

type publicConfig struct {
	Version                int             `json:"version"`
	Accounts               []publicAccount `json:"accounts"`
	Strategy               string          `json:"strategy"`
	TestModel              string          `json:"testModel"`
	AllowInsecureHTTP      bool            `json:"allowInsecureHttp"`
	GatewayTokenConfigured bool            `json:"gatewayTokenConfigured"`
	LastSwitchReason       string          `json:"lastSwitchReason,omitempty"`
	LastSwitchAt           string          `json:"lastSwitchAt,omitempty"`
}

type balanceSnapshot struct {
	Status       string
	Amount       float64
	Unit         string
	DisplayLabel string
	Unlimited    bool
	UpdatedAt    time.Time
}

type publicBalance struct {
	Status       string  `json:"status"`
	Amount       float64 `json:"amount"`
	Unit         string  `json:"unit,omitempty"`
	DisplayLabel string  `json:"displayLabel,omitempty"`
	Unlimited    bool    `json:"unlimited"`
	UpdatedAt    string  `json:"updatedAt,omitempty"`
	Fresh        bool    `json:"fresh"`
}

type accountRuntime struct {
	Revision         int
	CooldownUntil    time.Time
	AssignedRequests uint64
	LastUsedAt       time.Time
	Balance          balanceSnapshot
}

type accountStatus struct {
	ID               string        `json:"id"`
	Name             string        `json:"name"`
	State            string        `json:"state"`
	Verified         bool          `json:"verified"`
	BlockedReason    string        `json:"blockedReason,omitempty"`
	CoolingDown      bool          `json:"coolingDown"`
	CooldownUntil    string        `json:"cooldownUntil,omitempty"`
	AssignedRequests uint64        `json:"assignedRequests"`
	LastUsedAt       string        `json:"lastUsedAt,omitempty"`
	Balance          publicBalance `json:"balance"`
}

type routerStatus struct {
	Strategy              string          `json:"strategy"`
	EffectiveStrategy     string          `json:"effectiveStrategy"`
	FallbackReason        string          `json:"fallbackReason,omitempty"`
	LastRoutedAccountID   string          `json:"lastRoutedAccountId,omitempty"`
	LastRoutedAccountName string          `json:"lastRoutedAccountName,omitempty"`
	AvailableAccounts     int             `json:"availableAccounts"`
	TotalAccounts         int             `json:"totalAccounts"`
	Accounts              []accountStatus `json:"accounts"`
	LastSwitchReason      string          `json:"lastSwitchReason,omitempty"`
	LastSwitchAt          string          `json:"lastSwitchAt,omitempty"`
}

type application struct {
	mu                    sync.Mutex
	balanceRefreshMu      sync.Mutex
	cfg                   storedConfig
	configPath            string
	csrfToken             string
	client                *http.Client
	now                   func() time.Time
	runtime               map[string]*accountRuntime
	roundRobinCursor      int
	lastRoutedAccountID   string
	lastRoutedAccountName string
}

func main() {
	if err := run(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		os.Exit(1)
	}
}

func run() error {
	app, err := newApplication("", nil, time.Now)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              listenAddress,
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	return server.ListenAndServe()
}

func newApplication(path string, client *http.Client, now func() time.Time) (*application, error) {
	if path == "" {
		var err error
		path, err = defaultConfigPath()
		if err != nil {
			return nil, err
		}
	}
	if now == nil {
		now = time.Now
	}
	if client == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		client = &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           dialer.DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          32,
				MaxIdleConnsPerHost:   8,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	cfg, found, err := loadConfig(path)
	if err != nil {
		return nil, err
	}
	changed := false
	if !found {
		cfg = storedConfig{Version: configVersion, Strategy: strategyPriority, TestModel: "gpt-5.6-sol"}
		changed = true
	}
	if cfg.Version < configVersion {
		cfg.Version = configVersion
		changed = true
	}
	if cfg.Strategy == "" {
		cfg.Strategy = strategyPriority
		changed = true
	}
	if cfg.GatewayToken == "" {
		cfg.GatewayToken, err = randomToken()
		if err != nil {
			return nil, err
		}
		changed = true
	}
	if err := normalizeAndValidateConfig(&cfg); err != nil {
		return nil, err
	}
	if changed {
		if err := saveConfig(path, cfg); err != nil {
			return nil, err
		}
	}
	csrf, err := randomToken()
	if err != nil {
		return nil, err
	}
	app := &application{
		cfg:        cfg,
		configPath: path,
		csrfToken:  csrf,
		client:     client,
		now:        now,
		runtime:    make(map[string]*accountRuntime),
	}
	for _, account := range cfg.Accounts {
		app.runtime[account.ID] = &accountRuntime{Revision: account.Revision}
	}
	return app, nil
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
		TestModel:         legacy.TestModel,
		AllowInsecureHTTP: legacy.AllowInsecureHTTP,
		GatewayToken:      legacy.GatewayToken,
		LastSwitchReason:  legacy.LastSwitchReason,
		LastSwitchAt:      legacy.LastSwitchAt,
	}
}

func saveConfig(path string, cfg storedConfig) error {
	plain, err := json.MarshalIndent(cfg, "", "  ")
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

func cloneConfig(cfg storedConfig) storedConfig {
	cloned := cfg
	cloned.Accounts = append([]accountConfig(nil), cfg.Accounts...)
	return cloned
}

func normalizeAndValidateConfig(cfg *storedConfig) error {
	cfg.Version = configVersion
	cfg.Strategy = strings.TrimSpace(strings.ToLower(cfg.Strategy))
	if cfg.Strategy == "" {
		cfg.Strategy = strategyPriority
	}
	if !validStrategy(cfg.Strategy) {
		return fmt.Errorf("策略必须是 priority、round_robin、least_used 或 highest_balance")
	}
	cfg.TestModel = strings.TrimSpace(cfg.TestModel)
	seen := make(map[string]bool, len(cfg.Accounts))
	for index := range cfg.Accounts {
		account := &cfg.Accounts[index]
		account.ID = strings.TrimSpace(account.ID)
		account.Name = strings.TrimSpace(account.Name)
		account.BaseURL = strings.TrimRight(strings.TrimSpace(account.BaseURL), "/")
		account.APIKey = strings.TrimSpace(account.APIKey)
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
		if account.BlockedReason != "" && account.BlockedReason != "quota" && account.BlockedReason != "unauthorized" {
			return fmt.Errorf("账号 %s 的阻塞原因无效", account.Name)
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

func validStrategy(strategy string) bool {
	return strategy == strategyPriority || strategy == strategyRoundRobin ||
		strategy == strategyLeastUsed || strategy == strategyHighestBalance
}

func (a *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/v1", a.handleProxy)
	mux.HandleFunc("/v1/", a.handleProxy)
	mux.HandleFunc("/admin/", a.handleAdmin)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validLocalHost(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

func validLocalHost(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || port != "4000" {
		return false
	}
	return host == "127.0.0.1" || strings.EqualFold(host, "localhost")
}

func validLocalOrigin(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return validLocalHost(parsed.Host)
}

func (a *application) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(indexHTML)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(indexHTML)
	}
}

func (a *application) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "codex-quota-router", "version": applicationVersion})
}

func (a *application) handleAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if origin := r.Header.Get("Origin"); origin != "" && !validLocalOrigin(origin) {
		writeAdminError(w, http.StatusForbidden, "不允许的请求来源")
		return
	}
	unsafeMethod := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
	if unsafeMethod {
		if !validLocalOrigin(r.Header.Get("Origin")) {
			writeAdminError(w, http.StatusForbidden, "写操作必须来自本地管理页")
			return
		}
		if !constantEqual(r.Header.Get("X-CSRF-Token"), a.csrfToken) {
			writeAdminError(w, http.StatusForbidden, "CSRF 校验失败")
			return
		}
	}
	endpoint := strings.TrimPrefix(r.URL.Path, "/admin")
	endpoint = strings.TrimPrefix(endpoint, "/api")
	switch {
	case endpoint == "/bootstrap" && r.Method == http.MethodGet:
		a.handleBootstrap(w)
	case endpoint == "/status" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, a.status())
	case endpoint == "/config" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, a.publicConfig())
	case endpoint == "/config" && r.Method == http.MethodPut:
		a.handleSave(w, r)
	case endpoint == "/save" && r.Method == http.MethodPost:
		a.handleSave(w, r)
	case endpoint == "/codex" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"snippet": a.codexSnippet()})
	case endpoint == "/codex-config" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"snippet": a.codexSnippet()})
	case endpoint == "/test" && r.Method == http.MethodPost:
		a.handleTest(w, r)
	case endpoint == "/balances/refresh" && r.Method == http.MethodPost:
		a.handleBalancesRefresh(w, r)
	case endpoint == "/accounts/reset" && r.Method == http.MethodPost:
		a.handleAccountReset(w, r)
	default:
		writeAdminError(w, http.StatusNotFound, "管理接口不存在")
	}
}

func constantEqual(left, right string) bool {
	return left != "" && right != "" && len(left) == len(right) &&
		subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (a *application) handleBootstrap(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"csrfToken":    a.csrfToken,
		"config":       a.publicConfig(),
		"status":       a.status(),
		"codexSnippet": a.codexSnippet(),
	})
}

func (a *application) publicConfig() publicConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	accounts := make([]publicAccount, 0, len(a.cfg.Accounts))
	for _, account := range a.cfg.Accounts {
		accounts = append(accounts, publicAccount{
			ID: account.ID, Name: account.Name, BaseURL: account.BaseURL, Enabled: account.Enabled,
			Revision: account.Revision, Verified: account.Verified, BlockedReason: account.BlockedReason,
			KeyConfigured: account.APIKey != "",
		})
	}
	return publicConfig{
		Version: configVersion, Accounts: accounts, Strategy: a.cfg.Strategy, TestModel: a.cfg.TestModel,
		AllowInsecureHTTP: a.cfg.AllowInsecureHTTP, GatewayTokenConfigured: a.cfg.GatewayToken != "",
		LastSwitchReason: a.cfg.LastSwitchReason, LastSwitchAt: a.cfg.LastSwitchAt,
	}
}

func (a *application) status() routerStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	available := 0
	accounts := make([]accountStatus, 0, len(a.cfg.Accounts))
	for _, account := range a.cfg.Accounts {
		runtime := a.runtimeForLocked(account)
		state := accountState(account, runtime, now)
		if state == "available" {
			available++
		}
		cooldownUntil := ""
		if now.Before(runtime.CooldownUntil) {
			cooldownUntil = runtime.CooldownUntil.UTC().Format(time.RFC3339)
		}
		lastUsedAt := ""
		if !runtime.LastUsedAt.IsZero() {
			lastUsedAt = runtime.LastUsedAt.UTC().Format(time.RFC3339)
		}
		accounts = append(accounts, accountStatus{
			ID: account.ID, Name: account.Name, State: state, Verified: account.Verified,
			BlockedReason: account.BlockedReason, CoolingDown: cooldownUntil != "",
			CooldownUntil: cooldownUntil, AssignedRequests: runtime.AssignedRequests,
			LastUsedAt: lastUsedAt, Balance: publicBalanceAt(runtime.Balance, now),
		})
	}
	effective, fallback := a.effectiveStrategyLocked(now)
	return routerStatus{
		Strategy: a.cfg.Strategy, EffectiveStrategy: effective, FallbackReason: fallback,
		LastRoutedAccountID: a.lastRoutedAccountID, LastRoutedAccountName: a.lastRoutedAccountName,
		AvailableAccounts: available, TotalAccounts: len(a.cfg.Accounts), Accounts: accounts,
		LastSwitchReason: a.cfg.LastSwitchReason, LastSwitchAt: a.cfg.LastSwitchAt,
	}
}

func accountState(account accountConfig, runtime *accountRuntime, now time.Time) string {
	switch {
	case !account.Enabled:
		return "disabled"
	case !accountConfigured(account):
		return "incomplete"
	case account.BlockedReason != "":
		return "blocked"
	case now.Before(runtime.CooldownUntil):
		return "cooldown"
	default:
		return "available"
	}
}

func publicBalanceAt(balance balanceSnapshot, now time.Time) publicBalance {
	updatedAt := ""
	if !balance.UpdatedAt.IsZero() {
		updatedAt = balance.UpdatedAt.UTC().Format(time.RFC3339)
	}
	status := balance.Status
	if status == "" {
		status = "unknown"
	}
	return publicBalance{
		Status: status, Amount: balance.Amount, Unit: balance.Unit, DisplayLabel: balance.DisplayLabel,
		Unlimited: balance.Unlimited, UpdatedAt: updatedAt, Fresh: balanceFresh(balance, now),
	}
}

func balanceFresh(balance balanceSnapshot, now time.Time) bool {
	return balanceFreshFor(balance, now, balanceTTL)
}

func balanceFreshFor(balance balanceSnapshot, now time.Time, maxAge time.Duration) bool {
	return !balance.UpdatedAt.IsZero() && !now.After(balance.UpdatedAt.Add(maxAge))
}

func (a *application) effectiveStrategyLocked(now time.Time) (string, string) {
	if a.cfg.Strategy != strategyHighestBalance {
		return a.cfg.Strategy, ""
	}
	unit := ""
	found := false
	for _, account := range a.cfg.Accounts {
		runtime := a.runtimeForLocked(account)
		if accountState(account, runtime, now) != "available" {
			continue
		}
		found = true
		if !balanceFresh(runtime.Balance, now) {
			return strategyLeastUsed, "balance_stale"
		}
		if runtime.Balance.Status != "ok" {
			return strategyLeastUsed, "balance_unavailable"
		}
		if runtime.Balance.Unlimited {
			continue
		}
		if runtime.Balance.Unit == "" {
			return strategyLeastUsed, "balance_unit_unknown"
		}
		if unit == "" {
			unit = runtime.Balance.Unit
		} else if unit != runtime.Balance.Unit {
			return strategyLeastUsed, "balance_unit_mismatch"
		}
	}
	if !found {
		return strategyLeastUsed, "balance_unavailable"
	}
	return strategyHighestBalance, ""
}

func (a *application) codexSnippet() string {
	a.mu.Lock()
	token := a.cfg.GatewayToken
	a.mu.Unlock()
	return "model_provider = \"quota_router\"\n\n" +
		"[model_providers.quota_router]\n" +
		"name = \"quota-router\"\n" +
		"base_url = \"http://127.0.0.1:4000/v1\"\n" +
		"wire_api = \"responses\"\n" +
		"experimental_bearer_token = " + strconv.Quote(token) + "\n" +
		"requires_openai_auth = true\n"
}

func (a *application) handleSave(w http.ResponseWriter, r *http.Request) {
	var request saveRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.mu.Lock()
	candidate := cloneConfig(a.cfg)
	if request.Strategy != nil {
		candidate.Strategy = *request.Strategy
	}
	if request.TestModel != nil {
		candidate.TestModel = *request.TestModel
	}
	if request.AllowInsecureHTTP != nil {
		candidate.AllowInsecureHTTP = *request.AllowInsecureHTTP
	}
	if request.RotateGatewayToken {
		token, err := randomToken()
		if err != nil {
			a.mu.Unlock()
			writeAdminError(w, http.StatusInternalServerError, "无法生成新的网关 Token")
			return
		}
		candidate.GatewayToken = token
	}
	changedAccounts := make(map[string]bool)
	if request.Accounts != nil {
		existing := make(map[string]accountConfig, len(a.cfg.Accounts))
		for _, account := range a.cfg.Accounts {
			existing[account.ID] = account
		}
		seen := make(map[string]bool, len(*request.Accounts))
		next := make([]accountConfig, 0, len(*request.Accounts))
		for index, input := range *request.Accounts {
			input.ID = strings.TrimSpace(input.ID)
			input.Name = strings.TrimSpace(input.Name)
			input.BaseURL = strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
			input.APIKey = strings.TrimSpace(input.APIKey)
			if input.ID == "" {
				if input.ClearAPIKey || input.APIKey == "" {
					a.mu.Unlock()
					writeAdminError(w, http.StatusBadRequest, "新账号必须填写 API Key")
					return
				}
				id, err := randomAccountID()
				if err != nil {
					a.mu.Unlock()
					writeAdminError(w, http.StatusInternalServerError, "无法生成账号 ID")
					return
				}
				for seen[id] || existing[id].ID != "" {
					id, err = randomAccountID()
					if err != nil {
						a.mu.Unlock()
						writeAdminError(w, http.StatusInternalServerError, "无法生成账号 ID")
						return
					}
				}
				enabled := true
				if input.Enabled != nil {
					enabled = *input.Enabled
				}
				account := accountConfig{
					ID: id, Name: input.Name, BaseURL: input.BaseURL, APIKey: input.APIKey,
					Enabled: enabled, Revision: 1,
				}
				if account.Name == "" {
					account.Name = fmt.Sprintf("账号 %d", index+1)
				}
				seen[id] = true
				changedAccounts[id] = true
				next = append(next, account)
				continue
			}
			if seen[input.ID] {
				a.mu.Unlock()
				writeAdminError(w, http.StatusBadRequest, "账号 ID 重复")
				return
			}
			old, ok := existing[input.ID]
			if !ok {
				a.mu.Unlock()
				writeAdminError(w, http.StatusBadRequest, "不能使用未知的账号 ID")
				return
			}
			seen[input.ID] = true
			if old.APIKey != "" && input.APIKey == "" && !input.ClearAPIKey &&
				baseURLMovesToDifferentOrigin(old.BaseURL, input.BaseURL) {
				a.mu.Unlock()
				writeAdminError(w, http.StatusBadRequest, "Base URL 更换到不同来源时，必须重新填写 API Key 或明确清除旧 Key")
				return
			}
			account := old
			account.Name = input.Name
			account.BaseURL = input.BaseURL
			if input.Enabled != nil {
				account.Enabled = *input.Enabled
			}
			if input.ClearAPIKey {
				account.APIKey = ""
			} else if input.APIKey != "" {
				account.APIKey = input.APIKey
			}
			if account.Name == "" {
				account.Name = old.Name
			}
			if account.BaseURL != old.BaseURL || account.APIKey != old.APIKey {
				account.Revision = old.Revision + 1
				account.Verified = false
				account.BlockedReason = ""
				changedAccounts[account.ID] = true
			}
			next = append(next, account)
		}
		candidate.Accounts = next
	}
	if err := normalizeAndValidateConfig(&candidate); err != nil {
		a.mu.Unlock()
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := saveConfig(a.configPath, candidate); err != nil {
		a.mu.Unlock()
		writeAdminError(w, http.StatusInternalServerError, "配置保存失败")
		return
	}
	a.cfg = candidate
	nextRuntime := make(map[string]*accountRuntime, len(candidate.Accounts))
	for _, account := range candidate.Accounts {
		if runtime := a.runtime[account.ID]; runtime != nil && !changedAccounts[account.ID] && runtime.Revision == account.Revision {
			nextRuntime[account.ID] = runtime
		} else {
			nextRuntime[account.ID] = &accountRuntime{Revision: account.Revision}
		}
	}
	a.runtime = nextRuntime
	if len(candidate.Accounts) == 0 {
		a.roundRobinCursor = 0
	} else {
		a.roundRobinCursor %= len(candidate.Accounts)
	}
	if a.lastRoutedAccountID != "" {
		lastRoutedFound := false
		for _, account := range candidate.Accounts {
			if account.ID == a.lastRoutedAccountID {
				a.lastRoutedAccountName = account.Name
				lastRoutedFound = true
				break
			}
		}
		if !lastRoutedFound {
			a.lastRoutedAccountID = ""
			a.lastRoutedAccountName = ""
		}
	}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "config": a.publicConfig(), "status": a.status(), "codexSnippet": a.codexSnippet(),
	})
}

func (a *application) handleTest(w http.ResponseWriter, r *http.Request) {
	var request testRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.AccountID = strings.TrimSpace(request.AccountID)
	if request.AccountID == "" && request.Candidate != nil {
		request.AccountID = strings.TrimSpace(request.Candidate.ID)
	}
	a.mu.Lock()
	model := a.cfg.TestModel
	allowInsecureHTTP := a.cfg.AllowInsecureHTTP
	var saved accountConfig
	savedFound := false
	for _, account := range a.cfg.Accounts {
		if account.ID == request.AccountID {
			saved = account
			savedFound = true
			break
		}
	}
	a.mu.Unlock()
	if request.TestModel != "" {
		model = strings.TrimSpace(request.TestModel)
	}
	if request.AllowInsecureHTTP != nil {
		allowInsecureHTTP = *request.AllowInsecureHTTP
	}
	candidate := saved
	if request.Candidate != nil {
		if savedFound && saved.APIKey != "" && strings.TrimSpace(request.Candidate.APIKey) == "" &&
			!request.Candidate.ClearAPIKey &&
			baseURLMovesToDifferentOrigin(saved.BaseURL, request.Candidate.BaseURL) {
			writeAdminError(w, http.StatusBadRequest, "Base URL 更换到不同来源时，必须重新填写 API Key 或明确清除旧 Key")
			return
		}
		candidate.Name = strings.TrimSpace(request.Candidate.Name)
		candidate.BaseURL = strings.TrimRight(strings.TrimSpace(request.Candidate.BaseURL), "/")
		if request.Candidate.ClearAPIKey {
			candidate.APIKey = ""
		} else if strings.TrimSpace(request.Candidate.APIKey) != "" {
			candidate.APIKey = strings.TrimSpace(request.Candidate.APIKey)
		}
	}
	if candidate.Name == "" {
		candidate.Name = "待测试账号"
	}
	validation := storedConfig{
		Version: configVersion, Accounts: []accountConfig{candidate}, Strategy: strategyPriority,
		AllowInsecureHTTP: allowInsecureHTTP,
	}
	if candidate.ID == "" {
		candidate.ID = "candidate"
		validation.Accounts[0].ID = candidate.ID
	}
	if validation.Accounts[0].Revision < 1 {
		validation.Accounts[0].Revision = 1
	}
	if err := normalizeAndValidateConfig(&validation); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	candidate = validation.Accounts[0]
	if !accountConfigured(candidate) {
		writeAdminError(w, http.StatusBadRequest, "该账号尚未完整配置")
		return
	}
	if model == "" {
		writeAdminError(w, http.StatusBadRequest, "请先填写测试模型")
		return
	}
	matchesSaved := savedFound && candidate.BaseURL == saved.BaseURL && candidate.APIKey == saved.APIKey
	ctx, cancel := context.WithTimeout(r.Context(), upstreamTestTime)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{
		"model": model, "input": "Reply with OK only.", "max_output_tokens": 8, "stream": false,
	})
	target, err := joinUpstreamURL(candidate.BaseURL, "/v1/responses", "")
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "账号 URL 无效")
		return
	}
	started := a.now()
	upstreamRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		writeAdminError(w, http.StatusInternalServerError, "无法创建测试请求")
		return
	}
	upstreamRequest.Header.Set("Authorization", "Bearer "+candidate.APIKey)
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Set("Accept", "application/json")
	response, err := a.client.Do(upstreamRequest)
	latency := a.now().Sub(started).Milliseconds()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "statusCode": 0, "latencyMs": latency, "message": "无法连接到上游",
		})
		return
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	ok := response.StatusCode >= 200 && response.StatusCode < 300
	message := "连接成功"
	balance := balanceSnapshot{Status: "unknown"}
	if !ok {
		message = redactSecret(upstreamErrorMessage(response.StatusCode, body), candidate.APIKey)
	} else {
		if matchesSaved {
			a.markAccountTestSucceeded(saved)
		}
		balanceContext, balanceCancel := newBalanceRefreshContext(ctx)
		balance = a.probeBalance(balanceContext, candidate)
		balanceCancel()
		if matchesSaved {
			a.applyBalance(saved, balance)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": ok, "statusCode": response.StatusCode, "latencyMs": latency, "message": message,
		"balance": publicBalanceAt(balance, a.now()),
	})
}

func (a *application) handleBalancesRefresh(w http.ResponseWriter, r *http.Request) {
	maxAge := time.Duration(0)
	if r.URL.Query().Get("automatic") == "1" {
		maxAge = balanceAutoTTL
	}
	a.refreshBalances(r.Context(), nil, maxAge)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": a.status()})
}

func (a *application) handleAccountReset(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.ID = strings.TrimSpace(request.ID)
	a.mu.Lock()
	index := a.accountIndexLocked(request.ID)
	if index < 0 {
		a.mu.Unlock()
		writeAdminError(w, http.StatusNotFound, "账号不存在")
		return
	}
	candidate := cloneConfig(a.cfg)
	candidate.Accounts[index].BlockedReason = ""
	candidate.Accounts[index].Revision++
	candidate.LastSwitchReason = "account_reset"
	candidate.LastSwitchAt = a.now().UTC().Format(time.RFC3339)
	if err := saveConfig(a.configPath, candidate); err != nil {
		a.mu.Unlock()
		writeAdminError(w, http.StatusInternalServerError, "账号状态保存失败")
		return
	}
	a.cfg = candidate
	runtime := a.runtimeForLocked(candidate.Accounts[index])
	runtime.CooldownUntil = time.Time{}
	a.mu.Unlock()
	a.refreshBalances(r.Context(), map[string]bool{request.ID: true}, 0)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": a.publicConfig(), "status": a.status()})
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, destination any) error {
	if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		return fmt.Errorf("Content-Type 必须是 application/json")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("JSON 请求无效")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("JSON 请求只能包含一个对象")
	}
	return nil
}

func (a *application) handleProxy(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	gatewayToken := a.cfg.GatewayToken
	a.mu.Unlock()
	if !requestHasGatewayToken(r, gatewayToken) {
		writeProxyError(w, http.StatusUnauthorized, "invalid_gateway_token", "本地网关 Token 无效")
		return
	}
	if r.ContentLength > maxProxyBody {
		writeProxyError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "请求体超过 128 MiB 限制")
		return
	}
	// ponytail: request bodies stay in memory up to 128 MiB; use a 0600 temp file if larger inputs are ever required.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProxyBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeProxyError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "请求体超过 128 MiB 限制")
			return
		}
		writeProxyError(w, http.StatusBadRequest, "invalid_request_body", "无法读取请求体")
		return
	}
	attempted := make(map[string]bool)
	var previous *http.Response
	for {
		account, ok := a.selectAccount(r.Context(), attempted)
		if !ok {
			if previous != nil {
				defer previous.Body.Close()
				forwardResponse(w, previous)
				return
			}
			writeProxyError(w, http.StatusServiceUnavailable, "upstream_not_available", "没有可用账号")
			return
		}
		attempted[account.ID] = true
		if previous != nil {
			previous.Body.Close()
			previous = nil
		}
		response, sendErr := a.sendUpstream(r.Context(), r, body, account)
		if sendErr != nil {
			writeProxyError(w, http.StatusBadGateway, "upstream_unavailable", "无法连接到上游账号")
			return
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			a.markAccountVerified(account)
			defer response.Body.Close()
			forwardResponse(w, response)
			return
		}
		if response.StatusCode >= 500 {
			defer response.Body.Close()
			forwardResponse(w, response)
			return
		}
		retry := false
		if response.StatusCode == http.StatusUnauthorized {
			a.handleAccountUnauthorized(account)
			retry = true
		} else {
			classification, inspectErr := inspectErrorResponse(response)
			if inspectErr == nil {
				switch classification {
				case "quota":
					a.blockAccount(account, "quota")
					retry = true
				case "rate_limit":
					a.cooldownAccount(account)
					retry = true
				}
			}
		}
		if !retry {
			defer response.Body.Close()
			forwardResponse(w, response)
			return
		}
		previous = response
	}
}

func requestHasGatewayToken(r *http.Request, expected string) bool {
	if expected == "" {
		return false
	}
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authorization) >= 7 && strings.EqualFold(authorization[:7], "Bearer ") {
		if constantEqual(strings.TrimSpace(authorization[7:]), expected) {
			return true
		}
	}
	return constantEqual(strings.TrimSpace(r.Header.Get("X-API-Key")), expected)
}

func accountConfigured(account accountConfig) bool {
	return account.BaseURL != "" && account.APIKey != ""
}

func (a *application) runtimeForLocked(account accountConfig) *accountRuntime {
	runtime := a.runtime[account.ID]
	if runtime == nil || runtime.Revision != account.Revision {
		runtime = &accountRuntime{Revision: account.Revision}
		a.runtime[account.ID] = runtime
	}
	return runtime
}

func (a *application) accountIndexLocked(id string) int {
	for index := range a.cfg.Accounts {
		if a.cfg.Accounts[index].ID == id {
			return index
		}
	}
	return -1
}

func (a *application) selectAccount(ctx context.Context, attempted map[string]bool) (accountConfig, bool) {
	a.mu.Lock()
	strategy := a.cfg.Strategy
	var stale map[string]bool
	if strategy == strategyHighestBalance {
		now := a.now()
		stale = make(map[string]bool)
		for _, account := range a.cfg.Accounts {
			runtime := a.runtimeForLocked(account)
			if accountState(account, runtime, now) == "available" && !balanceFresh(runtime.Balance, now) {
				stale[account.ID] = true
			}
		}
	}
	a.mu.Unlock()
	if len(stale) != 0 {
		a.refreshBalances(ctx, stale, balanceTTL)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	type candidateAccount struct {
		account accountConfig
		index   int
		runtime *accountRuntime
	}
	candidates := make([]candidateAccount, 0, len(a.cfg.Accounts))
	for index, account := range a.cfg.Accounts {
		runtime := a.runtimeForLocked(account)
		if !attempted[account.ID] && accountState(account, runtime, now) == "available" {
			candidates = append(candidates, candidateAccount{account: account, index: index, runtime: runtime})
		}
	}
	if len(candidates) == 0 {
		return accountConfig{}, false
	}
	effective, _ := a.effectiveStrategyLocked(now)
	selected := 0
	switch effective {
	case strategyRoundRobin:
		start := 0
		if len(a.cfg.Accounts) != 0 {
			start = a.roundRobinCursor % len(a.cfg.Accounts)
		}
		for offset := 0; offset < len(a.cfg.Accounts); offset++ {
			index := (start + offset) % len(a.cfg.Accounts)
			for candidateIndex := range candidates {
				if candidates[candidateIndex].index == index {
					selected = candidateIndex
					offset = len(a.cfg.Accounts)
					break
				}
			}
		}
		a.roundRobinCursor = (candidates[selected].index + 1) % len(a.cfg.Accounts)
	case strategyLeastUsed:
		for index := 1; index < len(candidates); index++ {
			if candidates[index].runtime.AssignedRequests < candidates[selected].runtime.AssignedRequests {
				selected = index
			}
		}
	case strategyHighestBalance:
		for index := 1; index < len(candidates); index++ {
			left := candidates[index]
			right := candidates[selected]
			switch {
			case left.runtime.Balance.Unlimited && !right.runtime.Balance.Unlimited:
				selected = index
			case left.runtime.Balance.Unlimited == right.runtime.Balance.Unlimited &&
				left.runtime.Balance.Amount > right.runtime.Balance.Amount:
				selected = index
			case left.runtime.Balance.Unlimited == right.runtime.Balance.Unlimited &&
				left.runtime.Balance.Amount == right.runtime.Balance.Amount &&
				left.runtime.AssignedRequests < right.runtime.AssignedRequests:
				selected = index
			}
		}
	}
	chosen := candidates[selected]
	chosen.runtime.AssignedRequests++
	chosen.runtime.LastUsedAt = now
	a.lastRoutedAccountID = chosen.account.ID
	a.lastRoutedAccountName = chosen.account.Name
	return chosen.account, true
}

func (a *application) markAccountVerified(expected accountConfig) {
	a.setAccountVerified(expected, false)
}

func (a *application) markAccountTestSucceeded(expected accountConfig) {
	a.setAccountVerified(expected, true)
}

func (a *application) setAccountVerified(expected accountConfig, clearBlocked bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return
	}
	current := a.cfg.Accounts[index]
	if current.Verified && (!clearBlocked || current.BlockedReason == "") {
		return
	}
	candidate := cloneConfig(a.cfg)
	candidate.Accounts[index].Verified = true
	if clearBlocked {
		candidate.Accounts[index].BlockedReason = ""
	}
	if saveConfig(a.configPath, candidate) == nil {
		a.cfg = candidate
	}
}

func (a *application) handleAccountUnauthorized(expected accountConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return
	}
	if !a.cfg.Accounts[index].Verified {
		a.runtimeForLocked(a.cfg.Accounts[index]).CooldownUntil = a.now().Add(accountCooldown)
		return
	}
	a.blockAccountLocked(index, "unauthorized")
}

func (a *application) blockAccount(expected accountConfig, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return
	}
	a.blockAccountLocked(index, reason)
}

func (a *application) blockAccountLocked(index int, reason string) {
	now := a.now()
	candidate := cloneConfig(a.cfg)
	candidate.Accounts[index].BlockedReason = reason
	candidate.LastSwitchReason = "account_" + reason
	candidate.LastSwitchAt = now.UTC().Format(time.RFC3339)
	if saveConfig(a.configPath, candidate) == nil {
		a.cfg = candidate
	} else {
		a.cfg.Accounts[index].BlockedReason = reason
		a.cfg.LastSwitchReason = candidate.LastSwitchReason
		a.cfg.LastSwitchAt = candidate.LastSwitchAt
	}
	a.runtimeForLocked(a.cfg.Accounts[index]).CooldownUntil = time.Time{}
}

func (a *application) cooldownAccount(expected accountConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return
	}
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	runtime.CooldownUntil = a.now().Add(accountCooldown)
}

func sameAccountRevision(current, expected accountConfig) bool {
	return current.ID == expected.ID && current.Revision == expected.Revision &&
		current.BaseURL == expected.BaseURL && current.APIKey == expected.APIKey
}

func (a *application) applyBalance(expected accountConfig, balance balanceSnapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return
	}
	a.runtimeForLocked(a.cfg.Accounts[index]).Balance = balance
}

func (a *application) sendUpstream(ctx context.Context, original *http.Request, body []byte, account accountConfig) (*http.Response, error) {
	target, err := joinUpstreamURL(account.BaseURL, original.URL.Path, original.URL.RawQuery)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, original.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(request.Header, original.Header)
	removeHopHeaders(request.Header)
	request.Header.Del("Authorization")
	request.Header.Del("X-API-Key")
	request.Header.Del("Cookie")
	request.Header.Set("Authorization", "Bearer "+account.APIKey)
	return a.client.Do(request)
}

func joinUpstreamURL(baseURL, requestPath, rawQuery string) (string, error) {
	target, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimRight(target.Path, "/")
	suffix := requestPath
	if strings.HasSuffix(basePath, "/v1") && (suffix == "/v1" || strings.HasPrefix(suffix, "/v1/")) {
		suffix = strings.TrimPrefix(suffix, "/v1")
	}
	if suffix != "" && suffix != "/" {
		target.Path = basePath + "/" + strings.TrimLeft(suffix, "/")
	} else if basePath == "" {
		target.Path = "/"
	} else {
		target.Path = basePath
	}
	target.RawPath = ""
	target.RawQuery = rawQuery
	target.Fragment = ""
	return target.String(), nil
}

func balanceAPIURL(baseURL, endpoint string) (string, error) {
	target, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	path := strings.TrimRight(target.Path, "/")
	if strings.HasSuffix(path, "/v1") {
		path = strings.TrimSuffix(path, "/v1")
	}
	target.Path = strings.TrimRight(path, "/") + endpoint
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	return target.String(), nil
}

func newBalanceRefreshContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, balanceRefreshTime)
}

func (a *application) refreshBalances(ctx context.Context, ids map[string]bool, maxAge time.Duration) {
	a.balanceRefreshMu.Lock()
	defer a.balanceRefreshMu.Unlock()
	ctx, cancel := newBalanceRefreshContext(ctx)
	defer cancel()
	a.mu.Lock()
	now := a.now()
	accounts := make([]accountConfig, 0, len(a.cfg.Accounts))
	updatedAt := make(map[string]time.Time, len(a.cfg.Accounts))
	for _, account := range a.cfg.Accounts {
		if ids != nil && !ids[account.ID] {
			continue
		}
		if maxAge > 0 && !account.Enabled {
			continue
		}
		runtime := a.runtimeForLocked(account)
		if accountConfigured(account) && (maxAge <= 0 || !balanceFreshFor(runtime.Balance, now, maxAge)) {
			accounts = append(accounts, account)
			updatedAt[account.ID] = runtime.Balance.UpdatedAt
		}
	}
	a.mu.Unlock()
	if maxAge > 0 {
		sortAccountsByBalanceAge(accounts, updatedAt)
	}
	workerCount := balanceWorkers
	if len(accounts) < workerCount {
		workerCount = len(accounts)
	}
	jobs := make(chan accountConfig)
	var wait sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for account := range jobs {
				balance := a.probeBalance(ctx, account)
				if ctx.Err() == nil || balance.Status != "error" {
					a.applyBalance(account, balance)
				}
			}
		}()
	}
	for _, account := range accounts {
		select {
		case jobs <- account:
		case <-ctx.Done():
			close(jobs)
			wait.Wait()
			return
		}
	}
	close(jobs)
	wait.Wait()
}

func sortAccountsByBalanceAge(accounts []accountConfig, updatedAt map[string]time.Time) {
	sort.SliceStable(accounts, func(left, right int) bool {
		leftTime := updatedAt[accounts[left].ID]
		rightTime := updatedAt[accounts[right].ID]
		if leftTime.IsZero() != rightTime.IsZero() {
			return leftTime.IsZero()
		}
		return leftTime.Before(rightTime)
	})
}

func (a *application) probeBalance(ctx context.Context, account accountConfig) balanceSnapshot {
	checkedAt := a.now()
	result := balanceSnapshot{Status: "error", UpdatedAt: checkedAt}
	usageURL, err := balanceAPIURL(account.BaseURL, "/api/usage/token/")
	if err != nil {
		return result
	}
	status, body, err := a.getUpstreamJSON(ctx, usageURL, account.APIKey)
	if err != nil {
		return result
	}
	if status == http.StatusNotFound {
		return a.probeDashboardBalance(ctx, account, checkedAt)
	}
	if status < 200 || status >= 300 {
		return result
	}
	payload, ok := decodeJSONObject(body)
	if !ok {
		return a.probeDashboardBalance(ctx, account, checkedAt)
	}
	data := nestedObject(payload, "data")
	unlimited, _ := boolValue(data["unlimited_quota"])
	total, hasTotal := numberValue(data["total_available"])
	if !unlimited && !hasTotal {
		return a.probeDashboardBalance(ctx, account, checkedAt)
	}
	result.Status = "ok"
	result.Amount = total
	result.Unlimited = unlimited
	result.DisplayLabel = "站点额度"
	statusURL, statusErr := balanceAPIURL(account.BaseURL, "/api/status")
	if statusErr != nil {
		return result
	}
	statusCode, statusBody, statusErr := a.getUpstreamJSON(ctx, statusURL, account.APIKey)
	if statusErr != nil || statusCode < 200 || statusCode >= 300 {
		return result
	}
	statusPayload, ok := decodeJSONObject(statusBody)
	if !ok {
		return result
	}
	statusData := nestedObject(statusPayload, "data")
	perUnit, hasPerUnit := numberValue(statusData["quota_per_unit"])
	unit := displayUnit(statusData["quota_display_type"])
	if hasPerUnit && perUnit > 0 && unit != "" {
		result.Amount /= perUnit
		result.Unit = unit
		result.DisplayLabel = unit
	}
	return result
}

func (a *application) probeDashboardBalance(ctx context.Context, account accountConfig, checkedAt time.Time) balanceSnapshot {
	result := balanceSnapshot{Status: "error", UpdatedAt: checkedAt}
	subscriptionURL, err := joinUpstreamURL(account.BaseURL, "/dashboard/billing/subscription", "")
	if err != nil {
		return result
	}
	status, body, err := a.getUpstreamJSON(ctx, subscriptionURL, account.APIKey)
	if err != nil {
		return result
	}
	if status == http.StatusNotFound {
		result.Status = "unsupported"
		return result
	}
	if status < 200 || status >= 300 {
		return result
	}
	subscription, ok := decodeJSONObject(body)
	if !ok {
		return result
	}
	subscriptionData := nestedObject(subscription, "data")
	unlimited, _ := boolValue(subscriptionData["unlimited_quota"])
	hardLimit, hasHardLimit := firstNumber(subscriptionData, "hard_limit_usd", "system_hard_limit_usd")
	unit := ""
	if hasHardLimit {
		unit = "USD"
	} else {
		hardLimit, hasHardLimit = numberValue(subscriptionData["total_available"])
	}
	if !unlimited && !hasHardLimit {
		return result
	}
	now := a.now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	end := now.AddDate(0, 0, 1).Format("2006-01-02")
	query := url.Values{"start_date": []string{start}, "end_date": []string{end}}.Encode()
	usageURL, err := joinUpstreamURL(account.BaseURL, "/dashboard/billing/usage", query)
	if err != nil {
		return result
	}
	status, body, err = a.getUpstreamJSON(ctx, usageURL, account.APIKey)
	if err != nil {
		return result
	}
	if status == http.StatusNotFound {
		result.Status = "unsupported"
		return result
	}
	if status < 200 || status >= 300 {
		return result
	}
	usage, ok := decodeJSONObject(body)
	if !ok {
		return result
	}
	usageData := nestedObject(usage, "data")
	totalUsage, hasUsage := numberValue(usageData["total_usage"])
	if !hasUsage {
		totalUsage, hasUsage = firstNumber(usageData, "total_used", "used")
	} else {
		totalUsage /= 100
	}
	if !hasUsage {
		return result
	}
	result.Status = "ok"
	result.Unlimited = unlimited
	result.Unit = unit
	result.DisplayLabel = unit
	if result.DisplayLabel == "" {
		result.DisplayLabel = "站点额度"
	}
	result.Amount = hardLimit - totalUsage
	if result.Amount < 0 {
		result.Amount = 0
	}
	return result
}

func (a *application) getUpstreamJSON(ctx context.Context, target, apiKey string) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBody+1))
	if err != nil || len(body) > maxErrorBody {
		return response.StatusCode, nil, errors.New("invalid balance response")
	}
	return response.StatusCode, body, nil
}

func decodeJSONObject(data []byte) (map[string]any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil {
		return nil, false
	}
	return payload, true
}

func nestedObject(payload map[string]any, key string) map[string]any {
	if nested, ok := payload[key].(map[string]any); ok {
		return nested
	}
	return payload
}

func numberValue(value any) (float64, bool) {
	finite := func(number float64, err error) (float64, bool) {
		return number, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
	}
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		return finite(number, err)
	case float64:
		return finite(typed, nil)
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return finite(number, err)
	default:
		return 0, false
	}
}

func firstNumber(payload map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if value, ok := numberValue(payload[key]); ok {
			return value, true
		}
	}
	return 0, false
}

func boolValue(value any) (bool, bool) {
	switch typed := value.(type) {
	case bool:
		return typed, true
	case json.Number:
		number, err := typed.Int64()
		return number != 0, err == nil
	case float64:
		return typed != 0, true
	case string:
		boolean, err := strconv.ParseBool(strings.TrimSpace(typed))
		return boolean, err == nil
	default:
		return false, false
	}
}

func displayUnit(value any) string {
	typed, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(typed))
}

type readCloser struct {
	io.Reader
	io.Closer
}

func inspectErrorResponse(response *http.Response) (string, error) {
	originalBody := response.Body
	prefix, err := io.ReadAll(io.LimitReader(originalBody, maxErrorBody+1))
	response.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(prefix), originalBody), Closer: originalBody}
	if err != nil {
		return "", err
	}
	if response.StatusCode == http.StatusPaymentRequired || structuredQuotaError(prefix) {
		return "quota", nil
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return "rate_limit", nil
	}
	return "", nil
}

func structuredQuotaError(body []byte) bool {
	if len(body) > maxErrorBody {
		return false
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	identifiers := make([]string, 0, 4)
	messages := make([]string, 0, 2)
	collectErrorFields(payload, &identifiers, &messages)
	quotaCodes := map[string]bool{
		"insufficient_quota": true, "insufficient_user_quota": true,
		"insufficient_system_quota": true, "insufficient_channel_quota": true,
		"quota_exceeded": true, "quota_not_enough": true, "user_quota_not_enough": true,
		"billing_hard_limit_reached": true, "billing_not_active": true,
		"insufficient_balance": true, "insufficient_credit": true,
		"balance_exhausted": true, "credit_balance_exhausted": true, "credits_exhausted": true,
	}
	for _, identifier := range identifiers {
		normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(identifier)))
		if quotaCodes[normalized] ||
			(strings.Contains(normalized, "quota") && (strings.Contains(normalized, "insufficient") ||
				strings.Contains(normalized, "exhausted") || strings.Contains(normalized, "exceeded") ||
				strings.Contains(normalized, "not_enough"))) ||
			(strings.Contains(normalized, "balance") && (strings.Contains(normalized, "insufficient") ||
				strings.Contains(normalized, "exhausted") || strings.Contains(normalized, "not_enough"))) {
			return true
		}
	}
	quotaPhrases := []string{
		"insufficient quota", "quota exceeded", "exceeded your current quota", "quota has been exhausted",
		"insufficient balance", "balance is insufficient", "credit balance is too low", "billing hard limit",
		"额度不足", "额度已用尽", "额度已耗尽", "余额不足", "余额已用尽", "余额已耗尽",
	}
	for _, message := range messages {
		message = strings.ToLower(message)
		for _, phrase := range quotaPhrases {
			if strings.Contains(message, phrase) {
				return true
			}
		}
	}
	return false
}

func collectErrorFields(value any, identifiers, messages *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			lowerKey := strings.ToLower(key)
			switch lowerKey {
			case "code", "type", "reason", "error_code":
				if text, ok := item.(string); ok {
					*identifiers = append(*identifiers, text)
				}
			case "message":
				if text, ok := item.(string); ok {
					*messages = append(*messages, text)
				}
			}
			if lowerKey == "error" || lowerKey == "details" || lowerKey == "detail" {
				collectErrorFields(item, identifiers, messages)
			}
		}
	case []any:
		for _, item := range typed {
			collectErrorFields(item, identifiers, messages)
		}
	}
}

func upstreamErrorMessage(status int, body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &payload) == nil {
		message := payload.Error.Message
		if message == "" {
			message = payload.Message
		}
		message = strings.TrimSpace(message)
		if message != "" {
			if len(message) > 300 {
				message = message[:300] + "…"
			}
			return message
		}
	}
	return fmt.Sprintf("上游返回 HTTP %d", status)
}

func redactSecret(message, secret string) string {
	if secret == "" {
		return message
	}
	return strings.ReplaceAll(message, secret, "[已隐藏]")
}

func forwardResponse(w http.ResponseWriter, response *http.Response) {
	copyHeaders(w.Header(), response.Header)
	removeHopHeaders(w.Header())
	w.WriteHeader(response.StatusCode)
	buffer := make([]byte, 32<<10)
	for {
		count, readErr := response.Body.Read(buffer)
		if count > 0 {
			if _, writeErr := w.Write(buffer[:count]); writeErr != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			break
		}
	}
	for key, values := range response.Trailer {
		w.Header().Del(key)
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
}

func copyHeaders(destination, source http.Header) {
	for key, values := range source {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func removeHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		header.Del(name)
	}
}

func writeProxyError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": message, "type": "gateway_error", "code": code},
	})
}

func writeAdminError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message, "message": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
