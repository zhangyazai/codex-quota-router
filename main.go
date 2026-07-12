package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	listenAddress           = "127.0.0.1:4000"
	applicationVersion      = "0.2.1"
	configVersion           = 3
	configDirectory         = "codex-quota-router"
	configFilename          = "config.dat"
	logFilename             = "router.log"
	maxLogFileSize          = 10 << 20
	logRotationRetry        = time.Minute
	maxAdminBody            = 1 << 20
	maxErrorBody            = 1 << 20
	maxProxyBody            = 128 << 20
	accountCooldown         = time.Minute
	balanceTTL              = 5 * time.Minute
	balanceAutoTTL          = time.Minute
	balanceRefreshTime      = 20 * time.Second
	balanceRoutingTime      = 1500 * time.Millisecond
	balanceWorkers          = 8
	balanceProbeParallelism = 3
	upstreamTestTime        = 30 * time.Second
)

const (
	strategyPriority       = "priority"
	strategyRoundRobin     = "round_robin"
	strategyLeastUsed      = "least_used"
	strategyHighestBalance = "highest_balance"
)

const (
	newAPIAuthAPIKey      = "api_key"
	newAPIAuthPassword    = "password"
	newAPIAuthAccessToken = "access_token"
	newAPIUnlimitedLimit  = 100000000

	balanceScopeActual      = "actual"
	balanceScopeTokenOnly   = "token_only"
	balanceScopeAccountOnly = "account_only"

	balanceRefreshOK          = "ok"
	balanceRefreshPartial     = "partial"
	balanceRefreshError       = "error"
	balanceRefreshAuthError   = "auth_error"
	balanceRefreshUnsupported = "unsupported"
	balanceRefreshCanceled    = "canceled"

	balanceStageTokenUsage            = "token_usage"
	balanceStageAccountLogin          = "account_login"
	balanceStageAccountQuota          = "account_quota"
	balanceStageQuotaMetadata         = "quota_metadata"
	balanceStageDashboardSubscription = "dashboard_subscription"
	balanceStageDashboardUsage        = "dashboard_usage"
	balanceStageAccount               = "account"

	balanceErrorTimeout         = "timeout"
	balanceErrorNetwork         = "network_error"
	balanceErrorAPIKeyAuth      = "api_key_unauthorized"
	balanceErrorAccountAuth     = "account_unauthorized"
	balanceErrorAccessTokenAuth = "access_token_unauthorized"
	balanceErrorUserIDRequired  = "user_id_required"
	balanceErrorUserIDMismatch  = "user_id_mismatch"
	balanceErrorTwoFactor       = "two_factor_required"
	balanceErrorRateLimited     = "rate_limited"
	balanceErrorUpstream        = "upstream_unavailable"
	balanceErrorRejected        = "upstream_rejected"
	balanceErrorInvalidResponse = "invalid_response"
	balanceErrorMissingQuota    = "missing_quota"
	balanceErrorUnsupported     = "unsupported"
	balanceErrorCanceled        = "canceled"
)

var (
	errNewAPIAuthentication = errors.New("New API authentication failed")
	errUnsafeLogPath        = errors.New("unsafe log path")
)

//go:embed web/index.html
var indexHTML []byte

type accountConfig struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	BaseURL        string `json:"baseUrl"`
	APIKey         string `json:"apiKey"`
	NewAPIAuthMode string `json:"newApiAuthMode,omitempty"`
	NewAPIUsername string `json:"newApiUsername,omitempty"`
	NewAPIUserID   int    `json:"newApiUserId,omitempty"`
	NewAPISecret   string `json:"newApiSecret,omitempty"`
	Enabled        bool   `json:"enabled"`
	Revision       int    `json:"revision"`
	Verified       bool   `json:"verified"`
	BlockedReason  string `json:"blockedReason,omitempty"`
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
	ID                string `json:"id"`
	Name              string `json:"name"`
	BaseURL           string `json:"baseUrl"`
	APIKey            string `json:"apiKey"`
	NewAPIAuthMode    string `json:"newApiAuthMode"`
	NewAPIUsername    string `json:"newApiUsername"`
	NewAPIUserID      int    `json:"newApiUserId"`
	NewAPISecret      string `json:"newApiSecret"`
	Enabled           *bool  `json:"enabled"`
	ClearAPIKey       bool   `json:"clearApiKey"`
	ClearNewAPISecret bool   `json:"clearNewApiSecret"`
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

type balanceRefreshRequest struct {
	AccountIDs []string `json:"accountIds"`
}

type publicAccount struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	BaseURL                string `json:"baseUrl"`
	NewAPIAuthMode         string `json:"newApiAuthMode"`
	NewAPIUsername         string `json:"newApiUsername,omitempty"`
	NewAPIUserID           int    `json:"newApiUserId,omitempty"`
	Enabled                bool   `json:"enabled"`
	Revision               int    `json:"revision"`
	Verified               bool   `json:"verified"`
	BlockedReason          string `json:"blockedReason,omitempty"`
	KeyConfigured          bool   `json:"keyConfigured"`
	NewAPISecretConfigured bool   `json:"newApiSecretConfigured"`
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
	Status        string
	Amount        float64
	Unit          string
	DisplayLabel  string
	Unlimited     bool
	Scope         string
	LimitedBy     string
	UpdatedAt     time.Time
	RefreshStatus string
	CheckedAt     time.Time
	ErrorStage    string
	ErrorCode     string
	Retryable     bool
	Failures      int
	NextRetryAt   time.Time
	hardLimit     float64
	hasHardLimit  bool
}

type publicBalance struct {
	Status        string  `json:"status"`
	Amount        float64 `json:"amount"`
	Unit          string  `json:"unit,omitempty"`
	DisplayLabel  string  `json:"displayLabel,omitempty"`
	Unlimited     bool    `json:"unlimited"`
	Scope         string  `json:"scope,omitempty"`
	LimitedBy     string  `json:"limitedBy,omitempty"`
	UpdatedAt     string  `json:"updatedAt,omitempty"`
	RefreshStatus string  `json:"refreshStatus,omitempty"`
	CheckedAt     string  `json:"checkedAt,omitempty"`
	ErrorStage    string  `json:"errorStage,omitempty"`
	ErrorCode     string  `json:"errorCode,omitempty"`
	Retryable     bool    `json:"retryable,omitempty"`
	Fresh         bool    `json:"fresh"`
}

type balanceFailure struct {
	Status    string
	Stage     string
	Code      string
	Retryable bool
}

type balanceProbeError struct {
	failure balanceFailure
	cause   error
}

func (err *balanceProbeError) Error() string {
	return err.failure.Code
}

func (err *balanceProbeError) Unwrap() error {
	return err.cause
}

type tokenBalanceResult struct {
	balance       balanceSnapshot
	failure       *balanceFailure
	fromDashboard bool
}

type accountQuotaResult struct {
	quota   float64
	failure *balanceFailure
}

type quotaMetadataResult struct {
	data       map[string]any
	statusCode int
	failure    *balanceFailure
}

type balanceRefreshReport struct {
	AccountID string        `json:"accountId"`
	Balance   publicBalance `json:"balance"`
}

type balanceRefreshCounts struct {
	Total       int `json:"total"`
	Success     int `json:"success"`
	Partial     int `json:"partial"`
	Failed      int `json:"failed"`
	Unsupported int `json:"unsupported"`
	Canceled    int `json:"canceled"`
}

type accountRuntime struct {
	Revision         int
	CooldownUntil    time.Time
	AssignedRequests uint64
	LastUsedAt       time.Time
	Balance          balanceSnapshot
	NewAPISession    string
	NewAPIUserID     int
	NewAPIAuthHash   [sha256.Size]byte
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
	balanceRefreshGate    chan struct{}
	cfg                   storedConfig
	configPath            string
	csrfToken             string
	client                *http.Client
	now                   func() time.Time
	logger                *slog.Logger
	balanceTimeout        time.Duration
	balanceRoutingTimeout time.Duration
	runtime               map[string]*accountRuntime
	requestSequence       uint64
	roundRobinCursor      int
	lastRoutedAccountID   string
	lastRoutedAccountName string
}

func main() {
	if err := run(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	configPath, err := defaultConfigPath()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		appendStartupFailure(configPath, err)
		return err
	}
	defer listener.Close()
	logger, logWriter, err := openApplicationLogger(configPath)
	if err != nil {
		return fmt.Errorf("初始化日志失败: %w", err)
	}
	defer logWriter.Close()
	logger.Info("service_starting", "version", applicationVersion, "goos", runtime.GOOS, "listen", listenAddress)
	app, err := newApplication(configPath, nil, time.Now)
	if err != nil {
		logger.Error("service_start_failed", "error_kind", logErrorKind(err))
		return err
	}
	app.logger = logger
	server := &http.Server{
		Handler:           app.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	logger.Info("service_ready", "accounts", len(app.cfg.Accounts), "strategy", app.cfg.Strategy)
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		logger.Info("service_stopped", "reason", "server_closed")
	} else {
		logger.Error("service_stopped", "reason", "server_error", "error_kind", logErrorKind(err))
	}
	return err
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
		cfg:                   cfg,
		configPath:            path,
		csrfToken:             csrf,
		client:                client,
		now:                   now,
		balanceTimeout:        balanceRefreshTime,
		balanceRoutingTimeout: balanceRoutingTime,
		runtime:               make(map[string]*accountRuntime),
		balanceRefreshGate:    make(chan struct{}, 1),
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

func applicationLogPath(goos, temporaryRoot, runtimeRoot, configPath string) string {
	switch goos {
	case "linux":
		if filepath.IsAbs(runtimeRoot) {
			return filepath.Join(runtimeRoot, configDirectory, logFilename)
		}
		directory := configDirectory + "-" + logReference(filepath.Clean(configPath))
		return filepath.Join(temporaryRoot, directory, logFilename)
	case "darwin":
		directory := configDirectory + "-" + logReference(filepath.Clean(configPath))
		return filepath.Join(temporaryRoot, directory, logFilename)
	default:
		return filepath.Join(filepath.Dir(configPath), logFilename)
	}
}

type rotatingLogWriter struct {
	mu                  sync.Mutex
	path                string
	file                *os.File
	size                int64
	maxSize             int64
	closed              bool
	retryAt             time.Time
	suspended           bool
	reopenAfterRotation bool
}

func openApplicationLogger(configPath string) (*slog.Logger, *rotatingLogWriter, error) {
	logPath, err := resolveApplicationLogPath(configPath)
	if err != nil {
		return nil, nil, err
	}
	writer, err := openRotatingLogWriter(logPath, maxLogFileSize)
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, writer, nil
}

func resolveApplicationLogPath(configPath string) (string, error) {
	runtimeRoot := ""
	if runtime.GOOS == "linux" {
		candidate := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
		if filepath.IsAbs(candidate) {
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				runtimeRoot = candidate
			}
		}
	}
	logPath := applicationLogPath(runtime.GOOS, os.TempDir(), runtimeRoot, configPath)
	directory := filepath.Dir(logPath)
	if err := ensureLogDirectory(directory); err != nil {
		if runtime.GOOS != "linux" || runtimeRoot == "" || errors.Is(err, errUnsafeLogPath) {
			return "", err
		}
		logPath = applicationLogPath(runtime.GOOS, os.TempDir(), "", configPath)
		directory = filepath.Dir(logPath)
		if err := ensureLogDirectory(directory); err != nil {
			return "", err
		}
	}
	return logPath, nil
}

