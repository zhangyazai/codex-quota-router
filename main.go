package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"
)

const (
	listenAddress      = "127.0.0.1:4000"
	applicationVersion = "0.3.5"
)

type application struct {
	mu                    sync.Mutex
	configMu              sync.Mutex
	requestStatsWriteMu   sync.Mutex
	balanceRefreshGate    chan struct{}
	codexUsageRefreshGate chan struct{}
	cfg                   storedConfig
	configPath            string
	csrfToken             string
	client                *http.Client
	now                   func() time.Time
	logger                *slog.Logger
	persistConfig         func(string, storedConfig) error
	persistRequestStats   func(string, storedRequestStats) error
	balanceTimeout        time.Duration
	balanceRoutingTimeout time.Duration
	proxyBodyBudget       *byteBudget
	codexTraffic          codexTrafficGate
	runtime               map[string]*accountRuntime
	requestStats          map[string]accountRequestStats
	requestStatsPath      string
	requestStatsTimer     *time.Timer
	requestStatsDirty     bool
	requestSequence       uint64
	recovering            map[string]bool
	probeChanged          chan struct{}
	roundRobinCursor      int
	leastUsedBatchAccount string
	leastUsedBatchLeft    int
	lastRoutedAccountID   string
	lastRoutedAccountName string
	gatewayTokenIndex     map[[32]byte]string
	adminSessions         map[[32]byte]time.Time
	loginAttempts         map[string]loginAttempt
	loginGate             chan struct{}
	codexAuth             *codexAuthManager
	codexUsage            *codexUsageCache
	responsesState        *responsesStateCache
}

func main() {
	if err := run(); err != nil {
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
		ReadTimeout:       proxyBodyReadTime,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	logger.Info("service_ready", "accounts", len(app.cfg.Accounts), "strategy", app.cfg.Strategy)
	err = serve(server, listener)
	if statsErr := app.flushRequestStats(); err == nil {
		err = statsErr
	}
	if err == nil {
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
				Proxy:                  http.ProxyFromEnvironment,
				DialContext:            dialer.DialContext,
				ForceAttemptHTTP2:      true,
				MaxIdleConns:           2048,
				MaxIdleConnsPerHost:    512,
				MaxConnsPerHost:        2048,
				IdleConnTimeout:        90 * time.Second,
				TLSHandshakeTimeout:    10 * time.Second,
				ExpectContinueTimeout:  time.Second,
				ResponseHeaderTimeout:  upstreamHeaderTime,
				MaxResponseHeaderBytes: 1 << 20,
				TLSClientConfig: &tls.Config{
					MinVersion: tls.VersionTLS12,
				},
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
	loadedVersion := cfg.Version
	changed := false
	needsInitialGatewayToken := !found
	if !found {
		cfg = storedConfig{Version: configVersion, Strategy: strategyPriority}
		changed = true
	}
	if cfg.Version < configVersion {
		needsInitialGatewayToken = true
		changed = true
	}
	if cfg.Strategy == "" {
		cfg.Strategy = strategyPriority
		changed = true
	}
	if len(cfg.GatewayTokens) == 0 && cfg.GatewayToken != "" {
		cfg.GatewayTokens = []gatewayTokenConfig{{
			ID: "gateway_legacy", Name: "默认 Token", Token: cfg.GatewayToken,
		}}
		cfg.GatewayToken = ""
		changed = true
	}
	if len(cfg.GatewayTokens) == 0 && needsInitialGatewayToken {
		var token gatewayTokenConfig
		token, err = newGatewayTokenConfig("默认 Token", now())
		if err != nil {
			return nil, err
		}
		cfg.GatewayTokens = []gatewayTokenConfig{token}
		changed = true
	}
	if err := normalizeAndValidateConfig(&cfg); err != nil {
		return nil, err
	}
	statsPath := requestStatsPath(path)
	requestStats, err := loadRequestStats(statsPath)
	if err != nil {
		return nil, fmt.Errorf("加载请求统计失败: %w", err)
	}
	if changed {
		if found && loadedVersion == 5 && configVersion == 6 {
			if err := backupConfigVersion(path, loadedVersion); err != nil {
				return nil, fmt.Errorf("备份 v5 配置失败: %w", err)
			}
		}
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
		persistConfig:         saveConfig,
		persistRequestStats:   saveRequestStats,
		balanceTimeout:        balanceRefreshTime,
		balanceRoutingTimeout: balanceRoutingTime,
		proxyBodyBudget:       newByteBudget(maxProxyBody),
		runtime:               make(map[string]*accountRuntime),
		requestStats:          requestStats,
		requestStatsPath:      statsPath,
		balanceRefreshGate:    make(chan struct{}, 1),
		codexUsageRefreshGate: make(chan struct{}, 1),
		recovering:            make(map[string]bool),
		probeChanged:          make(chan struct{}),
		gatewayTokenIndex:     make(map[[32]byte]string),
		adminSessions:         make(map[[32]byte]time.Time),
		loginAttempts:         make(map[string]loginAttempt),
		loginGate:             make(chan struct{}, 4),
		codexAuth:             newCodexAuthManager(client, codexOAuthDefaultSessionLimit),
		codexUsage:            newCodexUsageCache(client, now),
	}
	app.codexAuth.now = now
	app.rebuildGatewayTokenIndexLocked()
	for _, account := range cfg.Accounts {
		app.runtime[account.ID] = &accountRuntime{Revision: account.Revision}
	}
	return app, nil
}