func appendStartupFailure(configPath string, startupErr error) {
	logPath, err := resolveApplicationLogPath(configPath)
	if err != nil || validateLogFile(logPath) != nil {
		return
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	if file.Chmod(0o600) != nil {
		return
	}
	info, err := file.Stat()
	if err != nil {
		return
	}
	line := fmt.Sprintf("time=%s level=ERROR msg=service_start_failed stage=listen error_kind=%s\n",
		time.Now().UTC().Format(time.RFC3339Nano), logErrorKind(startupErr))
	if info.Size()+int64(len(line)) > maxLogFileSize {
		return
	}
	_, _ = file.WriteString(line)
}

func ensureLogDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: 日志目录不是普通目录", errUnsafeLogPath)
	}
	return os.Chmod(path, 0o700)
}

func openRotatingLogWriter(path string, maxSize int64) (*rotatingLogWriter, error) {
	if err := validateLogFile(path); err != nil {
		return nil, err
	}
	writer := &rotatingLogWriter{path: path, maxSize: maxSize}
	if err := writer.openLocked(os.O_APPEND); err != nil {
		return nil, err
	}
	return writer, nil
}

func validateLogFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: 日志文件不是普通文件", errUnsafeLogPath)
	}
	return nil
}

func (w *rotatingLogWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if w.file == nil {
		if err := w.openLocked(os.O_APPEND); err != nil {
			return 0, err
		}
		if w.reopenAfterRotation {
			w.reopenAfterRotation = false
			w.retryAt = time.Time{}
			w.suspended = false
			_ = w.writeMarkerLocked("log_writes_resumed", "reopen_recovered")
		}
	}
	nextSize := w.size + int64(len(data))
	if w.maxSize > 0 && w.size > 0 && nextSize > w.maxSize {
		now := time.Now()
		if w.retryAt.IsZero() || !now.Before(w.retryAt) {
			if err := w.rotateLocked(); err != nil {
				w.retryAt = now.Add(logRotationRetry)
				if w.file == nil {
					return 0, err
				}
			} else {
				w.retryAt = time.Time{}
				w.suspended = false
			}
		}
	}
	if w.maxSize > 0 && w.size+int64(len(data)) > 2*w.maxSize {
		if !w.suspended {
			_ = w.writeMarkerLocked("log_writes_suspended", "rotation_failed")
			w.suspended = true
		}
		return len(data), nil
	}
	written, err := w.file.Write(data)
	w.size += int64(written)
	return written, err
}

func (w *rotatingLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *rotatingLogWriter) rotateLocked() error {
	file := w.file
	w.file = nil
	if err := file.Close(); err != nil {
		if recoverErr := w.recoverLocked("close_failed"); recoverErr != nil {
			return recoverErr
		}
		return err
	}
	if err := replaceFile(w.path, w.path+".1"); err != nil {
		if recoverErr := w.recoverLocked("replace_failed"); recoverErr != nil {
			return recoverErr
		}
		return err
	}
	w.reopenAfterRotation = true
	if err := w.openLocked(os.O_APPEND); err != nil {
		return err
	}
	w.reopenAfterRotation = false
	return nil
}

func (w *rotatingLogWriter) openLocked(flag int) error {
	file, err := os.OpenFile(w.path, flag|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *rotatingLogWriter) recoverLocked(reason string) error {
	if err := w.openLocked(os.O_APPEND); err != nil {
		return err
	}
	if w.suspended {
		return nil
	}
	return w.writeMarkerLocked("log_rotation_fallback", reason)
}

func (w *rotatingLogWriter) writeMarkerLocked(event, reason string) error {
	marker := fmt.Sprintf("time=%s level=WARN msg=%s reason=%s\n",
		time.Now().UTC().Format(time.RFC3339Nano), event, reason)
	written, err := w.file.WriteString(marker)
	w.size += int64(written)
	return err
}

func (a *application) logEvent(ctx context.Context, level slog.Level, event string, attributes ...any) {
	if a.logger == nil {
		return
	}
	a.logger.Log(ctx, level, event, attributes...)
}

func logReference(value string) string {
	if value == "" {
		return "none"
	}
	digest := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(digest[:6])
}

func proxyRouteKind(path string) string {
	path = strings.TrimPrefix(path, "/v1")
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "root"
	}
	segment, _, _ := strings.Cut(path, "/")
	switch segment {
	case "responses", "chat", "models":
		return segment
	default:
		return "other"
	}
}

func logErrorKind(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected_eof"
	}
	if os.IsPermission(err) {
		return "permission"
	}
	if os.IsNotExist(err) {
		return "not_found"
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return "dns"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	var operationError *net.OpError
	if errors.As(err, &operationError) {
		if operationError.Op == "listen" {
			return "listen"
		}
		if operationError.Op == "dial" {
			return "connect"
		}
		return "network"
	}
	return "other"
}

func upstreamOperationForLog(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return "invalid_url"
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case strings.HasSuffix(path, "/api/usage/token"):
		return "balance_token"
	case strings.HasSuffix(path, "/api/status"):
		return "status"
	case strings.HasSuffix(path, "/api/user/login"):
		return "account_login"
	case strings.HasSuffix(path, "/api/user/self"):
		return "account_balance"
	case strings.HasSuffix(path, "/dashboard/billing/subscription"):
		return "billing_subscription"
	case strings.HasSuffix(path, "/dashboard/billing/usage"):
		return "billing_usage"
	default:
		return "upstream_json"
	}
}

func upstreamReferenceForLog(target string) string {
	origin, ok := normalizedOrigin(target)
	if !ok {
		return "invalid"
	}
	return logReference(origin)
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
		rawAuthMode := strings.TrimSpace(strings.ToLower(account.NewAPIAuthMode))
		if !validNewAPIAuthMode(rawAuthMode) {
			return fmt.Errorf("账号 %s 的 New API 余额认证方式无效", account.Name)
		}
		account.NewAPIAuthMode = normalizeNewAPIAuthMode(rawAuthMode)
		account.NewAPIUsername = strings.TrimSpace(account.NewAPIUsername)
		if account.NewAPIAuthMode == newAPIAuthAccessToken {
			account.NewAPISecret = normalizeBearerToken(account.NewAPISecret)
		}
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
		switch account.NewAPIAuthMode {
		case newAPIAuthAPIKey:
			account.NewAPIUsername = ""
			account.NewAPIUserID = 0
			account.NewAPISecret = ""
		case newAPIAuthPassword:
			account.NewAPIUserID = 0
			if account.NewAPIUsername == "" || account.NewAPISecret == "" {
				return fmt.Errorf("账号 %s 的 New API 用户名和密码不能为空", account.Name)
			}
		case newAPIAuthAccessToken:
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

func validStrategy(strategy string) bool {
	return strategy == strategyPriority || strategy == strategyRoundRobin ||
		strategy == strategyLeastUsed || strategy == strategyHighestBalance
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
	input.BaseURL = strings.TrimRight(strings.TrimSpace(input.BaseURL), "/")
	input.APIKey = strings.TrimSpace(input.APIKey)
	input.NewAPIUsername = strings.TrimSpace(input.NewAPIUsername)
	mode := strings.TrimSpace(strings.ToLower(input.NewAPIAuthMode))
	if !validNewAPIAuthMode(mode) {
		return fmt.Errorf("New API 余额认证方式无效")
	}
	input.NewAPIAuthMode = normalizeNewAPIAuthMode(mode)
	if input.NewAPIAuthMode == newAPIAuthAccessToken {
		input.NewAPISecret = normalizeBearerToken(input.NewAPISecret)
	}
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

func newAPIAuthChanged(left, right accountConfig) bool {
	return normalizeNewAPIAuthMode(left.NewAPIAuthMode) != normalizeNewAPIAuthMode(right.NewAPIAuthMode) ||
		left.NewAPIUsername != right.NewAPIUsername || left.NewAPIUserID != right.NewAPIUserID ||
		left.NewAPISecret != right.NewAPISecret
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
	case endpoint == "/balances/test" && r.Method == http.MethodPost:
		a.handleBalanceTest(w, r)
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
			ID: account.ID, Name: account.Name, BaseURL: account.BaseURL,
			NewAPIAuthMode: normalizeNewAPIAuthMode(account.NewAPIAuthMode), NewAPIUsername: account.NewAPIUsername,
			NewAPIUserID: account.NewAPIUserID, NewAPISecretConfigured: account.NewAPISecret != "",
			Enabled: account.Enabled, Revision: account.Revision, Verified: account.Verified,
			BlockedReason: account.BlockedReason, KeyConfigured: account.APIKey != "",
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
	checkedAt := ""
	if !balance.CheckedAt.IsZero() {
		checkedAt = balance.CheckedAt.UTC().Format(time.RFC3339)
	}
	status := balance.Status
	if status == "" {
		status = "unknown"
	}
	scope := balance.Scope
	if scope == "" && status == "ok" {
		scope = balanceScopeActual
	}
	return publicBalance{
		Status: status, Amount: balance.Amount, Unit: balance.Unit, DisplayLabel: balance.DisplayLabel,
		Unlimited: balance.Unlimited, Scope: scope, LimitedBy: balance.LimitedBy,
		UpdatedAt: updatedAt, RefreshStatus: balance.RefreshStatus, CheckedAt: checkedAt,
		ErrorStage: balance.ErrorStage, ErrorCode: balance.ErrorCode, Retryable: balance.Retryable,
		Fresh: balanceFresh(balance, now),
	}
}

func balanceFresh(balance balanceSnapshot, now time.Time) bool {
	return balanceFreshFor(balance, now, balanceTTL)
}

func balanceFreshFor(balance balanceSnapshot, now time.Time, maxAge time.Duration) bool {
	return !balance.UpdatedAt.IsZero() && !now.After(balance.UpdatedAt.Add(maxAge))
}

func balanceRetryDelay(failures int, retryable bool) time.Duration {
	if !retryable {
		return 5 * time.Minute
	}
	switch {
	case failures <= 1:
		return 5 * time.Second
	case failures == 2:
		return 15 * time.Second
	case failures == 3:
		return 30 * time.Second
	default:
		return time.Minute
	}
}

func balanceRefreshDue(balance balanceSnapshot, now time.Time, maxAge time.Duration) bool {
	if maxAge <= 0 {
		return true
	}
	if !balance.NextRetryAt.IsZero() && now.Before(balance.NextRetryAt) {
		return false
	}
	if balance.RefreshStatus != "" && balance.RefreshStatus != balanceRefreshOK {
		return true
	}
	return !balanceFreshFor(balance, now, maxAge)
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
		if runtime.Balance.Status != "ok" {
			return strategyLeastUsed, "balance_unavailable"
		}
		if runtime.Balance.RefreshStatus == balanceRefreshAuthError {
			return strategyLeastUsed, "balance_unavailable"
		}
		if !balanceFresh(runtime.Balance, now) {
			return strategyLeastUsed, "balance_stale"
		}
		scope := runtime.Balance.Scope
		if scope == "" {
			scope = balanceScopeActual
		}
		if scope != balanceScopeActual {
			return strategyLeastUsed, "balance_account_unverified"
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
			if err := normalizeAccountInput(&input); err != nil {
				a.mu.Unlock()
				writeAdminError(w, http.StatusBadRequest, err.Error())
				return
			}
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
				if err := applyNewAPIAuthInput(&account, accountConfig{}, input); err != nil {
					a.mu.Unlock()
					writeAdminError(w, http.StatusBadRequest, err.Error())
					return
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
			originChanged := baseURLMovesToDifferentOrigin(old.BaseURL, input.BaseURL)
			if old.APIKey != "" && input.APIKey == "" && !input.ClearAPIKey && originChanged {
				a.mu.Unlock()
				writeAdminError(w, http.StatusBadRequest, "Base URL 更换到不同来源时，必须重新填写 API Key 或明确清除旧 Key")
				return
			}
			if old.NewAPISecret != "" && input.NewAPISecret == "" && !input.ClearNewAPISecret &&
				input.NewAPIAuthMode != newAPIAuthAPIKey && originChanged {
				a.mu.Unlock()
				writeAdminError(w, http.StatusBadRequest, "Base URL 更换到不同来源时，必须重新填写 New API 余额凭据或切换为仅 API Key")
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
			if err := applyNewAPIAuthInput(&account, old, input); err != nil {
				a.mu.Unlock()
				writeAdminError(w, http.StatusBadRequest, err.Error())
				return
			}
			if account.Name == "" {
				account.Name = old.Name
			}
			proxyChanged := account.BaseURL != old.BaseURL || account.APIKey != old.APIKey
			if proxyChanged || newAPIAuthChanged(account, old) {
				account.Revision = old.Revision + 1
				changedAccounts[account.ID] = true
			}
			if proxyChanged {
				account.Verified = false
				account.BlockedReason = ""
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
		a.logEvent(r.Context(), slog.LevelError, "config_save_failed",
			"error_kind", logErrorKind(err), "accounts", len(candidate.Accounts),
			"changed_accounts", len(changedAccounts), "strategy", candidate.Strategy,
			"gateway_token_rotated", request.RotateGatewayToken)
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
	a.logEvent(r.Context(), slog.LevelInfo, "config_saved",
		"accounts", len(candidate.Accounts), "changed_accounts", len(changedAccounts),
		"strategy", candidate.Strategy, "gateway_token_rotated", request.RotateGatewayToken)
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
	candidate, saved, savedFound, model, err := a.prepareTestCandidate(request, false)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	if model == "" {
		writeAdminError(w, http.StatusBadRequest, "请先填写测试模型")
		return
	}
	accountRef := logReference(candidate.ID)
	logStarted := time.Now()
	matchesSaved := savedFound && sameAccountRevision(saved, candidate)
	ctx, cancel := context.WithTimeout(r.Context(), upstreamTestTime)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{
		"model": model, "input": "Reply with OK only.", "max_output_tokens": 8, "stream": false,
	})
	target, err := joinUpstreamURL(candidate.BaseURL, "/v1/responses", "")
	if err != nil {
		a.logEvent(r.Context(), slog.LevelWarn, "account_test_finished",
			"account_ref", accountRef, "result", "invalid_url", "error_kind", logErrorKind(err),
			"duration_ms", time.Since(logStarted).Milliseconds())
		writeAdminError(w, http.StatusBadRequest, "账号 URL 无效")
		return
	}
	started := a.now()
	upstreamRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		a.logEvent(r.Context(), slog.LevelError, "account_test_finished",
			"account_ref", accountRef, "result", "request_creation_failed", "error_kind", logErrorKind(err),
			"duration_ms", time.Since(logStarted).Milliseconds())
		writeAdminError(w, http.StatusInternalServerError, "无法创建测试请求")
		return
	}
	upstreamRequest.Header.Set("Authorization", "Bearer "+candidate.APIKey)
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Set("Accept", "application/json")
	response, err := a.client.Do(upstreamRequest)
	latency := a.now().Sub(started).Milliseconds()
	if err != nil {
		a.logEvent(r.Context(), slog.LevelWarn, "account_test_finished",
			"account_ref", accountRef, "result", "upstream_error", "status", 0,
			"error_kind", logErrorKind(err), "latency_ms", latency)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok": false, "statusCode": 0, "latencyMs": latency, "message": "无法连接到上游",
		})
		return
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	ok := response.StatusCode >= 200 && response.StatusCode < 300
	message := "连接成功"
	if !ok {
		message = redactSecrets(upstreamErrorMessage(response.StatusCode, body), candidate.APIKey, candidate.NewAPISecret)
	} else if matchesSaved {
		a.markAccountTestSucceeded(saved)
	}
	result := "failed"
	level := slog.LevelWarn
	if ok {
		result = "ok"
		level = slog.LevelInfo
	}
	a.logEvent(r.Context(), level, "account_test_finished",
		"account_ref", accountRef, "result", result, "status", response.StatusCode,
		"latency_ms", latency, "duration_ms", time.Since(logStarted).Milliseconds())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": ok, "statusCode": response.StatusCode, "latencyMs": latency, "message": message,
	})
}

func (a *application) prepareTestCandidate(request testRequest, includeBalanceAuth bool) (accountConfig, accountConfig, bool, string, error) {
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
		input := *request.Candidate
		if err := normalizeAccountInput(&input); err != nil {
			return accountConfig{}, accountConfig{}, false, "", err
		}
		originChanged := savedFound && baseURLMovesToDifferentOrigin(saved.BaseURL, input.BaseURL)
		if savedFound && saved.APIKey != "" && input.APIKey == "" && !input.ClearAPIKey && originChanged {
			return accountConfig{}, accountConfig{}, false, "", errors.New("Base URL 更换到不同来源时，必须重新填写 API Key 或明确清除旧 Key")
		}
		if includeBalanceAuth && savedFound && saved.NewAPISecret != "" && input.NewAPISecret == "" && !input.ClearNewAPISecret &&
			input.NewAPIAuthMode != newAPIAuthAPIKey && originChanged {
			return accountConfig{}, accountConfig{}, false, "", errors.New("Base URL 更换到不同来源时，必须重新填写 New API 余额凭据或切换为仅 API Key")
		}
		candidate.Name = input.Name
		candidate.BaseURL = input.BaseURL
		if input.ClearAPIKey {
			candidate.APIKey = ""
		} else if input.APIKey != "" {
			candidate.APIKey = input.APIKey
		}
		if includeBalanceAuth {
			if err := applyNewAPIAuthInput(&candidate, saved, input); err != nil {
				return accountConfig{}, accountConfig{}, false, "", err
			}
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
		return accountConfig{}, accountConfig{}, false, "", err
	}
	candidate = validation.Accounts[0]
	if !accountConfigured(candidate) {
		return accountConfig{}, accountConfig{}, false, "", errors.New("该账号尚未完整配置")
	}
	return candidate, saved, savedFound, model, nil
}

func (a *application) handleBalancesRefresh(w http.ResponseWriter, r *http.Request) {
	var request balanceRefreshRequest
	if r.ContentLength != 0 {
		if err := decodeJSONBody(w, r, &request); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	var ids map[string]bool
	for _, id := range request.AccountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if ids == nil {
			ids = make(map[string]bool)
		}
		ids[id] = true
	}
	maxAge := time.Duration(0)
	if r.URL.Query().Get("automatic") == "1" {
		maxAge = balanceAutoTTL
	}
	reports := a.refreshBalances(r.Context(), ids, maxAge)
	counts := countBalanceReports(reports)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     counts.Failed == 0 && counts.Canceled == 0,
		"counts": counts, "reports": reports, "status": a.status(),
	})
}

func countBalanceReports(reports []balanceRefreshReport) balanceRefreshCounts {
	counts := balanceRefreshCounts{Total: len(reports)}
	for _, report := range reports {
		switch {
		case report.Balance.RefreshStatus == balanceRefreshCanceled:
			counts.Canceled++
		case report.Balance.Status == balanceRefreshUnsupported ||
			report.Balance.RefreshStatus == balanceRefreshUnsupported:
			counts.Unsupported++
		case report.Balance.Status != "ok":
			counts.Failed++
		case report.Balance.RefreshStatus == balanceRefreshPartial:
			counts.Partial++
		case report.Balance.RefreshStatus == balanceRefreshOK || report.Balance.RefreshStatus == "":
			counts.Success++
		default:
			counts.Failed++
		}
	}
	return counts
}

func (a *application) handleBalanceTest(w http.ResponseWriter, r *http.Request) {
	var request testRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	candidate, saved, savedFound, _, err := a.prepareTestCandidate(request, true)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	accountRef := logReference(candidate.ID)
	logStarted := time.Now()
	ctx, cancel := a.newBalanceRefreshContext(r.Context())
	if !a.acquireBalanceRefresh(ctx) {
		balance := canceledBalanceAttempt(a.now())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && r.Context().Err() == nil {
			balance = timeoutBalanceAttempt(balanceSnapshot{}, a.now())
		}
		cancel()
		public := publicBalanceAt(balance, a.now())
		a.logEvent(r.Context(), slog.LevelWarn, "balance_test_finished",
			"account_ref", accountRef, "status", public.Status,
			"refresh_status", public.RefreshStatus, "error_stage", public.ErrorStage,
			"error_code", public.ErrorCode, "retryable", public.Retryable,
			"duration_ms", time.Since(logStarted).Milliseconds())
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "balance": public,
			"report": balanceRefreshReport{AccountID: candidate.ID, Balance: public},
		})
		return
	}
	defer a.releaseBalanceRefresh()
	balance := a.probeBalance(ctx, candidate)
	ctxErr := ctx.Err()
	cancel()
	if errors.Is(ctxErr, context.DeadlineExceeded) && r.Context().Err() == nil {
		balance = timeoutBalanceAttempt(balance, a.now())
	}
	if savedFound && sameAccountRevision(saved, candidate) && r.Context().Err() == nil {
		if merged, applied := a.applyBalance(saved, balance); applied {
			balance = merged
		}
	}
	public := publicBalanceAt(balance, a.now())
	report := balanceRefreshReport{AccountID: candidate.ID, Balance: public}
	level := slog.LevelWarn
	if public.Status == "ok" {
		level = slog.LevelInfo
	}
	a.logEvent(r.Context(), level, "balance_test_finished",
		"account_ref", accountRef, "status", public.Status,
		"refresh_status", public.RefreshStatus, "error_stage", public.ErrorStage,
		"error_code", public.ErrorCode, "retryable", public.Retryable,
		"duration_ms", time.Since(logStarted).Milliseconds())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": public.Status == "ok", "balance": public, "report": report,
	})
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
		a.logEvent(r.Context(), slog.LevelError, "account_reset_failed",
			"account_ref", logReference(request.ID), "error_kind", logErrorKind(err))
		writeAdminError(w, http.StatusInternalServerError, "账号状态保存失败")
		return
	}
	a.cfg = candidate
	runtime := a.runtimeForLocked(candidate.Accounts[index])
	runtime.CooldownUntil = time.Time{}
	a.mu.Unlock()
	a.logEvent(r.Context(), slog.LevelInfo, "account_reset", "account_ref", logReference(request.ID))
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
	started := time.Now()
	a.mu.Lock()
	a.requestSequence++
	requestID := a.requestSequence
	gatewayToken := a.cfg.GatewayToken
	a.mu.Unlock()
	routeKind := proxyRouteKind(r.URL.Path)
	a.logEvent(r.Context(), slog.LevelInfo, "proxy_request_started",
		"request_id", requestID, "method", r.Method, "route", routeKind,
		"content_length", r.ContentLength, "query_present", r.URL.RawQuery != "")
	if !requestHasGatewayToken(r, gatewayToken) {
		a.logEvent(r.Context(), slog.LevelWarn, "proxy_request_rejected",
			"request_id", requestID, "status", http.StatusUnauthorized, "reason", "invalid_gateway_token",
			"duration_ms", time.Since(started).Milliseconds())
		writeProxyError(w, http.StatusUnauthorized, "invalid_gateway_token", "本地网关 Token 无效")
		return
	}
	if r.ContentLength > maxProxyBody {
		a.logEvent(r.Context(), slog.LevelWarn, "proxy_request_rejected",
			"request_id", requestID, "status", http.StatusRequestEntityTooLarge, "reason", "request_body_too_large",
			"duration_ms", time.Since(started).Milliseconds())
		writeProxyError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "请求体超过 128 MiB 限制")
		return
	}
	// ponytail: request bodies stay in memory up to 128 MiB; use a 0600 temp file if larger inputs are ever required.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProxyBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			a.logEvent(r.Context(), slog.LevelWarn, "proxy_request_rejected",
				"request_id", requestID, "status", http.StatusRequestEntityTooLarge, "reason", "request_body_too_large",
				"duration_ms", time.Since(started).Milliseconds())
			writeProxyError(w, http.StatusRequestEntityTooLarge, "request_body_too_large", "请求体超过 128 MiB 限制")
			return
		}
		a.logEvent(r.Context(), slog.LevelWarn, "proxy_request_rejected",
			"request_id", requestID, "status", http.StatusBadRequest, "reason", "invalid_request_body",
			"error_kind", logErrorKind(err), "duration_ms", time.Since(started).Milliseconds())
		writeProxyError(w, http.StatusBadRequest, "invalid_request_body", "无法读取请求体")
		return
	}
	attempted := make(map[string]bool)
	var previous *http.Response
	previousAccountRef := "none"
	previousAttempt := 0
	for {
		account, ok := a.selectAccount(r.Context(), attempted)
		if !ok {
			if previous != nil {
				defer previous.Body.Close()
				a.logEvent(r.Context(), slog.LevelWarn, "proxy_no_candidate",
					"request_id", requestID, "attempts", len(attempted), "last_status", previous.StatusCode)
				a.forwardResponse(w, previous, requestID, previousAccountRef, previousAttempt, started)
				return
			}
			a.logEvent(r.Context(), slog.LevelWarn, "proxy_request_rejected",
				"request_id", requestID, "status", http.StatusServiceUnavailable, "reason", "no_available_account",
				"attempts", len(attempted), "duration_ms", time.Since(started).Milliseconds())
			writeProxyError(w, http.StatusServiceUnavailable, "upstream_not_available", "没有可用账号")
			return
		}
		attempt := len(attempted) + 1
		accountRef := logReference(account.ID)
		attempted[account.ID] = true
		if previous != nil {
			previous.Body.Close()
			previous = nil
		}
		attemptStarted := time.Now()
		response, sendErr := a.sendUpstream(r.Context(), r, body, account)
		if sendErr != nil {
			a.logEvent(r.Context(), slog.LevelError, "proxy_upstream_failed",
				"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
				"status", http.StatusBadGateway, "request_bytes", len(body), "error_kind", logErrorKind(sendErr),
				"latency_ms", time.Since(attemptStarted).Milliseconds(), "duration_ms", time.Since(started).Milliseconds())
			writeProxyError(w, http.StatusBadGateway, "upstream_unavailable", "无法连接到上游账号")
			return
		}
		stream := strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
		responseLevel := slog.LevelInfo
		if response.StatusCode >= 400 {
			responseLevel = slog.LevelWarn
		}
		a.logEvent(r.Context(), responseLevel, "proxy_upstream_response",
			"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
			"status", response.StatusCode, "stream", stream, "request_bytes", len(body),
			"latency_ms", time.Since(attemptStarted).Milliseconds())
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			a.markAccountVerified(account)
			defer response.Body.Close()
			a.forwardResponse(w, response, requestID, accountRef, attempt, started)
			return
		}
		if response.StatusCode >= 500 {
			defer response.Body.Close()
			a.forwardResponse(w, response, requestID, accountRef, attempt, started)
			return
		}
		retry := false
		if response.StatusCode == http.StatusUnauthorized {
			action := a.handleAccountUnauthorized(account)
			a.logEvent(r.Context(), slog.LevelWarn, "proxy_account_retry",
				"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
				"reason", "unauthorized", "action", action)
			retry = true
		} else {
			classification, inspectErr := inspectErrorResponse(response)
			if inspectErr != nil {
				a.logEvent(r.Context(), slog.LevelWarn, "proxy_response_inspection_failed",
					"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
					"error_kind", logErrorKind(inspectErr))
			} else {
				switch classification {
				case "quota":
					action := a.blockAccount(account, "quota")
					a.logEvent(r.Context(), slog.LevelWarn, "proxy_account_retry",
						"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
						"reason", "quota", "action", action)
					retry = true
				case "rate_limit":
					action := a.cooldownAccount(account)
					a.logEvent(r.Context(), slog.LevelWarn, "proxy_account_retry",
						"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
						"reason", "rate_limit", "action", action)
					retry = true
				}
			}
		}
		if !retry {
			defer response.Body.Close()
			a.forwardResponse(w, response, requestID, accountRef, attempt, started)
			return
		}
		previous = response
		previousAccountRef = accountRef
		previousAttempt = attempt
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
			if accountState(account, runtime, now) == "available" && balanceRefreshDue(runtime.Balance, now, balanceTTL) {
				stale[account.ID] = true
			}
		}
	}
	a.mu.Unlock()
	if len(stale) != 0 {
		routingTimeout := a.balanceRoutingTimeout
		if routingTimeout <= 0 {
			routingTimeout = balanceRoutingTime
		}
		routeCtx, cancel := context.WithTimeout(ctx, routingTimeout)
		a.refreshBalancesForRoute(routeCtx, stale, balanceTTL)
		routeErr := routeCtx.Err()
		cancel()
		if errors.Is(routeErr, context.DeadlineExceeded) {
			a.markBalanceRouteTimeout(stale)
		}
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

func (a *application) markBalanceRouteTimeout(ids map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	for _, account := range a.cfg.Accounts {
		if !ids[account.ID] {
			continue
		}
		runtime := a.runtimeForLocked(account)
		if !balanceRefreshDue(runtime.Balance, now, balanceTTL) {
			continue
		}
		failures := runtime.Balance.Failures + 1
		if failures < 1 {
			failures = 1
		}
		runtime.Balance.RefreshStatus = balanceRefreshError
		runtime.Balance.ErrorStage = balanceStageAccount
		runtime.Balance.ErrorCode = balanceErrorTimeout
		runtime.Balance.Retryable = true
		runtime.Balance.Failures = failures
		runtime.Balance.NextRetryAt = now.Add(balanceRetryDelay(failures, true))
		if runtime.Balance.UpdatedAt.IsZero() {
			runtime.Balance.Status = balanceRefreshError
		}
	}
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
	if err := saveConfig(a.configPath, candidate); err == nil {
		a.cfg = candidate
	} else {
		a.logEvent(context.Background(), slog.LevelError, "account_state_persist_failed",
			"account_ref", logReference(expected.ID), "state", "verified", "error_kind", logErrorKind(err))
	}
}

func (a *application) handleAccountUnauthorized(expected accountConfig) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return "stale"
	}
	if !a.cfg.Accounts[index].Verified {
		a.runtimeForLocked(a.cfg.Accounts[index]).CooldownUntil = a.now().Add(accountCooldown)
		return "cooldown"
	}
	a.blockAccountLocked(index, "unauthorized")
	return "blocked"
}

func (a *application) blockAccount(expected accountConfig, reason string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return "stale"
	}
	a.blockAccountLocked(index, reason)
	return "blocked"
}

func (a *application) blockAccountLocked(index int, reason string) {
	now := a.now()
	candidate := cloneConfig(a.cfg)
	candidate.Accounts[index].BlockedReason = reason
	candidate.LastSwitchReason = "account_" + reason
	candidate.LastSwitchAt = now.UTC().Format(time.RFC3339)
	if err := saveConfig(a.configPath, candidate); err == nil {
		a.cfg = candidate
	} else {
		a.cfg.Accounts[index].BlockedReason = reason
		a.cfg.LastSwitchReason = candidate.LastSwitchReason
		a.cfg.LastSwitchAt = candidate.LastSwitchAt
		a.logEvent(context.Background(), slog.LevelError, "account_state_persist_failed",
			"account_ref", logReference(candidate.Accounts[index].ID), "state", "blocked",
			"reason", reason, "error_kind", logErrorKind(err))
	}
	a.runtimeForLocked(a.cfg.Accounts[index]).CooldownUntil = time.Time{}
}

func (a *application) cooldownAccount(expected accountConfig) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return "stale"
	}
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	runtime.CooldownUntil = a.now().Add(accountCooldown)
	return "cooldown"
}

func sameAccountRevision(current, expected accountConfig) bool {
	return current.ID == expected.ID && current.Revision == expected.Revision &&
		current.BaseURL == expected.BaseURL && current.APIKey == expected.APIKey &&
		normalizeNewAPIAuthMode(current.NewAPIAuthMode) == normalizeNewAPIAuthMode(expected.NewAPIAuthMode) &&
		current.NewAPIUsername == expected.NewAPIUsername && current.NewAPIUserID == expected.NewAPIUserID &&
		current.NewAPISecret == expected.NewAPISecret
}

func mergeBalanceAttempt(previous, attempt balanceSnapshot) balanceSnapshot {
	if attempt.CheckedAt.IsZero() {
		attempt.CheckedAt = attempt.UpdatedAt
	}
	if attempt.RefreshStatus == "" {
		attempt.RefreshStatus = attempt.Status
	}
	if attempt.RefreshStatus == balanceRefreshOK {
		attempt.Status = "ok"
		attempt.Failures = 0
		attempt.NextRetryAt = time.Time{}
		attempt.ErrorStage = ""
		attempt.ErrorCode = ""
		attempt.Retryable = false
		return attempt
	}

	failures := previous.Failures + 1
	if failures < 1 {
		failures = 1
	}
	attempt.Failures = failures
	attempt.NextRetryAt = attempt.CheckedAt.Add(balanceRetryDelay(failures, attempt.Retryable))
	if attempt.RefreshStatus == balanceRefreshPartial {
		attempt.Status = "ok"
		if previous.UpdatedAt.IsZero() {
			return attempt
		}
		merged := previous
		if previous.Status == "ok" {
			merged.Status = "ok"
			merged.RefreshStatus = attempt.RefreshStatus
			merged.ErrorStage = attempt.ErrorStage
			merged.ErrorCode = attempt.ErrorCode
		} else if merged.RefreshStatus == "" || merged.RefreshStatus == balanceRefreshOK {
			merged.RefreshStatus = merged.Status
		}
		merged.CheckedAt = attempt.CheckedAt
		merged.Retryable = attempt.Retryable
		merged.Failures = attempt.Failures
		merged.NextRetryAt = attempt.NextRetryAt
		if previous.Status == "ok" && !attempt.Retryable {
			merged.Status = balanceRefreshError
		}
		return merged
	}
	if previous.UpdatedAt.IsZero() {
		return attempt
	}

	merged := previous
	merged.RefreshStatus = attempt.RefreshStatus
	merged.CheckedAt = attempt.CheckedAt
	merged.ErrorStage = attempt.ErrorStage
	merged.ErrorCode = attempt.ErrorCode
	merged.Retryable = attempt.Retryable
	merged.Failures = attempt.Failures
	merged.NextRetryAt = attempt.NextRetryAt
	if attempt.RefreshStatus == balanceRefreshAuthError || attempt.RefreshStatus == balanceRefreshUnsupported ||
		!attempt.Retryable {
		merged.Status = attempt.Status
	}
	return merged
}

func (a *application) applyBalance(expected accountConfig, balance balanceSnapshot) (balanceSnapshot, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return balanceSnapshot{}, false
	}
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	if !balance.CheckedAt.IsZero() && !runtime.Balance.CheckedAt.IsZero() &&
		balance.CheckedAt.Before(runtime.Balance.CheckedAt) {
		return runtime.Balance, false
	}
	runtime.Balance = mergeBalanceAttempt(runtime.Balance, balance)
	return runtime.Balance, true
}

func newAPIAuthHash(account accountConfig) [sha256.Size]byte {
	value := strings.Join([]string{
		account.BaseURL,
		normalizeNewAPIAuthMode(account.NewAPIAuthMode),
		account.NewAPIUsername,
		strconv.Itoa(account.NewAPIUserID),
		account.NewAPISecret,
	}, "\x00")
	return sha256.Sum256([]byte(value))
}

func (a *application) cachedNewAPISession(expected accountConfig) (string, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return "", 0
	}
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	if runtime.NewAPIAuthHash != newAPIAuthHash(expected) {
		return "", 0
	}
	return runtime.NewAPISession, runtime.NewAPIUserID
}

func (a *application) cacheNewAPISession(expected accountConfig, cookie string, userID int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return
	}
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	runtime.NewAPISession = cookie
	runtime.NewAPIUserID = userID
	runtime.NewAPIAuthHash = newAPIAuthHash(expected)
}

func (a *application) clearNewAPISession(expected accountConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return
	}
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	runtime.NewAPISession = ""
	runtime.NewAPIUserID = 0
	runtime.NewAPIAuthHash = [sha256.Size]byte{}
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

func (a *application) newBalanceRefreshContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	timeout := a.balanceTimeout
	if timeout <= 0 {
		timeout = balanceRefreshTime
	}
	return context.WithTimeout(parent, timeout)
}

func (a *application) acquireBalanceRefresh(ctx context.Context) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case a.balanceRefreshGate <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (a *application) releaseBalanceRefresh() {
	<-a.balanceRefreshGate
}

func timeoutBalanceAttempt(balance balanceSnapshot, checkedAt time.Time) balanceSnapshot {
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}
	balance.Status = balanceRefreshError
	balance.RefreshStatus = balanceRefreshError
	balance.CheckedAt = checkedAt
	balance.UpdatedAt = time.Time{}
	balance.ErrorStage = balanceStageAccount
	balance.ErrorCode = balanceErrorTimeout
	balance.Retryable = true
	return balance
}

func canceledBalanceAttempt(checkedAt time.Time) balanceSnapshot {
	return balanceSnapshot{
		Status: balanceRefreshError, RefreshStatus: balanceRefreshCanceled, CheckedAt: checkedAt,
		ErrorStage: balanceStageAccount, ErrorCode: balanceErrorCanceled,
	}
}

func (a *application) refreshBalances(ctx context.Context, ids map[string]bool, maxAge time.Duration) []balanceRefreshReport {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	accountCount := 0
	defer func() {
		result := "ok"
		level := slog.LevelInfo
		if ctx.Err() != nil {
			result = "interrupted"
			level = slog.LevelWarn
		}
		a.logEvent(context.Background(), level, "balance_refresh_finished",
			"accounts", accountCount, "automatic", maxAge > 0, "filtered", ids != nil,
			"result", result, "error_kind", logErrorKind(ctx.Err()),
			"duration_ms", time.Since(started).Milliseconds())
	}()
	if !a.acquireBalanceRefresh(ctx) {
		return nil
	}
	defer a.releaseBalanceRefresh()
	accounts, order := a.prepareBalanceRefresh(ids, maxAge)
	accountCount = len(accounts)
	return a.refreshBalanceAccounts(ctx, accounts, order)
}

func (a *application) refreshBalancesForRoute(ctx context.Context, ids map[string]bool, maxAge time.Duration) {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	if !a.acquireBalanceRefresh(ctx) {
		a.logEvent(context.Background(), slog.LevelWarn, "balance_refresh_finished",
			"accounts", 0, "automatic", maxAge > 0, "filtered", ids != nil,
			"result", "interrupted", "error_kind", logErrorKind(ctx.Err()),
			"duration_ms", time.Since(started).Milliseconds())
		return
	}
	accounts, order := a.prepareBalanceRefresh(ids, maxAge)
	if len(accounts) == 0 {
		a.releaseBalanceRefresh()
		a.logEvent(context.Background(), slog.LevelInfo, "balance_refresh_finished",
			"accounts", 0, "automatic", maxAge > 0, "filtered", ids != nil,
			"result", "ok", "error_kind", "none",
			"duration_ms", time.Since(started).Milliseconds())
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer a.releaseBalanceRefresh()
		a.refreshBalanceAccounts(context.Background(), accounts, order)
		a.logEvent(context.Background(), slog.LevelInfo, "balance_refresh_finished",
			"accounts", len(accounts), "automatic", maxAge > 0, "filtered", ids != nil,
			"result", "ok", "error_kind", "none",
			"duration_ms", time.Since(started).Milliseconds())
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func (a *application) prepareBalanceRefresh(ids map[string]bool, maxAge time.Duration) ([]accountConfig, map[string]int) {
	a.mu.Lock()
	now := a.now()
	accounts := make([]accountConfig, 0, len(a.cfg.Accounts))
	updatedAt := make(map[string]time.Time, len(a.cfg.Accounts))
	order := make(map[string]int, len(a.cfg.Accounts))
	for _, account := range a.cfg.Accounts {
		if ids != nil && !ids[account.ID] {
			continue
		}
		if maxAge > 0 && !account.Enabled {
			continue
		}
		runtime := a.runtimeForLocked(account)
		if accountConfigured(account) && balanceRefreshDue(runtime.Balance, now, maxAge) {
			order[account.ID] = len(order)
			accounts = append(accounts, account)
			updatedAt[account.ID] = runtime.Balance.UpdatedAt
		}
	}
	a.mu.Unlock()
	if maxAge > 0 {
		sortAccountsByBalanceAge(accounts, updatedAt)
	}
	return accounts, order
}

func (a *application) refreshBalanceAccounts(ctx context.Context, accounts []accountConfig, order map[string]int) []balanceRefreshReport {
	workerCount := balanceWorkers
	if len(accounts) < workerCount {
		workerCount = len(accounts)
	}
	jobs := make(chan accountConfig)
	reports := make(chan balanceRefreshReport, len(accounts))
	var wait sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for account := range jobs {
				accountCtx, cancel := a.newBalanceRefreshContext(ctx)
				balance := a.probeBalance(accountCtx, account)
				accountErr := accountCtx.Err()
				cancel()
				if ctx.Err() != nil {
					reports <- balanceRefreshReport{
						AccountID: account.ID, Balance: publicBalanceAt(canceledBalanceAttempt(a.now()), a.now()),
					}
					continue
				}
				if errors.Is(accountErr, context.DeadlineExceeded) {
					balance = timeoutBalanceAttempt(balance, a.now())
				}
				merged, applied := a.applyBalance(account, balance)
				if applied {
					reports <- balanceRefreshReport{AccountID: account.ID, Balance: publicBalanceAt(merged, a.now())}
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
			close(reports)
			result := make([]balanceRefreshReport, 0, len(reports))
			for report := range reports {
				result = append(result, report)
			}
			return result
		}
	}
	close(jobs)
	wait.Wait()
	close(reports)
	result := make([]balanceRefreshReport, 0, len(accounts))
	for report := range reports {
		result = append(result, report)
	}
	sort.SliceStable(result, func(left, right int) bool {
		return order[result[left].AccountID] < order[result[right].AccountID]
	})
	return result
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

func balanceFailureFor(stage string, statusCode int, err error) balanceFailure {
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return balanceFailure{Status: balanceRefreshError, Stage: stage, Code: balanceErrorTimeout, Retryable: true}
		}
		if errors.Is(err, context.Canceled) {
			return balanceFailure{Status: balanceRefreshCanceled, Stage: stage, Code: balanceErrorCanceled}
		}
		return balanceFailure{Status: balanceRefreshError, Stage: stage, Code: balanceErrorNetwork, Retryable: true}
	}
	accountStage := stage == balanceStageAccountLogin || stage == balanceStageAccountQuota
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		code := balanceErrorAPIKeyAuth
		if accountStage {
			code = balanceErrorAccountAuth
		}
		return balanceFailure{Status: balanceRefreshAuthError, Stage: stage, Code: code}
	case statusCode == http.StatusNotFound:
		return balanceFailure{Status: balanceRefreshUnsupported, Stage: stage, Code: balanceErrorUnsupported}
	case statusCode == http.StatusTooManyRequests:
		return balanceFailure{Status: balanceRefreshError, Stage: stage, Code: balanceErrorRateLimited, Retryable: true}
	case statusCode >= 500:
		return balanceFailure{Status: balanceRefreshError, Stage: stage, Code: balanceErrorUpstream, Retryable: true}
	default:
		return balanceFailure{Status: balanceRefreshError, Stage: stage, Code: balanceErrorRejected}
	}
}

func failureFromError(stage string, err error) balanceFailure {
	var probeErr *balanceProbeError
	if errors.As(err, &probeErr) {
		return probeErr.failure
	}
	if errors.Is(err, errNewAPIAuthentication) {
		return balanceFailure{Status: balanceRefreshAuthError, Stage: stage, Code: balanceErrorAccountAuth}
	}
	return balanceFailureFor(stage, 0, err)
}

func balanceAttemptFromFailure(checkedAt time.Time, failure balanceFailure) balanceSnapshot {
	status := failure.Status
	if status == balanceRefreshCanceled {
		status = balanceRefreshError
	}
	return balanceSnapshot{
		Status: status, RefreshStatus: failure.Status, CheckedAt: checkedAt,
		ErrorStage: failure.Stage, ErrorCode: failure.Code, Retryable: failure.Retryable,
	}
}

func markBalancePartial(balance balanceSnapshot, failure balanceFailure) balanceSnapshot {
	balance.Status = "ok"
	balance.RefreshStatus = balanceRefreshPartial
	balance.ErrorStage = failure.Stage
	balance.ErrorCode = failure.Code
	balance.Retryable = failure.Retryable
	return balance
}

func (a *application) probeTokenBalance(ctx context.Context, account accountConfig, checkedAt time.Time) tokenBalanceResult {
	usageURL, err := balanceAPIURL(account.BaseURL, "/api/usage/token/")
	if err != nil {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageTokenUsage, Code: balanceErrorInvalidResponse}
		return tokenBalanceResult{failure: &failure}
	}
	status, body, requestErr := a.getUpstreamJSON(ctx, usageURL, account.APIKey)
	if requestErr != nil {
		failure := balanceFailureFor(balanceStageTokenUsage, status, requestErr)
		return tokenBalanceResult{failure: &failure}
	}
	if status == http.StatusNotFound {
		balance := a.probeDashboardBalance(ctx, account, checkedAt)
		if balance.Status != "ok" {
			failure := balanceFailure{
				Status: balance.RefreshStatus, Stage: balance.ErrorStage, Code: balance.ErrorCode, Retryable: balance.Retryable,
			}
			return tokenBalanceResult{balance: balance, failure: &failure, fromDashboard: true}
		}
		return tokenBalanceResult{balance: balance, fromDashboard: true}
	}
	if status < 200 || status >= 300 {
		failure := balanceFailureFor(balanceStageTokenUsage, status, nil)
		return tokenBalanceResult{failure: &failure}
	}
	payload, ok := decodeJSONObject(body)
	if !ok {
		balance := a.probeDashboardBalance(ctx, account, checkedAt)
		if balance.Status != "ok" {
			failure := balanceFailure{
				Status: balance.RefreshStatus, Stage: balance.ErrorStage, Code: balance.ErrorCode, Retryable: balance.Retryable,
			}
			return tokenBalanceResult{balance: balance, failure: &failure, fromDashboard: true}
		}
		return tokenBalanceResult{balance: balance, fromDashboard: true}
	}
	if success, exists := boolValue(payload["success"]); exists && !success {
		failure := balanceFailure{
			Status: balanceRefreshAuthError, Stage: balanceStageTokenUsage, Code: balanceErrorAPIKeyAuth,
		}
		return tokenBalanceResult{failure: &failure}
	}
	data := nestedObject(payload, "data")
	unlimited, _ := boolValue(data["unlimited_quota"])
	total, hasTotal := numberValue(data["total_available"])
	if !unlimited && !hasTotal {
		balance := a.probeDashboardBalance(ctx, account, checkedAt)
		if balance.Status != "ok" {
			failure := balanceFailure{
				Status: balance.RefreshStatus, Stage: balance.ErrorStage, Code: balance.ErrorCode, Retryable: balance.Retryable,
			}
			return tokenBalanceResult{balance: balance, failure: &failure, fromDashboard: true}
		}
		return tokenBalanceResult{balance: balance, fromDashboard: true}
	}
	return tokenBalanceResult{balance: balanceSnapshot{
		Status: "ok", Amount: total, Unlimited: unlimited, Scope: balanceScopeTokenOnly,
		DisplayLabel: "站点额度", UpdatedAt: checkedAt, RefreshStatus: balanceRefreshOK, CheckedAt: checkedAt,
	}}
}

func (a *application) probeQuotaMetadata(ctx context.Context, account accountConfig) quotaMetadataResult {
	data, statusCode, err := a.getNewAPIStatus(ctx, account)
	if err != nil {
		failure := failureFromError(balanceStageQuotaMetadata, err)
		return quotaMetadataResult{statusCode: statusCode, failure: &failure}
	}
	return quotaMetadataResult{data: data, statusCode: statusCode}
}

func (a *application) probeAccountQuota(ctx context.Context, account accountConfig) accountQuotaResult {
	quota, err := a.probeNewAPIAccountQuota(ctx, account)
	if err != nil {
		failure := failureFromError(balanceStageAccountQuota, err)
		return accountQuotaResult{failure: &failure}
	}
	if quota < 0 {
		quota = 0
	}
	return accountQuotaResult{quota: quota}
}

func (a *application) probeBalance(ctx context.Context, account accountConfig) (result balanceSnapshot) {
	started := time.Now()
	defer func() {
		status := result.Status
		if status == "" {
			status = "unknown"
		}
		level := slog.LevelInfo
		if status == balanceRefreshError || status == balanceRefreshAuthError {
			level = slog.LevelWarn
		}
		a.logEvent(context.Background(), level, "balance_probe_finished",
			"account_ref", logReference(account.ID), "status", status,
			"refresh_status", result.RefreshStatus, "scope", result.Scope, "limited_by", result.LimitedBy,
			"error_stage", result.ErrorStage, "error_code", result.ErrorCode, "retryable", result.Retryable,
			"context_error", logErrorKind(ctx.Err()), "duration_ms", time.Since(started).Milliseconds())
	}()
	checkedAt := a.now()
	tokenChannel := make(chan tokenBalanceResult, 1)
	metadataChannel := make(chan quotaMetadataResult, 1)
	go func() { tokenChannel <- a.probeTokenBalance(ctx, account, checkedAt) }()
	go func() { metadataChannel <- a.probeQuotaMetadata(ctx, account) }()

	mode := normalizeNewAPIAuthMode(account.NewAPIAuthMode)
	var accountChannel chan accountQuotaResult
	if mode != newAPIAuthAPIKey {
		accountChannel = make(chan accountQuotaResult, 1)
		go func() { accountChannel <- a.probeAccountQuota(ctx, account) }()
	}

	token := <-tokenChannel
	metadata := <-metadataChannel
	var accountQuota accountQuotaResult
	if accountChannel != nil {
		accountQuota = <-accountChannel
	}

	if token.failure != nil {
		result = balanceAttemptFromFailure(checkedAt, *token.failure)
		if token.failure.Status != balanceRefreshAuthError && accountChannel != nil &&
			accountQuota.failure == nil && metadata.failure == nil {
			amount, unit, label, converted := convertNewAPIQuota(accountQuota.quota, metadata.data)
			if converted {
				partial := balanceSnapshot{
					Status: "ok", Amount: amount, Unit: unit, DisplayLabel: label,
					Scope: balanceScopeAccountOnly, UpdatedAt: checkedAt, CheckedAt: checkedAt,
				}
				return markBalancePartial(partial, *token.failure)
			}
		}
		return result
	}

	result = token.balance
	result.CheckedAt = checkedAt
	if accountChannel != nil && accountQuota.failure != nil {
		result.Status = accountQuota.failure.Status
		result.RefreshStatus = accountQuota.failure.Status
		result.UpdatedAt = time.Time{}
		result.ErrorStage = accountQuota.failure.Stage
		result.ErrorCode = accountQuota.failure.Code
		result.Retryable = accountQuota.failure.Retryable
		return result
	}

	if token.fromDashboard && mode != newAPIAuthAPIKey {
		if metadata.failure != nil {
			if result.Unit != "" {
				result.Scope = balanceScopeTokenOnly
				return markBalancePartial(result, *metadata.failure)
			}
			return balanceAttemptFromFailure(checkedAt, *metadata.failure)
		}
		accountAmount, accountUnit, _, converted := convertNewAPIQuota(accountQuota.quota, metadata.data)
		if !converted {
			failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageQuotaMetadata, Code: balanceErrorInvalidResponse, Retryable: true}
			return balanceAttemptFromFailure(checkedAt, failure)
		}
		if result.hasHardLimit {
			amount, unit, label, converted := convertUSDToDisplay(result.Amount, metadata.data)
			if !converted {
				failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageQuotaMetadata, Code: balanceErrorInvalidResponse, Retryable: true}
				return balanceAttemptFromFailure(checkedAt, failure)
			}
			result.Amount = amount
			result.Unit = unit
			result.DisplayLabel = label
		}
		if result.Unit != accountUnit {
			failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageQuotaMetadata, Code: balanceErrorInvalidResponse, Retryable: true}
			return balanceAttemptFromFailure(checkedAt, failure)
		}
		result.Scope = balanceScopeActual
		result.Unlimited = false
		if accountAmount < result.Amount {
			result.Amount = accountAmount
			result.LimitedBy = "account"
		} else {
			result.LimitedBy = "token"
		}
	} else if accountChannel != nil {
		result.Scope = balanceScopeActual
		result.Unlimited = false
		if token.balance.Unlimited || accountQuota.quota < result.Amount {
			result.Amount = accountQuota.quota
			result.LimitedBy = "account"
		} else {
			result.LimitedBy = "token"
		}
	}

	if metadata.failure != nil {
		if token.fromDashboard && mode == newAPIAuthAPIKey && metadata.statusCode == http.StatusNotFound {
			if result.hasHardLimit && result.hardLimit >= newAPIUnlimitedLimit {
				result.Unlimited = true
			} else {
				result.Scope = balanceScopeActual
			}
			result.RefreshStatus = balanceRefreshOK
			return result
		}
		if result.Unit != "" {
			return markBalancePartial(result, *metadata.failure)
		}
		return balanceAttemptFromFailure(checkedAt, *metadata.failure)
	}
	if !token.fromDashboard || result.Unit == "" {
		if amount, unit, label, converted := convertNewAPIQuota(result.Amount, metadata.data); converted {
			result.Amount = amount
			result.Unit = unit
			result.DisplayLabel = label
		} else {
			failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageQuotaMetadata, Code: balanceErrorInvalidResponse, Retryable: true}
			return balanceAttemptFromFailure(checkedAt, failure)
		}
	}

	if mode == newAPIAuthAPIKey && !token.fromDashboard && result.Unlimited {
		dashboard := a.probeDashboardBalance(ctx, account, checkedAt)
		if dashboard.Status == "ok" && dashboard.hasHardLimit && dashboard.hardLimit < newAPIUnlimitedLimit {
			amount, unit, label, converted := convertUSDToDisplay(dashboard.Amount, metadata.data)
			if converted && result.Unit == unit {
				dashboard.Amount = amount
				dashboard.Unit = unit
				dashboard.DisplayLabel = label
				dashboard.Scope = balanceScopeActual
				dashboard.LimitedBy = "account"
				return dashboard
			}
		}
	}
	result.Status = "ok"
	result.UpdatedAt = checkedAt
	result.RefreshStatus = balanceRefreshOK
	return result
}

func (a *application) probeBalanceFallback(ctx context.Context, account accountConfig, checkedAt time.Time) balanceSnapshot {
	return a.probeDashboardBalance(ctx, account, checkedAt)
}

func (a *application) getNewAPIStatus(ctx context.Context, account accountConfig) (map[string]any, int, error) {
	target, err := balanceAPIURL(account.BaseURL, "/api/status")
	if err != nil {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageQuotaMetadata, Code: balanceErrorInvalidResponse}
		return nil, 0, &balanceProbeError{failure: failure, cause: err}
	}
	status, body, err := a.getUpstreamJSON(ctx, target, account.APIKey)
	if err != nil {
		failure := balanceFailureFor(balanceStageQuotaMetadata, status, err)
		return nil, status, &balanceProbeError{failure: failure, cause: err}
	}
	if status < 200 || status >= 300 {
		failure := balanceFailureFor(balanceStageQuotaMetadata, status, nil)
		return nil, status, &balanceProbeError{failure: failure}
	}
	payload, ok := decodeJSONObject(body)
	if !ok {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageQuotaMetadata, Code: balanceErrorInvalidResponse, Retryable: true}
		return nil, status, &balanceProbeError{failure: failure}
	}
	return nestedObject(payload, "data"), status, nil
}

func convertNewAPIQuota(amount float64, statusData map[string]any) (float64, string, string, bool) {
	unit := displayUnit(statusData["quota_display_type"])
	if unit == "TOKENS" {
		return amount, unit, unit, true
	}
	perUnit, hasPerUnit := numberValue(statusData["quota_per_unit"])
	if !hasPerUnit || perUnit <= 0 {
		return 0, "", "", false
	}
	return convertUSDToDisplay(amount/perUnit, statusData)
}

func convertUSDToDisplay(amount float64, statusData map[string]any) (float64, string, string, bool) {
	unit := displayUnit(statusData["quota_display_type"])
	switch unit {
	case "USD":
		return amount, unit, unit, true
	case "CNY":
		rate, ok := numberValue(statusData["usd_exchange_rate"])
		if !ok || rate <= 0 {
			return 0, "", "", false
		}
		return amount * rate, unit, unit, true
	case "CUSTOM":
		rate, ok := numberValue(statusData["custom_currency_exchange_rate"])
		if !ok || rate <= 0 {
			return 0, "", "", false
		}
		label, _ := statusData["custom_currency_symbol"].(string)
		label = strings.TrimSpace(label)
		if label == "" {
			label = "CUSTOM"
		}
		return amount * rate, fmt.Sprintf("CUSTOM:%s:%g", label, rate), label, true
	default:
		return 0, "", "", false
	}
}

func (a *application) probeNewAPIAccountQuota(ctx context.Context, account accountConfig) (float64, error) {
	switch normalizeNewAPIAuthMode(account.NewAPIAuthMode) {
	case newAPIAuthAccessToken:
		return a.getNewAPIUserQuota(ctx, account, "", account.NewAPIUserID)
	case newAPIAuthPassword:
		if cookie, userID := a.cachedNewAPISession(account); cookie != "" && userID > 0 {
			quota, err := a.getNewAPIUserQuota(ctx, account, cookie, userID)
			if err == nil {
				return quota, nil
			}
			if !errors.Is(err, errNewAPIAuthentication) {
				return 0, err
			}
			a.clearNewAPISession(account)
		}
		cookie, userID, err := a.loginNewAPI(ctx, account)
		if err != nil {
			return 0, err
		}
		a.cacheNewAPISession(account, cookie, userID)
		return a.getNewAPIUserQuota(ctx, account, cookie, userID)
	default:
		return 0, errors.New("New API account authentication is not configured")
	}
}

func (a *application) loginNewAPI(ctx context.Context, account accountConfig) (string, int, error) {
	target, err := balanceAPIURL(account.BaseURL, "/api/user/login")
	if err != nil {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountLogin, Code: balanceErrorInvalidResponse}
		return "", 0, &balanceProbeError{failure: failure, cause: err}
	}
	body, _ := json.Marshal(map[string]string{"username": account.NewAPIUsername, "password": account.NewAPISecret})
	status, responseBody, cookies, err := a.requestUpstreamJSON(ctx, http.MethodPost, target, body, nil)
	if err != nil {
		failure := balanceFailureFor(balanceStageAccountLogin, status, err)
		return "", 0, &balanceProbeError{failure: failure, cause: err}
	}
	if status < 200 || status >= 300 {
		failure := balanceFailureFor(balanceStageAccountLogin, status, nil)
		var cause error
		if failure.Status == balanceRefreshAuthError {
			cause = errNewAPIAuthentication
		}
		return "", 0, &balanceProbeError{failure: failure, cause: cause}
	}
	payload, ok := decodeJSONObject(responseBody)
	if !ok {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountLogin, Code: balanceErrorInvalidResponse, Retryable: true}
		return "", 0, &balanceProbeError{failure: failure}
	}
	if success, exists := boolValue(payload["success"]); exists && !success {
		failure := balanceFailure{Status: balanceRefreshAuthError, Stage: balanceStageAccountLogin, Code: balanceErrorAccountAuth}
		return "", 0, &balanceProbeError{failure: failure, cause: errNewAPIAuthentication}
	}
	data := nestedObject(payload, "data")
	if require2FA, _ := boolValue(data["require_2fa"]); require2FA {
		failure := balanceFailure{Status: balanceRefreshAuthError, Stage: balanceStageAccountLogin, Code: balanceErrorTwoFactor}
		return "", 0, &balanceProbeError{failure: failure, cause: errNewAPIAuthentication}
	}
	var userIDText string
	switch value := data["id"].(type) {
	case json.Number:
		userIDText = value.String()
	case string:
		userIDText = strings.TrimSpace(value)
	}
	if userIDText == "" {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountLogin, Code: balanceErrorInvalidResponse}
		return "", 0, &balanceProbeError{failure: failure}
	}
	userIDValue, err := strconv.ParseInt(userIDText, 10, strconv.IntSize)
	if err != nil || userIDValue < 1 {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountLogin, Code: balanceErrorInvalidResponse}
		return "", 0, &balanceProbeError{failure: failure, cause: err}
	}
	userID := int(userIDValue)
	cookie := cookieHeader(cookies)
	if cookie == "" {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountLogin, Code: balanceErrorInvalidResponse}
		return "", 0, &balanceProbeError{failure: failure}
	}
	return cookie, userID, nil
}

func (a *application) getNewAPIUserQuota(ctx context.Context, account accountConfig, cookie string, userID int) (float64, error) {
	target, err := balanceAPIURL(account.BaseURL, "/api/user/self")
	if err != nil {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountQuota, Code: balanceErrorInvalidResponse}
		return 0, &balanceProbeError{failure: failure, cause: err}
	}
	headers := make(http.Header)
	headers.Set("New-Api-User", strconv.Itoa(userID))
	if cookie != "" {
		headers.Set("Cookie", cookie)
	} else {
		headers.Set("Authorization", "Bearer "+account.NewAPISecret)
	}
	status, body, _, err := a.requestUpstreamJSON(ctx, http.MethodGet, target, nil, headers)
	if err != nil {
		failure := balanceFailureFor(balanceStageAccountQuota, status, err)
		return 0, &balanceProbeError{failure: failure, cause: err}
	}
	if status < 200 || status >= 300 {
		failure := balanceFailureFor(balanceStageAccountQuota, status, nil)
		var cause error
		if failure.Status == balanceRefreshAuthError {
			if cookie == "" {
				failure.Code = accessTokenAuthErrorCode(body)
			}
			cause = errNewAPIAuthentication
		}
		return 0, &balanceProbeError{failure: failure, cause: cause}
	}
	payload, ok := decodeJSONObject(body)
	if !ok {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountQuota, Code: balanceErrorInvalidResponse, Retryable: true}
		return 0, &balanceProbeError{failure: failure}
	}
	if success, exists := boolValue(payload["success"]); exists && !success {
		code := balanceErrorAccountAuth
		if cookie == "" {
			code = accessTokenAuthErrorCode(body)
		}
		failure := balanceFailure{Status: balanceRefreshAuthError, Stage: balanceStageAccountQuota, Code: code}
		return 0, &balanceProbeError{failure: failure, cause: errNewAPIAuthentication}
	}
	quota, ok := numberValue(nestedObject(payload, "data")["quota"])
	if !ok {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountQuota, Code: balanceErrorMissingQuota}
		return 0, &balanceProbeError{failure: failure}
	}
	return quota, nil
}

func cookieHeader(cookies []*http.Cookie) string {
	parts := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie != nil && cookie.Name != "" && cookie.Value != "" && cookie.MaxAge >= 0 {
			parts = append(parts, cookie.Name+"="+cookie.Value)
		}
	}
	return strings.Join(parts, "; ")
}

func (a *application) probeDashboardBalance(ctx context.Context, account accountConfig, checkedAt time.Time) balanceSnapshot {
	result := balanceSnapshot{Status: balanceRefreshError, RefreshStatus: balanceRefreshError, CheckedAt: checkedAt}
	subscriptionURL, err := joinUpstreamURL(account.BaseURL, "/dashboard/billing/subscription", "")
	if err != nil {
		result.ErrorStage = balanceStageDashboardSubscription
		result.ErrorCode = balanceErrorInvalidResponse
		return result
	}
	status, body, err := a.getUpstreamJSON(ctx, subscriptionURL, account.APIKey)
	if err != nil {
		failure := balanceFailureFor(balanceStageDashboardSubscription, status, err)
		result.RefreshStatus = failure.Status
		result.ErrorStage = failure.Stage
		result.ErrorCode = failure.Code
		result.Retryable = failure.Retryable
		return result
	}
	if status == http.StatusNotFound {
		result.Status = balanceRefreshUnsupported
		result.RefreshStatus = balanceRefreshUnsupported
		result.ErrorStage = balanceStageDashboardSubscription
		result.ErrorCode = balanceErrorUnsupported
		return result
	}
	if status < 200 || status >= 300 {
		failure := balanceFailureFor(balanceStageDashboardSubscription, status, nil)
		result.Status = failure.Status
		result.RefreshStatus = failure.Status
		result.ErrorStage = failure.Stage
		result.ErrorCode = failure.Code
		result.Retryable = failure.Retryable
		return result
	}
	subscription, ok := decodeJSONObject(body)
	if !ok {
		result.ErrorStage = balanceStageDashboardSubscription
		result.ErrorCode = balanceErrorInvalidResponse
		result.Retryable = true
		return result
	}
	subscriptionData := nestedObject(subscription, "data")
	unlimited, _ := boolValue(subscriptionData["unlimited_quota"])
	hardLimit, hasHardLimit := firstNumber(subscriptionData, "hard_limit_usd", "system_hard_limit_usd")
	if !hasHardLimit {
		result.ErrorStage = balanceStageDashboardSubscription
		result.ErrorCode = balanceErrorInvalidResponse
		return result
	}
	now := a.now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	end := now.AddDate(0, 0, 1).Format("2006-01-02")
	query := url.Values{"start_date": []string{start}, "end_date": []string{end}}.Encode()
	usageURL, err := joinUpstreamURL(account.BaseURL, "/dashboard/billing/usage", query)
	if err != nil {
		result.ErrorStage = balanceStageDashboardUsage
		result.ErrorCode = balanceErrorInvalidResponse
		return result
	}
	status, body, err = a.getUpstreamJSON(ctx, usageURL, account.APIKey)
	if err != nil {
		failure := balanceFailureFor(balanceStageDashboardUsage, status, err)
		result.RefreshStatus = failure.Status
		result.ErrorStage = failure.Stage
		result.ErrorCode = failure.Code
		result.Retryable = failure.Retryable
		return result
	}
	if status == http.StatusNotFound {
		result.Status = balanceRefreshUnsupported
		result.RefreshStatus = balanceRefreshUnsupported
		result.ErrorStage = balanceStageDashboardUsage
		result.ErrorCode = balanceErrorUnsupported
		return result
	}
	if status < 200 || status >= 300 {
		failure := balanceFailureFor(balanceStageDashboardUsage, status, nil)
		result.Status = failure.Status
		result.RefreshStatus = failure.Status
		result.ErrorStage = failure.Stage
		result.ErrorCode = failure.Code
		result.Retryable = failure.Retryable
		return result
	}
	usage, ok := decodeJSONObject(body)
	if !ok {
		result.ErrorStage = balanceStageDashboardUsage
		result.ErrorCode = balanceErrorInvalidResponse
		result.Retryable = true
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
		result.ErrorStage = balanceStageDashboardUsage
		result.ErrorCode = balanceErrorInvalidResponse
		return result
	}
	result.Status = "ok"
	result.RefreshStatus = balanceRefreshOK
	result.UpdatedAt = checkedAt
	result.Unlimited = unlimited
	result.Scope = balanceScopeTokenOnly
	result.Unit = "USD"
	result.DisplayLabel = "USD"
	result.Amount = hardLimit - totalUsage
	result.hardLimit = hardLimit
	result.hasHardLimit = hasHardLimit
	if result.Amount < 0 {
		result.Amount = 0
	}
	return result
}

func (a *application) getUpstreamJSON(ctx context.Context, target, apiKey string) (int, []byte, error) {
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+apiKey)
	status, body, _, err := a.requestUpstreamJSON(ctx, http.MethodGet, target, nil, headers)
	return status, body, err
}

func (a *application) requestUpstreamJSON(ctx context.Context, method, target string, body []byte, headers http.Header) (int, []byte, []*http.Cookie, error) {
	started := time.Now()
	operation := upstreamOperationForLog(target)
	upstreamRef := upstreamReferenceForLog(target)
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		a.logEvent(context.Background(), slog.LevelWarn, "upstream_json_finished",
			"operation", operation, "upstream_ref", upstreamRef, "status", 0,
			"result", "request_creation_failed", "error_kind", logErrorKind(err),
			"duration_ms", time.Since(started).Milliseconds())
		return 0, nil, nil, err
	}
	request.Header.Set("Accept", "application/json")
	if len(body) != 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	copyHeaders(request.Header, headers)
	response, err := a.client.Do(request)
	if err != nil {
		a.logEvent(context.Background(), slog.LevelWarn, "upstream_json_finished",
			"operation", operation, "upstream_ref", upstreamRef, "status", 0,
			"result", "network_error", "error_kind", logErrorKind(err),
			"duration_ms", time.Since(started).Milliseconds())
		return 0, nil, nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBody+1))
	if err != nil || len(responseBody) > maxErrorBody {
		a.logEvent(context.Background(), slog.LevelWarn, "upstream_json_finished",
			"operation", operation, "upstream_ref", upstreamRef, "status", response.StatusCode,
			"result", "invalid_response", "error_kind", logErrorKind(err),
			"duration_ms", time.Since(started).Milliseconds())
		return response.StatusCode, nil, nil, errors.New("invalid balance response")
	}
	return response.StatusCode, responseBody, response.Cookies(), nil
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

func accessTokenAuthErrorCode(body []byte) string {
	payload, ok := decodeJSONObject(body)
	if !ok {
		return balanceErrorAccessTokenAuth
	}
	message, _ := payload["message"].(string)
	message = strings.ToLower(strings.TrimSpace(message))
	mentionsUserID := strings.Contains(message, "new-api-user") || strings.Contains(message, "user id") ||
		strings.Contains(message, "userid") || strings.Contains(message, "用户 id") || strings.Contains(message, "用户id")
	switch {
	case mentionsUserID && (strings.Contains(message, "not provided") || strings.Contains(message, "missing") ||
		strings.Contains(message, "required") || strings.Contains(message, "未提供") || strings.Contains(message, "不能为空")):
		return balanceErrorUserIDRequired
	case mentionsUserID && (strings.Contains(message, "mismatch") || strings.Contains(message, "not match") ||
		strings.Contains(message, "does not match") || strings.Contains(message, "不匹配") || strings.Contains(message, "不一致")):
		return balanceErrorUserIDMismatch
	default:
		return balanceErrorAccessTokenAuth
	}
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

func redactSecrets(message string, secrets ...string) string {
	sort.SliceStable(secrets, func(left, right int) bool { return len(secrets[left]) > len(secrets[right]) })
	seen := make(map[string]bool, len(secrets))
	for _, secret := range secrets {
		if secret != "" && !seen[secret] {
			message = strings.ReplaceAll(message, secret, "[已隐藏]")
			seen[secret] = true
		}
	}
	return message
}

func (a *application) forwardResponse(
	w http.ResponseWriter,
	response *http.Response,
	requestID uint64,
	accountRef string,
	attempt int,
	started time.Time,
) {
	copyHeaders(w.Header(), response.Header)
	removeHopHeaders(w.Header())
	w.WriteHeader(response.StatusCode)
	buffer := make([]byte, 32<<10)
	written := int64(0)
	terminal := "eof"
	errorKind := "none"
	stream := strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	defer func() {
		level := slog.LevelInfo
		if terminal != "eof" {
			level = slog.LevelWarn
		}
		a.logEvent(context.Background(), level, "proxy_response_finished",
			"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
			"status", response.StatusCode, "stream", stream, "bytes", written,
			"terminal", terminal, "error_kind", errorKind,
			"duration_ms", time.Since(started).Milliseconds())
	}()
	for {
		count, readErr := response.Body.Read(buffer)
		if count > 0 {
			writeCount, writeErr := w.Write(buffer[:count])
			written += int64(writeCount)
			if writeErr != nil || writeCount != count {
				terminal = "downstream_write_error"
				if writeErr != nil {
					errorKind = logErrorKind(writeErr)
				} else {
					errorKind = "short_write"
				}
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				terminal = "upstream_read_error"
				errorKind = logErrorKind(readErr)
			}
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
