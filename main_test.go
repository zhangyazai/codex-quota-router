package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	"golang.org/x/net/websocket"
)

func TestHealthAndEmbeddedPageExposeAutoRefreshRelease(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now)
	healthRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/healthz", nil)
	healthRequest.Host = listenAddress
	markLocalRequest(healthRequest)
	healthResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(healthResponse, healthRequest)
	if healthResponse.Code != http.StatusOK ||
		!strings.Contains(healthResponse.Body.String(), `"version":"`+applicationVersion+`"`) {
		t.Fatalf("health status=%d body=%s", healthResponse.Code, healthResponse.Body.String())
	}

	indexRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/", nil)
	indexRequest.Host = listenAddress
	markLocalRequest(indexRequest)
	indexResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(indexResponse, indexRequest)
	body := indexResponse.Body.String()
	csp := indexResponse.Header().Get("Content-Security-Policy")
	const wantCSP = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'; img-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'"
	if indexResponse.Code != http.StatusOK || indexResponse.Header().Get("Cache-Control") != "no-store" ||
		indexResponse.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
		csp != wantCSP ||
		strings.Contains(body, "__CQR_NONCE__") || strings.Contains(body, "<style") ||
		strings.Contains(body, "<script nonce") || strings.Contains(body, " style=") ||
		!strings.Contains(body, `/assets/css/tokens.css`) || !strings.Contains(body, `/assets/css/base.css`) ||
		!strings.Contains(body, `/assets/css/layout.css`) || !strings.Contains(body, `/assets/css/components.css`) ||
		!strings.Contains(body, `/assets/css/features.css`) || !strings.Contains(body, `/assets/js/main.mjs`) ||
		!strings.Contains(body, "可参与路由账号") || !strings.Contains(body, "立即唤醒验证") ||
		!strings.Contains(body, `value="lowest_balance"`) || !strings.Contains(body, "唤醒并等待验证") ||
		!strings.Contains(body, "账面余额") || !strings.Contains(body, "请求统计") ||
		!strings.Contains(body, `data-slot="requests"`) || !strings.Contains(body, `data-slot="successful-requests"`) ||
		!strings.Contains(body, `data-slot="failed-requests"`) || !strings.Contains(body, `data-slot="request-health"`) ||
		!strings.Contains(body, `data-slot="models"`) {
		t.Fatalf("embedded auto-refresh page missing: status=%d", indexResponse.Code)
	}

	var scripts strings.Builder
	err := fs.WalkDir(webAssets, "web/assets/js", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".mjs") {
			return nil
		}
		data, readErr := webAssets.ReadFile(name)
		if readErr != nil {
			return readErr
		}
		_, _ = scripts.Write(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	scriptBody := scripts.String()
	if !strings.Contains(scriptBody, "balancePollInterval = 60000") ||
		!strings.Contains(scriptBody, "?automatic=1") || !strings.Contains(scriptBody, `available: "可参与路由"`) ||
		!strings.Contains(scriptBody, `recent_success: "近期成功"`) || !strings.Contains(scriptBody, `recent_failure: "最近失败"`) ||
		!strings.Contains(scriptBody, `recovery_pending: "待真实请求验证"`) ||
		!strings.Contains(scriptBody, "formatRequestHealth") || !strings.Contains(scriptBody, "/admin/models") ||
		strings.Contains(scriptBody, "testModel") {
		t.Fatal("embedded JavaScript modules missing auto-refresh behavior")
	}
}

func TestEmbeddedWebAssets(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now)
	request := func(method, target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:4000"+target, nil)
		r.Host = listenAddress
		markLocalRequest(r)
		response := httptest.NewRecorder()
		app.routes().ServeHTTP(response, r)
		return response
	}

	css := request(http.MethodGet, "/assets/css/tokens.css")
	if css.Code != http.StatusOK || css.Body.Len() == 0 || css.Header().Get("Content-Type") != "text/css; charset=utf-8" ||
		css.Header().Get("Cache-Control") != "no-store" || css.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("css status=%d headers=%v body=%q", css.Code, css.Header(), css.Body.String())
	}

	module := request(http.MethodGet, "/assets/js/main.mjs")
	if module.Code != http.StatusOK || module.Body.Len() == 0 || module.Header().Get("Content-Type") != "text/javascript; charset=utf-8" ||
		module.Header().Get("Cache-Control") != "no-store" || module.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("module status=%d headers=%v body=%q", module.Code, module.Header(), module.Body.String())
	}

	nestedModule := request(http.MethodGet, "/assets/js/core/api-client.mjs")
	if nestedModule.Code != http.StatusOK || nestedModule.Body.Len() == 0 {
		t.Fatalf("nested module status=%d body=%q", nestedModule.Code, nestedModule.Body.String())
	}

	head := request(http.MethodHead, "/assets/js/main.mjs")
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("module HEAD status=%d headers=%v body=%q", head.Code, head.Header(), head.Body.String())
	}

	post := request(http.MethodPost, "/assets/js/main.mjs")
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" || post.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("module POST status=%d headers=%v", post.Code, post.Header())
	}

	for _, target := range []string{"/assets/", "/assets/css/", "/assets/js/", "/assets/js/main.js"} {
		response := request(http.MethodGet, target)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s status=%d, want 404", target, response.Code)
		}
	}
}

func TestLoadConfigMigratesV1AndHonorsForceBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.dat")
	legacy := legacyStoredConfig{
		Version:   1,
		Primary:   legacyUpstream{BaseURL: "https://primary.invalid/v1", APIKey: "primary-key"},
		Backup:    legacyUpstream{BaseURL: "https://backup.invalid/v1", APIKey: "backup-key"},
		TestModel: "test-model", GatewayToken: "gateway-token", ForceBackup: true,
		PrimaryVerified: true, PrimaryExhausted: true,
	}
	plain, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	protected, err := protectConfig(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, protected, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, found, err := loadConfig(path)
	if err != nil || !found {
		t.Fatalf("loadConfig: found=%v err=%v", found, err)
	}
	if cfg.Version != configVersion || cfg.Strategy != strategyPriority || len(cfg.Accounts) != 2 {
		t.Fatalf("unexpected migration: %#v", cfg)
	}
	if len(cfg.GatewayTokens) != 1 || cfg.GatewayTokens[0].Token != "gateway-token" {
		t.Fatalf("gateway token was not migrated: %#v", cfg.GatewayTokens)
	}
	if cfg.Accounts[0].ID != "backup" || cfg.Accounts[1].ID != "primary" {
		t.Fatalf("ForceBackup order was not migrated: %#v", cfg.Accounts)
	}
	if !cfg.Accounts[0].Enabled || cfg.Accounts[1].Enabled {
		t.Fatalf("ForceBackup availability was not preserved: %#v", cfg.Accounts)
	}
	if !cfg.Accounts[1].Verified || cfg.Accounts[1].BlockedReason != "quota" {
		t.Fatalf("primary state was not migrated: %#v", cfg.Accounts[1])
	}
	app := newTestApplication(t, cfg.Strategy, time.Now, cfg.Accounts...)
	selected, ok := app.selectAccount(context.Background(), map[string]bool{})
	if !ok || selected.ID != "backup" {
		t.Fatalf("ForceBackup selected %q ok=%v", selected.ID, ok)
	}
	app.blockAccount(selected, "quota")
	if selected, ok = app.selectAccount(context.Background(), map[string]bool{}); ok {
		t.Fatalf("ForceBackup fell through to %q", selected.ID)
	}
	reloaded, _, err := loadConfig(path)
	if err != nil || reloaded.Version != configVersion || len(reloaded.Accounts) != 2 {
		t.Fatalf("migrated config was not persisted: %#v err=%v", reloaded, err)
	}
}

func TestNewApplicationMigratesV5ProvidersAndKeepsFirstBackup(t *testing.T) {
	if configVersion != 6 {
		t.Fatalf("configVersion=%d, want 6", configVersion)
	}
	path := filepath.Join(t.TempDir(), "config.dat")
	legacy := storedConfig{
		Version: 5, Strategy: strategyPriority,
		Accounts: []accountConfig{
			{ID: "legacy-key", Name: "Legacy Key", BaseURL: "https://legacy.invalid/v1", APIKey: "legacy-key", Enabled: true, Revision: 1},
			{
				ID: "legacy-new-api", Name: "Legacy New API", BaseURL: "https://new-api.invalid/v1", APIKey: "proxy-key",
				NewAPIAuthMode: newAPIAuthPassword, NewAPIUsername: "user", NewAPISecret: "password",
				Enabled: true, Revision: 1,
			},
			{
				ID: "legacy-new-api-token", Name: "Legacy New API Token", BaseURL: "https://token.invalid/v1", APIKey: "proxy-key",
				NewAPIAuthMode: newAPIAuthAccessToken, NewAPIUserID: 42, NewAPISecret: "access-token",
				Enabled: true, Revision: 1,
			},
		},
	}
	if err := saveProtectedJSON(path, legacy); err != nil {
		t.Fatal(err)
	}
	firstProtected, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newApplication(path, &http.Client{}, func() time.Time {
		return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := app.cfg
	if cfg.Version != 6 || len(cfg.Accounts) != 3 || cfg.Accounts[0].Provider != "auto" ||
		cfg.Accounts[1].Provider != "new_api" || cfg.Accounts[2].Provider != "new_api" || len(cfg.GatewayTokens) != 1 {
		t.Fatalf("unexpected v5 migration: %#v", cfg)
	}
	persisted, found, err := loadConfig(path)
	if err != nil || !found || persisted.Version != 6 || len(persisted.GatewayTokens) != 1 {
		t.Fatalf("migrated config was not persisted: found=%v config=%#v err=%v", found, persisted, err)
	}
	backupPath := path + ".v5.bak"
	firstBackup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstBackup, firstProtected) {
		t.Fatal("v5 backup does not contain the original protected config")
	}

	legacy.Accounts[0].APIKey = "second-key"
	if err := saveProtectedJSON(path, legacy); err != nil {
		t.Fatal(err)
	}
	secondProtected, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(secondProtected, firstProtected) {
		t.Fatal("second v5 fixture did not change")
	}
	secondApp, err := newApplication(path, &http.Client{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if secondApp.cfg.Version != 6 || secondApp.cfg.Accounts[0].APIKey != "second-key" || len(secondApp.cfg.GatewayTokens) != 1 {
		t.Fatalf("second v5 config was not migrated: %#v", secondApp.cfg)
	}
	if reloaded, found, err := loadConfig(path); err != nil || !found || reloaded.Version != 6 {
		t.Fatalf("second migrated config was not persisted: found=%v config=%#v err=%v", found, reloaded, err)
	}
	backupAfterSecondUpgrade, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backupAfterSecondUpgrade, firstProtected) {
		t.Fatal("existing v5 backup was overwritten")
	}
}

func TestNewApplicationStopsWhenV5BackupPathIsNotAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.dat")
	legacy := storedConfig{
		Version: 5, Strategy: strategyPriority,
		Accounts: []accountConfig{{
			ID: "legacy", Name: "Legacy", BaseURL: "https://legacy.invalid/v1", APIKey: "legacy-key",
			Enabled: true, Revision: 1,
		}},
	}
	if err := saveProtectedJSON(path, legacy); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".v5.bak", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := newApplication(path, &http.Client{}, time.Now); err == nil {
		t.Fatal("v5 upgrade succeeded with a non-file backup path")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("v5 config changed after backup failure")
	}
	cfg, found, err := loadConfig(path)
	if err != nil || !found || cfg.Version != 5 {
		t.Fatalf("original v5 config is no longer readable: found=%v config=%#v err=%v", found, cfg, err)
	}
}

func TestNewApplicationStopsWhenExistingV5BackupIsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.dat")
	legacy := storedConfig{
		Version: 5, Strategy: strategyPriority,
		Accounts: []accountConfig{{
			ID: "legacy", Name: "Legacy", BaseURL: "https://legacy.invalid/v1", APIKey: "legacy-key",
			Enabled: true, Revision: 1,
		}},
	}
	if err := saveProtectedJSON(path, legacy); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".v5.bak", []byte("incomplete backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newApplication(path, &http.Client{}, time.Now); err == nil {
		t.Fatal("v5 upgrade succeeded with an invalid existing backup")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("v5 config changed after existing backup validation failed")
	}
}

func TestLoadConfigRejectsFutureVersionWithoutRewriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.dat")
	plain := []byte(`{"version":` + strconv.Itoa(configVersion+1) + `,"accounts":[],"strategy":"priority","futureField":"keep-me"}`)
	protected, err := protectConfig(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, protected, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "高于当前程序支持") {
		t.Fatalf("future config error=%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("future config was rewritten")
	}
}

func TestStrategies(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		strategy string
		setup    func(*application)
		want     []string
	}{
		{name: "priority", strategy: strategyPriority, want: []string{"a", "a"}},
		{name: "round robin", strategy: strategyRoundRobin, want: []string{"a", "b", "a"}},
		{name: "least used", strategy: strategyLeastUsed, want: []string{"a", "a", "a"}},
		{
			name: "highest balance", strategy: strategyHighestBalance,
			setup: func(app *application) {
				app.runtime["a"].Balance = balanceSnapshot{
					Status: "ok", Amount: 10, Unit: "display_unit", UpdatedAt: now,
				}
				app.runtime["b"].Balance = balanceSnapshot{
					Status: "ok", Amount: 20, Unit: "display_unit", UpdatedAt: now,
				}
			},
			want: []string{"b", "b"},
		},
		{
			name: "unlimited wins", strategy: strategyHighestBalance,
			setup: func(app *application) {
				app.runtime["a"].Balance = balanceSnapshot{
					Status: "ok", Unlimited: true, Unit: "display_unit", UpdatedAt: now,
				}
				app.runtime["b"].Balance = balanceSnapshot{
					Status: "ok", Amount: 999, Unit: "display_unit", UpdatedAt: now,
				}
			},
			want: []string{"a"},
		},
		{
			name: "lowest balance", strategy: strategyLowestBalance,
			setup: func(app *application) {
				app.runtime["a"].Balance = balanceSnapshot{
					Status: "ok", Amount: 10, Unit: "display_unit", UpdatedAt: now,
				}
				app.runtime["b"].Balance = balanceSnapshot{
					Status: "ok", Amount: 20, Unit: "display_unit", UpdatedAt: now,
				}
			},
			want: []string{"a", "a"},
		},
		{
			name: "lowest balance ties use order", strategy: strategyLowestBalance,
			setup: func(app *application) {
				app.runtime["a"].Balance = balanceSnapshot{
					Status: "ok", Amount: 10, Unit: "display_unit", UpdatedAt: now,
				}
				app.runtime["b"].Balance = balanceSnapshot{
					Status: "ok", Amount: 10, Unit: "display_unit", UpdatedAt: now,
				}
				app.runtime["a"].AssignedRequests = 99
			},
			want: []string{"a", "a"},
		},
		{
			name: "lowest balance avoids unlimited", strategy: strategyLowestBalance,
			setup: func(app *application) {
				app.runtime["a"].Balance = balanceSnapshot{
					Status: "ok", Unlimited: true, UpdatedAt: now,
				}
				app.runtime["b"].Balance = balanceSnapshot{
					Status: "ok", Amount: 999, Unit: "display_unit", UpdatedAt: now,
				}
			},
			want: []string{"b"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			app := newTestApplication(t, test.strategy, func() time.Time { return now },
				testAccount("a", "https://a.invalid/v1", "key-a"),
				testAccount("b", "https://b.invalid/v1", "key-b"),
			)
			if test.setup != nil {
				test.setup(app)
			}
			for index, wanted := range test.want {
				account, ok := app.selectAccount(context.Background(), map[string]bool{})
				if !ok || account.ID != wanted {
					t.Fatalf("selection %d = %q ok=%v, want %q", index, account.ID, ok, wanted)
				}
			}
		})
	}
}

func TestLeastUsedRoutesInBatchesAndSkipsUnavailableAccount(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyLeastUsed, func() time.Time { return now },
		testAccount("a", "https://a.invalid/v1", "key-a"),
		testAccount("b", "https://b.invalid/v1", "key-b"),
	)
	for request := 0; request < leastUsedBatchSize; request++ {
		account, ok := app.selectAccount(context.Background(), map[string]bool{})
		if !ok || account.ID != "a" {
			t.Fatalf("batch request %d selected %q ok=%v, want a", request+1, account.ID, ok)
		}
	}
	account, ok := app.selectAccount(context.Background(), map[string]bool{})
	if !ok || account.ID != "b" {
		t.Fatalf("next batch selected %q ok=%v, want b", account.ID, ok)
	}
	app.cooldownAccount(account)
	account, ok = app.selectAccount(context.Background(), map[string]bool{})
	if !ok || account.ID != "a" {
		t.Fatalf("unavailable batch account selected %q ok=%v, want a", account.ID, ok)
	}
}

func TestRuntimeHealthStateDoesNotBlockRouting(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	account.Verified = true
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)

	status := app.status()
	if status.AvailableAccounts != 1 || status.Accounts[0].State != "available" ||
		status.Accounts[0].HealthState != accountHealthUnverified || !status.Accounts[0].Verified {
		t.Fatalf("historical verification was shown as current health: %#v", status)
	}
	selected, ok := app.selectAccount(context.Background(), map[string]bool{})
	if !ok || selected.ID != "a" {
		t.Fatalf("unverified account was not routable: selected=%q ok=%v", selected.ID, ok)
	}

	app.markAccountVerified(selected)
	status = app.status()
	if status.Accounts[0].HealthState != accountHealthRecentSuccess {
		t.Fatalf("successful account health=%q, want recent success", status.Accounts[0].HealthState)
	}

	now = now.Add(accountHealthTTL + time.Second)
	if health := app.status().Accounts[0].HealthState; health != accountHealthUnverified {
		t.Fatalf("stale success health=%q, want unverified", health)
	}
	app.markAccountHealthFailure(selected)
	if health := app.status().Accounts[0].HealthState; health != accountHealthRecentFailure {
		t.Fatalf("failed account health=%q, want recent failure", health)
	}
	selected, ok = app.selectAccount(context.Background(), map[string]bool{})
	if !ok || selected.ID != "a" {
		t.Fatalf("failed health prevented recovery routing: selected=%q ok=%v", selected.ID, ok)
	}
}

func TestProxyRequestCounters(t *testing.T) {
	var upstreamStatus atomic.Int32
	upstreamStatus.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(upstreamStatus.Load()))
		_, _ = io.WriteString(w, "response")
	}))
	defer server.Close()

	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", server.URL+"/v1", "key-a"))
	response := proxyRequest(app, `{}`)
	account := app.status().Accounts[0]
	if response.Code != http.StatusOK || account.AssignedRequests != 1 ||
		account.SuccessfulRequests != 1 || account.FailedRequests != 0 {
		t.Fatalf("successful response=%d account=%#v", response.Code, account)
	}

	upstreamStatus.Store(http.StatusInternalServerError)
	response = proxyRequest(app, `{}`)
	account = app.status().Accounts[0]
	if response.Code != http.StatusInternalServerError || account.AssignedRequests != 2 ||
		account.SuccessfulRequests != 1 || account.FailedRequests != 1 {
		t.Fatalf("failed response=%d account=%#v", response.Code, account)
	}
}

func TestLowestBalanceStrategyIsValid(t *testing.T) {
	cfg := storedConfig{Strategy: " LOWEST_BALANCE "}
	if err := normalizeAndValidateConfig(&cfg); err != nil || cfg.Strategy != strategyLowestBalance {
		t.Fatalf("lowest balance strategy rejected: strategy=%q err=%v", cfg.Strategy, err)
	}
}

func TestQuotaResetStrategyPrefersEarliestFixedWindow(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	apiAccount := testAccount("api", "https://api.invalid/v1", "api-key")
	apiAccount.QuotaResetEvery = 2
	apiAccount.QuotaResetUnit = quotaResetHour
	apiAccount.QuotaResetAnchorAt = now.Add(-30 * time.Minute).Format(time.RFC3339)
	oauthAccount := accountConfig{
		ID: "oauth", Name: "OAuth", AuthType: accountAuthCodexOAuth,
		CodexAccessToken: "access", CodexRefreshToken: "refresh", CodexAccountID: "account",
		Enabled: true, Revision: 1,
	}
	withoutWindow := testAccount("plain", "https://plain.invalid/v1", "plain-key")
	app := newTestApplication(t, strategyQuotaReset, func() time.Time { return now },
		apiAccount, oauthAccount, withoutWindow,
	)
	app.codexUsage = nil
	app.runtime["oauth"].CodexUsage = codexUsageSnapshot{
		CheckedAt: now,
		RateLimit: codexUsageRateLimit{
			Allowed: true, AllowedKnown: true,
			PrimaryWindow: codexUsageWindow{Present: true, ResetAt: now.Add(15 * time.Minute)},
		},
	}
	selected, ok := app.selectAccount(context.Background(), map[string]bool{})
	if !ok || selected.ID != "oauth" {
		t.Fatalf("quota reset selected %q ok=%v", selected.ID, ok)
	}
}

func TestQuotaResetValidationRejectsDurationOverflow(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	account := testAccount("api", "https://api.invalid/v1", "api-key")
	account.QuotaResetEvery = 100000
	account.QuotaResetUnit = quotaResetWeek
	account.QuotaResetAnchorAt = now.Format(time.RFC3339)
	cfg := storedConfig{Strategy: strategyQuotaReset, Accounts: []accountConfig{account}}
	if err := normalizeAndValidateConfig(&cfg); err == nil {
		t.Fatal("overflowing quota reset duration was accepted")
	}
}

func TestStatusReportsActualNextProbeAt(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	runtime := app.runtime[account.ID]
	runtime.UpstreamFailures = 3
	runtime.LastFailureAt = now
	runtime.CooldownReason = "upstream_failures"
	runtime.CooldownUntil = now.Add(5 * time.Minute)

	status := app.status().Accounts[0]
	if status.CooldownUntil != now.Add(5*time.Minute).Format(time.RFC3339) ||
		status.NextProbeAt != now.Add(maxSoftProbeDelay).Format(time.RFC3339) {
		t.Fatalf("status did not distinguish cooldown from controlled probe: %#v", status)
	}
}

func TestAPIQuotaCooldownUsesConfiguredResetWhenUpstreamOmitsHint(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	account := testAccount("api", "https://api.invalid/v1", "api-key")
	account.QuotaResetEvery = 2
	account.QuotaResetUnit = quotaResetHour
	account.QuotaResetAnchorAt = now.Add(-30 * time.Minute).Format(time.RFC3339)
	app := newTestApplication(t, strategyQuotaReset, func() time.Time { return now }, account)
	if action := app.blockAccountFor(account, "quota", nil); action != "cooldown" {
		t.Fatalf("block action=%q", action)
	}
	if want := now.Add(90 * time.Minute); !app.runtime[account.ID].CooldownUntil.Equal(want) {
		t.Fatalf("quota cooldown=%s, want %s", app.runtime[account.ID].CooldownUntil, want)
	}
}

func TestQuotaResetStrategyOrdersAutomaticAndManualAccountsTogether(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name           string
		manualResetIn  time.Duration
		automaticReset time.Duration
		want           string
	}{
		{name: "manual reset is earlier", manualResetIn: 15 * time.Minute, automaticReset: 30 * time.Minute, want: "manual"},
		{name: "automatic reset is earlier", manualResetIn: 30 * time.Minute, automaticReset: 15 * time.Minute, want: "automatic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			manual := testAccount("manual", "https://manual.invalid/v1", "manual-key")
			manual.QuotaResetPeriod = quotaResetPeriodCustom
			manual.QuotaResetEvery = 1
			manual.QuotaResetUnit = quotaResetHour
			manual.QuotaResetAnchorAt = now.Add(test.manualResetIn - time.Hour).Format(time.RFC3339)
			automatic := testAccount("automatic", "https://automatic.invalid/v1", "automatic-key")
			automatic.NewAPIAuthMode = newAPIAuthAccessToken
			automatic.NewAPIUserID = 42
			automatic.NewAPISecret = "access-token"

			accounts := []accountConfig{automatic, manual}
			if test.want == automatic.ID {
				accounts = []accountConfig{manual, automatic}
			}
			app := newTestApplication(t, strategyQuotaReset, func() time.Time { return now }, accounts...)
			automaticResetAt := now.Add(test.automaticReset)
			app.runtime[automatic.ID].Balance = balanceSnapshot{
				Status: "ok", Amount: 1, Unit: "USD", Scope: balanceScopeActual,
				UpdatedAt: now, RefreshStatus: balanceRefreshOK,
				Subscription: &subscriptionQuota{
					Total: 1, Remaining: 1, Unit: "USD", BillingPreference: "subscription_first",
					PriorityResetAt: automaticResetAt.Format(time.RFC3339), ResetAt: automaticResetAt.Format(time.RFC3339),
				},
			}

			selected, ok := app.selectAccount(context.Background(), map[string]bool{})
			if !ok || selected.ID != test.want {
				t.Fatalf("selected=%q ok=%v, want %q", selected.ID, ok, test.want)
			}
		})
	}
}

func TestQuotaResetExhaustedAutomaticSubscriptionUsesRecoveryReset(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	recoveryAt := now.Add(2 * time.Hour)
	automatic := testAccount("automatic", "https://automatic.invalid/v1", "automatic-key")
	automatic.NewAPIAuthMode = newAPIAuthAccessToken
	automatic.NewAPIUserID = 42
	automatic.NewAPISecret = "access-token"
	manual := testAccount("manual", "https://manual.invalid/v1", "manual-key")
	manual.QuotaResetPeriod = quotaResetPeriodCustom
	manual.QuotaResetEvery = 1
	manual.QuotaResetUnit = quotaResetHour
	manual.QuotaResetAnchorAt = now.Add(-30 * time.Minute).Format(time.RFC3339)
	app := newTestApplication(t, strategyQuotaReset, func() time.Time { return now }, automatic, manual)
	app.runtime[automatic.ID].Balance = balanceSnapshot{
		Status: "ok", Amount: 0, Unit: "USD", Scope: balanceScopeActual,
		UpdatedAt: now, RefreshStatus: balanceRefreshOK,
		Subscription: &subscriptionQuota{
			Total: 1, Remaining: 0, Unit: "USD", BillingPreference: "subscription_only",
			ResetAt: recoveryAt.Format(time.RFC3339),
		},
	}

	if resetAt := accountPriorityResetAt(automatic, app.runtime[automatic.ID], now); !resetAt.IsZero() {
		t.Fatalf("exhausted subscription priority reset=%s, want none", resetAt)
	}
	if resetAt := accountRecoveryResetAt(automatic, app.runtime[automatic.ID], now); !resetAt.Equal(recoveryAt) {
		t.Fatalf("exhausted subscription recovery reset=%s, want %s", resetAt, recoveryAt)
	}
	selected, ok := app.selectAccount(context.Background(), map[string]bool{})
	if !ok || selected.ID != manual.ID {
		t.Fatalf("exhausted subscription selected=%q ok=%v, want %q", selected.ID, ok, manual.ID)
	}
	if action := app.blockAccountFor(automatic, "quota", nil); action != "cooldown" {
		t.Fatalf("block action=%q", action)
	}
	if cooldownUntil := app.runtime[automatic.ID].CooldownUntil; !cooldownUntil.Equal(recoveryAt) {
		t.Fatalf("quota cooldown=%s, want %s", cooldownUntil, recoveryAt)
	}
}

func TestAutomaticSubscriptionBillingPreferenceResetEligibility(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	resetAt := now.Add(time.Hour)
	account := testAccount("automatic", "https://automatic.invalid/v1", "automatic-key")
	account.NewAPIAuthMode = newAPIAuthAccessToken
	account.NewAPIUserID = 42
	account.NewAPISecret = "access-token"
	for _, test := range []struct {
		preference   string
		wantPriority bool
		wantRecovery bool
	}{
		{preference: "wallet_only"},
		{preference: "wallet_first", wantRecovery: true},
		{preference: "subscription_first", wantPriority: true, wantRecovery: true},
		{preference: "subscription_only", wantPriority: true, wantRecovery: true},
		{preference: "", wantPriority: true, wantRecovery: true},
	} {
		name := test.preference
		if name == "" {
			name = "default subscription first"
		}
		t.Run(name, func(t *testing.T) {
			runtime := &accountRuntime{Balance: balanceSnapshot{
				Status: "ok", Amount: 1, Unit: "USD", Scope: balanceScopeActual,
				UpdatedAt: now, RefreshStatus: balanceRefreshOK,
				Subscription: &subscriptionQuota{
					Total: 1, Remaining: 1, Unit: "USD", BillingPreference: test.preference,
					PriorityResetAt: resetAt.Format(time.RFC3339), ResetAt: resetAt.Format(time.RFC3339),
				},
			}}
			priority := accountPriorityResetAt(account, runtime, now)
			if test.wantPriority {
				if !priority.Equal(resetAt) {
					t.Fatalf("priority reset=%s, want %s", priority, resetAt)
				}
			} else if !priority.IsZero() {
				t.Fatalf("priority reset=%s, want none", priority)
			}
			recovery := accountRecoveryResetAt(account, runtime, now)
			if test.wantRecovery {
				if !recovery.Equal(resetAt) {
					t.Fatalf("recovery reset=%s, want %s", recovery, resetAt)
				}
			} else if !recovery.IsZero() {
				t.Fatalf("recovery reset=%s, want none", recovery)
			}
		})
	}
	runtime := &accountRuntime{Balance: balanceSnapshot{
		Status: "ok", Amount: 1, Unit: "USD", Scope: balanceScopeActual,
		UpdatedAt: now, RefreshStatus: balanceRefreshOK,
		Subscription: &subscriptionQuota{
			Unlimited: true, BillingPreference: "subscription_first",
			PriorityResetAt: resetAt.Format(time.RFC3339), ResetAt: resetAt.Format(time.RFC3339),
		},
	}}
	if priority := accountPriorityResetAt(account, runtime, now); !priority.IsZero() {
		t.Fatalf("unlimited subscription priority reset=%s, want none", priority)
	}
	if recovery := accountRecoveryResetAt(account, runtime, now); !recovery.IsZero() {
		t.Fatalf("unlimited subscription recovery reset=%s, want none", recovery)
	}
}

func TestNewAPISubscriptionBalanceAggregatesResetTimes(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	exhaustedResetAt := now.Add(10 * time.Minute)
	priorityResetAt := now.Add(20 * time.Minute)
	laterResetAt := now.Add(30 * time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/subscription/self" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"success":true,"data":{"billing_preference":"subscription_first","subscriptions":[`+
			`{"subscription":{"amount_total":1000,"amount_used":1000,"next_reset_time":%d,"allow_wallet_overflow":true}},`+
			`{"subscription":{"amount_total":2000,"amount_used":500,"next_reset_time":%d,"allow_wallet_overflow":true}},`+
			`{"subscription":{"amount_total":3000,"amount_used":1000,"next_reset_time":%d,"allow_wallet_overflow":true}},`+
			`{"subscription":{"amount_total":0,"amount_used":0,"next_reset_time":%d,"allow_wallet_overflow":true}}]}}`,
			exhaustedResetAt.Unix(), priorityResetAt.Unix(), laterResetAt.Unix(), now.Add(time.Minute).Unix())
	}))
	defer server.Close()

	account := testAccount("automatic", server.URL+"/v1", "automatic-key")
	account.NewAPIAuthMode = newAPIAuthAccessToken
	account.NewAPIUserID = 42
	account.NewAPISecret = "access-token"
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	subscription, err := app.getNewAPISubscriptionBalance(context.Background(), account, "", account.NewAPIUserID)
	if err != nil {
		t.Fatal(err)
	}
	if !subscription.hasSubscriptions || !subscription.unlimited || subscription.preference != "subscription_first" ||
		subscription.total != 6000 || subscription.quota != 3500 {
		t.Fatalf("unexpected subscription aggregate: %#v", subscription)
	}
	if !subscription.priorityResetAt.Equal(priorityResetAt) {
		t.Fatalf("priority reset=%s, want %s", subscription.priorityResetAt, priorityResetAt)
	}
	if !subscription.resetAt.Equal(exhaustedResetAt) {
		t.Fatalf("recovery reset=%s, want %s", subscription.resetAt, exhaustedResetAt)
	}
}

func TestQuotaResetStrategyRefreshesAutomaticSubscriptionsBeforeSelection(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	refreshedResetAt := now.Add(10 * time.Minute)
	var subscriptionCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/usage/token/":
			_, _ = io.WriteString(w, `{"data":{"total_available":5000000}}`)
		case "/api/user/self":
			_, _ = io.WriteString(w, `{"success":true,"data":{"quota":5000000}}`)
		case "/api/subscription/self":
			subscriptionCalls.Add(1)
			_, _ = fmt.Fprintf(w, `{"success":true,"data":{"billing_preference":"subscription_first","subscriptions":[`+
				`{"subscription":{"amount_total":5000000,"amount_used":1000000,"next_reset_time":%d,"allow_wallet_overflow":false}}]}}`,
				refreshedResetAt.Unix())
		case "/api/status":
			_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	for _, test := range []struct {
		name   string
		cached balanceSnapshot
	}{
		{name: "missing cache"},
		{name: "priority reset reached", cached: balanceSnapshot{
			Status: "ok", Amount: 1, Unit: "USD", Scope: balanceScopeActual,
			UpdatedAt: now, RefreshStatus: balanceRefreshOK,
			Subscription: &subscriptionQuota{
				Total: 1, Remaining: 1, Unit: "USD", BillingPreference: "subscription_first",
				PriorityResetAt: now.Format(time.RFC3339), ResetAt: now.Add(time.Hour).Format(time.RFC3339),
			},
		}},
		{name: "recovery reset reached", cached: balanceSnapshot{
			Status: "ok", Amount: 1, Unit: "USD", Scope: balanceScopeActual,
			UpdatedAt: now, RefreshStatus: balanceRefreshOK,
			Subscription: &subscriptionQuota{
				Total: 1, Remaining: 1, Unit: "USD", BillingPreference: "subscription_first",
				PriorityResetAt: now.Add(20 * time.Minute).Format(time.RFC3339), ResetAt: now.Format(time.RFC3339),
			},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			subscriptionCalls.Store(0)
			manual := testAccount("manual", "https://manual.invalid/v1", "manual-key")
			manual.QuotaResetPeriod = quotaResetPeriodCustom
			manual.QuotaResetEvery = 1
			manual.QuotaResetUnit = quotaResetHour
			manual.QuotaResetAnchorAt = now.Add(-30 * time.Minute).Format(time.RFC3339)
			automatic := testAccount("automatic", server.URL+"/v1", "automatic-key")
			automatic.NewAPIAuthMode = newAPIAuthAccessToken
			automatic.NewAPIUserID = 42
			automatic.NewAPISecret = "access-token"
			app := newTestApplication(t, strategyQuotaReset, func() time.Time { return now }, manual, automatic)
			app.runtime[automatic.ID].Balance = test.cached

			selected, ok := app.selectAccount(context.Background(), map[string]bool{})
			if !ok || selected.ID != automatic.ID || subscriptionCalls.Load() != 1 {
				t.Fatalf("selected=%q ok=%v subscriptionCalls=%d", selected.ID, ok, subscriptionCalls.Load())
			}
			if resetAt := accountPriorityResetAt(automatic, app.runtime[automatic.ID], now); !resetAt.Equal(refreshedResetAt) {
				t.Fatalf("refreshed priority reset=%s, want %s", resetAt, refreshedResetAt)
			}
		})
	}
}

func TestManualQuotaResetPeriodsAndLegacyCompatibility(t *testing.T) {
	location, err := time.LoadLocation(defaultQuotaResetTimezone)
	if err != nil {
		t.Fatal(err)
	}
	calendarNow := time.Date(2026, 7, 14, 12, 34, 0, 0, location)
	monthlyAnchor := time.Date(2026, 1, 31, 9, 30, 0, 0, location)
	monthlyNow := time.Date(2026, 2, 1, 8, 0, 0, 0, location)
	legacyAnchor := calendarNow.Add(-30 * time.Minute)
	for _, test := range []struct {
		name       string
		now        time.Time
		period     string
		timezone   string
		every      int
		unit       string
		anchorAt   string
		wantPeriod string
		want       time.Time
	}{
		{
			name: "daily", now: calendarNow, period: quotaResetPeriodDaily, timezone: defaultQuotaResetTimezone,
			wantPeriod: quotaResetPeriodDaily, want: time.Date(2026, 7, 15, 0, 0, 0, 0, location),
		},
		{
			name: "weekly", now: calendarNow, period: quotaResetPeriodWeekly, timezone: defaultQuotaResetTimezone,
			wantPeriod: quotaResetPeriodWeekly, want: time.Date(2026, 7, 20, 0, 0, 0, 0, location),
		},
		{
			name: "monthly", now: calendarNow, period: quotaResetPeriodMonthly, timezone: defaultQuotaResetTimezone,
			wantPeriod: quotaResetPeriodMonthly, want: time.Date(2026, 8, 1, 0, 0, 0, 0, location),
		},
		{
			name: "custom three months", now: monthlyNow, period: quotaResetPeriodCustom, timezone: defaultQuotaResetTimezone,
			every: 3, unit: quotaResetMonth, anchorAt: monthlyAnchor.Format(time.RFC3339),
			wantPeriod: quotaResetPeriodCustom, want: time.Date(2026, 4, 30, 9, 30, 0, 0, location),
		},
		{
			name: "custom twelve months", now: time.Date(2026, 7, 14, 10, 0, 0, 0, location),
			period: quotaResetPeriodCustom, timezone: defaultQuotaResetTimezone,
			every: 12, unit: quotaResetMonth,
			anchorAt:   time.Date(2025, 7, 14, 9, 30, 0, 0, location).Format(time.RFC3339),
			wantPeriod: quotaResetPeriodCustom, want: time.Date(2027, 7, 14, 9, 30, 0, 0, location),
		},
		{
			name: "legacy every", now: calendarNow, every: 2, unit: quotaResetHour,
			anchorAt: legacyAnchor.Format(time.RFC3339), wantPeriod: quotaResetPeriodCustom,
			want: legacyAnchor.Add(2 * time.Hour),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := testAccount("manual", "https://manual.invalid/v1", "manual-key")
			account.QuotaResetPeriod = test.period
			account.QuotaResetTimezone = test.timezone
			account.QuotaResetEvery = test.every
			account.QuotaResetUnit = test.unit
			account.QuotaResetAnchorAt = test.anchorAt
			cfg := storedConfig{Strategy: strategyQuotaReset, Accounts: []accountConfig{account}}
			if err := normalizeAndValidateConfig(&cfg); err != nil {
				t.Fatal(err)
			}
			normalized := cfg.Accounts[0]
			if normalized.QuotaResetPeriod != test.wantPeriod {
				t.Fatalf("period=%q, want %q", normalized.QuotaResetPeriod, test.wantPeriod)
			}
			if resetAt := manualQuotaResetAt(normalized, test.now); !resetAt.Equal(test.want) {
				t.Fatalf("reset=%s, want %s", resetAt, test.want)
			}
		})
	}
}

func TestCodexOAuthConfigDoesNotExposeTokens(t *testing.T) {
	account := accountConfig{
		ID: "oauth", Name: "OAuth", AuthType: accountAuthCodexOAuth,
		CodexAccessToken: "access-secret", CodexRefreshToken: "refresh-secret",
		CodexIDToken: "id-secret", CodexAccountID: "chatgpt-account",
		CodexEmail: "user@example.com", CodexPlanType: "plus",
		Enabled: true, Revision: 1,
	}
	app := newTestApplication(t, strategyPriority, time.Now, account)
	public := app.publicConfig()
	if len(public.Accounts) != 1 || !public.Accounts[0].CodexAuthenticated ||
		public.Accounts[0].Provider != accountAuthCodexOAuth ||
		public.Accounts[0].CodexEmail != account.CodexEmail {
		t.Fatalf("unexpected public OAuth account: %#v", public.Accounts)
	}
	body, err := json.Marshal(public)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{account.CodexAccessToken, account.CodexRefreshToken, account.CodexIDToken} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("public config leaked OAuth secret %q", secret)
		}
	}
}

func TestAPIKeyProviderValues(t *testing.T) {
	for _, provider := range []string{"auto", "new_api", "sub2api", "openai_responses"} {
		t.Run(provider, func(t *testing.T) {
			cfg := storedConfig{
				Version: configVersion, Strategy: strategyPriority,
				Accounts: []accountConfig{{
					ID: "a", Name: "A", Provider: provider, BaseURL: "https://a.invalid/v1", APIKey: "key-a",
					Enabled: true, Revision: 1,
				}},
			}
			if err := normalizeAndValidateConfig(&cfg); err != nil {
				t.Fatalf("provider %q rejected: %v", provider, err)
			}
			if cfg.Accounts[0].Provider != provider {
				t.Fatalf("provider=%q, want %q", cfg.Accounts[0].Provider, provider)
			}
		})
	}

	cfg := storedConfig{
		Version: configVersion, Strategy: strategyPriority,
		Accounts: []accountConfig{{
			ID: "a", Name: "A", Provider: accountAuthCodexOAuth, BaseURL: "https://a.invalid/v1", APIKey: "key-a",
			Enabled: true, Revision: 1,
		}},
	}
	if err := normalizeAndValidateConfig(&cfg); err == nil {
		t.Fatal("API Key account accepted codex_oauth as its provider")
	}
	oauthProvider := accountAuthCodexOAuth
	oauthInput := accountInput{AuthType: accountAuthCodexOAuth, Provider: &oauthProvider}
	if err := normalizeAccountInput(&oauthInput); err != nil || oauthInput.Provider == nil ||
		*oauthInput.Provider != accountAuthCodexOAuth {
		t.Fatalf("explicit OAuth provider was rejected: provider=%v err=%v", oauthInput.Provider, err)
	}
}

func TestIncompleteCodexOAuthAccountCanBeSavedBeforeLogin(t *testing.T) {
	cfg := storedConfig{
		Strategy: strategyPriority,
		Accounts: []accountConfig{{
			ID: "oauth", Name: "OAuth", AuthType: accountAuthCodexOAuth,
			Enabled: true, Revision: 1,
		}},
	}
	if err := normalizeAndValidateConfig(&cfg); err != nil {
		t.Fatalf("incomplete OAuth account rejected: %v", err)
	}
	if accountConfigured(cfg.Accounts[0]) {
		t.Fatal("OAuth account without credentials became routable")
	}
}

func TestCodexQuotaCooldownUsesLongResetWindow(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	account := accountConfig{
		ID: "oauth", Name: "OAuth", AuthType: accountAuthCodexOAuth,
		CodexAccessToken: "access", CodexRefreshToken: "refresh", CodexAccountID: "account",
		Enabled: true, Revision: 1,
	}
	app := newTestApplication(t, strategyQuotaReset, func() time.Time { return now }, account)
	resetAfter := 7 * 24 * time.Hour
	if action := app.blockAccountFor(account, "quota", &resetAfter); action != "cooldown" {
		t.Fatalf("block action=%q", action)
	}
	runtime := app.runtime["oauth"]
	if runtime.CooldownUntil != now.Add(resetAfter) || !runtime.CodexUsage.Exhausted() ||
		runtime.CodexUsage.LimitResetAt != now.Add(resetAfter) {
		t.Fatalf("unexpected OAuth quota cooldown: %#v", runtime)
	}
	if cached, ok := app.codexUsage.Cached(account.ID); !ok || !cached.Exhausted() {
		t.Fatalf("OAuth quota cooldown was not synchronized to usage cache: ok=%v usage=%#v", ok, cached)
	}
}

func TestDisableCodexCreditsSkipsUnsafeOAuthUsage(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name         string
		disable      bool
		usage        codexUsageSnapshot
		withoutUsage bool
		want         string
	}{
		{
			name: "credits allowed keeps existing behavior",
			usage: codexUsageSnapshot{
				CheckedAt: now, ExpiresAt: now.Add(time.Minute),
				RateLimit: codexUsageRateLimit{
					Allowed: true, AllowedKnown: true,
					PrimaryWindow: codexUsageWindow{Present: true, UsedPercent: 100},
				},
				Credits: codexUsageCredits{Present: true, HasCredits: true, HasCreditsKnown: true},
			},
			want: "oauth",
		},
		{
			name:    "full plan window blocks credits",
			disable: true,
			usage: codexUsageSnapshot{
				CheckedAt: now, ExpiresAt: now.Add(time.Minute),
				RateLimit: codexUsageRateLimit{
					Allowed: true, AllowedKnown: true,
					PrimaryWindow: codexUsageWindow{Present: true, UsedPercent: 100},
				},
				Credits: codexUsageCredits{Present: true, HasCredits: true, HasCreditsKnown: true},
			},
			want: "fallback",
		},
		{name: "unknown usage fails closed", disable: true, withoutUsage: true, want: "fallback"},
	} {
		t.Run(test.name, func(t *testing.T) {
			oauth := testCodexOAuthAccount("oauth", "real-oauth", now)
			oauth.DisableCodexCredits = test.disable
			fallback := testAccount("fallback", "https://fallback.invalid/v1", "fallback-key")
			app := newTestApplication(t, strategyPriority, func() time.Time { return now }, oauth, fallback)
			if test.withoutUsage {
				app.codexUsage = nil
			} else {
				app.runtime[oauth.ID].CodexUsage = test.usage
			}
			selected, ok := app.selectAccount(context.Background(), map[string]bool{})
			if !ok || selected.ID != test.want {
				t.Fatalf("selected=%q ok=%v, want %q", selected.ID, ok, test.want)
			}
		})
	}
}

func TestCodexTrafficGateConcurrencyLimits(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)

	t.Run("per real account", func(t *testing.T) {
		gate := codexTrafficGate{}
		account := testCodexOAuthAccount("a", "real-a", now)
		first, err := gate.acquire(now, account)
		if err != nil {
			t.Fatal(err)
		}
		second, err := gate.acquire(now, account)
		if err != nil {
			t.Fatal(err)
		}
		if release, err := gate.acquire(now.Add(time.Second), account); err == nil {
			release()
			t.Fatal("third concurrent request was admitted")
		}
		first()
		second()
	})

	t.Run("global", func(t *testing.T) {
		gate := codexTrafficGate{}
		releases := make([]func(), 0, codexGlobalConcurrency)
		for index := 0; index < codexGlobalConcurrency; index++ {
			account := testCodexOAuthAccount(fmt.Sprintf("local-%d", index), fmt.Sprintf("real-%d", index), now)
			release, err := gate.acquire(now, account)
			if err != nil {
				t.Fatalf("acquire %d: %v", index, err)
			}
			releases = append(releases, release)
		}
		fifth := testCodexOAuthAccount("local-5", "real-5", now)
		if release, err := gate.acquire(now.Add(time.Second), fifth); err == nil {
			release()
			t.Fatal("fifth global concurrent request was admitted")
		}
		for _, release := range releases {
			release()
		}
	})

	t.Run("shared account id", func(t *testing.T) {
		gate := codexTrafficGate{}
		firstAccount := testCodexOAuthAccount("local-a", "shared-real", now)
		secondAccount := testCodexOAuthAccount("local-b", "shared-real", now)
		first, err := gate.acquire(now, firstAccount)
		if err != nil {
			t.Fatal(err)
		}
		second, err := gate.acquire(now, secondAccount)
		if err != nil {
			t.Fatal(err)
		}
		if release, err := gate.acquire(now.Add(time.Second), firstAccount); err == nil {
			release()
			t.Fatal("local accounts with one Codex account ID did not share the limit")
		}
		first()
		second()
	})

	t.Run("release once", func(t *testing.T) {
		gate := codexTrafficGate{}
		account := testCodexOAuthAccount("a", "real-a", now)
		first, err := gate.acquire(now, account)
		if err != nil {
			t.Fatal(err)
		}
		second, err := gate.acquire(now, account)
		if err != nil {
			t.Fatal(err)
		}
		nowAfterRefill := now.Add(2 * time.Second)
		first()
		first()
		third, err := gate.acquire(nowAfterRefill, account)
		if err != nil {
			t.Fatalf("request after release was rejected: %v", err)
		}
		if release, err := gate.acquire(nowAfterRefill, account); err == nil {
			release()
			t.Fatal("duplicate release decremented another in-flight request")
		}
		second()
		third()
	})
}

func TestCodexTrafficGateTokenBucketsRefill(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)

	t.Run("account", func(t *testing.T) {
		gate := codexTrafficGate{}
		account := testCodexOAuthAccount("a", "real-a", now)
		for index := 0; index < codexAccountBurst; index++ {
			release, err := gate.acquire(now, account)
			if err != nil {
				t.Fatalf("acquire %d: %v", index, err)
			}
			release()
		}
		if release, err := gate.acquire(now, account); err == nil {
			release()
			t.Fatal("account burst was not enforced")
		}
		now = now.Add(time.Second)
		release, err := gate.acquire(now, account)
		if err != nil {
			t.Fatalf("account token did not refill: %v", err)
		}
		release()
	})

	t.Run("global", func(t *testing.T) {
		gate := codexTrafficGate{}
		for index := 0; index < codexGlobalBurst; index++ {
			account := testCodexOAuthAccount(fmt.Sprintf("local-%d", index), fmt.Sprintf("real-%d", index), now)
			release, err := gate.acquire(now, account)
			if err != nil {
				t.Fatalf("acquire %d: %v", index, err)
			}
			release()
		}
		account := testCodexOAuthAccount("after-burst", "after-burst", now)
		if release, err := gate.acquire(now, account); err == nil {
			release()
			t.Fatal("global burst was not enforced")
		}
		now = now.Add(500 * time.Millisecond)
		release, err := gate.acquire(now, account)
		if err != nil {
			t.Fatalf("global token did not refill: %v", err)
		}
		release()
	})
}

func TestCodexTrafficAdmissionLivesUntilStreamBodyCloses(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	account := testCodexOAuthAccount("oauth", "real-account", now)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	var calls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")),
			Request:    request,
		}, nil
	})}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", nil)
	body := []byte(`{"input":"hello","stream":true}`)
	first, err := app.sendUpstream(request.Context(), request, body, account)
	if err != nil {
		t.Fatal(err)
	}
	second, err := app.sendUpstream(request.Context(), request, body, account)
	if err != nil {
		_ = first.Body.Close()
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if response, err := app.sendUpstream(request.Context(), request, body, account); err == nil {
		_ = response.Body.Close()
		_ = first.Body.Close()
		_ = second.Body.Close()
		t.Fatal("third stream was admitted before a body closed")
	}
	if err := first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := app.sendUpstream(request.Context(), request, body, account)
	if err != nil {
		_ = second.Body.Close()
		t.Fatalf("body close did not release admission: %v", err)
	}
	_ = second.Body.Close()
	_ = third.Body.Close()
	if calls.Load() != 3 {
		t.Fatalf("transport calls=%d, want 3", calls.Load())
	}
}

func TestCodexTrafficAdmissionRechecksProviderGate(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	account := testCodexOAuthAccount("oauth", "real-account", now)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	app.codexTraffic.jitter = func(time.Duration) time.Duration { return 0 }
	app.codexTraffic.tripProviderRateLimit(now, nil)
	var calls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("transport must not be called")
	})}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", nil)
	_, err := app.sendUpstream(request.Context(), request, []byte(`{"input":"hello"}`), account)
	var providerGate *codexProviderGateError
	if !errors.As(err, &providerGate) || calls.Load() != 0 {
		t.Fatalf("provider admission err=%v calls=%d", err, calls.Load())
	}
}

func TestCodexProviderRecoveryIncludesAccountCooldown(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	account := testCodexOAuthAccount("oauth", "real-account", now)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	app.codexTraffic.jitter = func(time.Duration) time.Duration { return 0 }
	app.codexTraffic.tripProviderRateLimit(now, nil)
	runtime := app.runtime[account.ID]
	runtime.CooldownReason = "quota"
	runtime.CooldownUntil = now.Add(5 * time.Minute)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", nil)
	supports := func(candidate accountConfig) bool { return accountSupportsProxyRequest(candidate, request) }
	if recoveryAt := app.nextCodexProviderRecoveryAt(supports, now); !recoveryAt.Equal(runtime.CooldownUntil) {
		t.Fatalf("provider recovery=%s, want %s", recoveryAt, runtime.CooldownUntil)
	}
	runtime.ProbeInFlight = true
	if recoveryAt := app.nextCodexProviderRecoveryAt(supports, now); !recoveryAt.IsZero() {
		t.Fatalf("in-flight probe exposed a stale recovery time: %s", recoveryAt)
	}
	runtime.ProbeInFlight = false
	app.cfg.Accounts[0].Enabled = false
	if recoveryAt := app.nextCodexProviderRecoveryAt(supports, now); !recoveryAt.IsZero() {
		t.Fatalf("disabled OAuth account kept a stale provider recovery time: %s", recoveryAt)
	}
}

func TestCodexProviderRateLimitBackoff(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)

	t.Run("sequence", func(t *testing.T) {
		gate := codexTrafficGate{jitter: func(time.Duration) time.Duration { return 0 }}
		for index, want := range []time.Duration{
			10 * time.Second, 30 * time.Second, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute,
		} {
			if until := gate.tripProviderRateLimit(now, nil); !until.Equal(now.Add(want)) {
				t.Fatalf("strike %d recovery=%s, want %s", index+1, until, now.Add(want))
			}
		}
	})

	t.Run("jitter retry after and existing gate", func(t *testing.T) {
		gate := codexTrafficGate{jitter: func(base time.Duration) time.Duration { return base }}
		if until := gate.tripProviderRateLimit(now, nil); !until.Equal(now.Add(12500 * time.Millisecond)) {
			t.Fatalf("jitter recovery=%s", until)
		}
		retryAfter := 45 * time.Minute
		if until := gate.tripProviderRateLimit(now, &retryAfter); !until.Equal(now.Add(retryAfter)) {
			t.Fatalf("Retry-After recovery=%s", until)
		}
		if until := gate.tripProviderRateLimit(now.Add(time.Minute), nil); !until.Equal(now.Add(retryAfter)) {
			t.Fatalf("existing gate was shortened: %s", until)
		}
	})

	t.Run("reset and cap", func(t *testing.T) {
		gate := codexTrafficGate{jitter: func(time.Duration) time.Duration { return 0 }}
		gate.tripProviderRateLimit(now, nil)
		resetNow := now.Add(upstreamFailureReset + time.Second)
		if until := gate.tripProviderRateLimit(resetNow, nil); !until.Equal(resetNow.Add(10 * time.Second)) {
			t.Fatalf("strike did not reset: %s", until)
		}
		gate = codexTrafficGate{jitter: func(time.Duration) time.Duration { return 0 }}
		retryAfter := 60 * 24 * time.Hour
		if until := gate.tripProviderRateLimit(now, &retryAfter); !until.Equal(now.Add(maxRetryAfter)) {
			t.Fatalf("provider gate was not capped: %s", until)
		}
	})
}

func TestCodexSafeRetriesConsumeOneTrafficToken(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	account := testCodexOAuthAccount("oauth", "real-account", now)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	var calls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if calls.Add(1) < safeUpstreamAttempts {
			return nil, &net.OpError{Op: "dial", Err: errors.New("network unreachable")}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")),
			Request:    request,
		}, nil
	})}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", nil)
	body := []byte(`{"input":"hello","stream":true}`)
	first, err := app.sendUpstream(request.Context(), request, body, account)
	if err != nil || calls.Load() != safeUpstreamAttempts {
		t.Fatalf("safe retry err=%v calls=%d", err, calls.Load())
	}
	_ = first.Body.Close()
	second, err := app.sendUpstream(request.Context(), request, body, account)
	if err != nil || calls.Load() != safeUpstreamAttempts+1 {
		t.Fatalf("second admission err=%v calls=%d", err, calls.Load())
	}
	_ = second.Body.Close()
}

func TestCodexProviderUsesChromeTransportAndPreservesInjectedTransport(t *testing.T) {
	if got := utls.HelloChrome_Auto.Version; got != "133" {
		t.Fatalf("Chrome fingerprint version=%q, want 133", got)
	}
	standard := &http.Transport{}
	first := newCodexProvider(&http.Client{Transport: standard})
	second := newCodexProvider(&http.Client{Transport: standard})
	chrome, ok := first.client.Transport.(*http2.Transport)
	if !ok {
		t.Fatalf("standard transport was not replaced: %T", first.client.Transport)
	}
	if chrome.MaxHeaderListSize != 0 {
		t.Fatalf("unexpected HTTP/2 max header list size: %d", chrome.MaxHeaderListSize)
	}
	if first.client.Transport != second.client.Transport {
		t.Fatal("Codex Chrome transport was not reused")
	}
	if first.client.Jar != sharedCodexCloudflareCookieJar || second.client.Jar != sharedCodexCloudflareCookieJar {
		t.Fatal("Codex Cloudflare cookie jar was not shared")
	}

	var captured http.Header
	injected := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		captured = request.Header.Clone()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: {}\n\n")),
			Request:    request,
		}, nil
	})
	provider := newCodexProvider(&http.Client{Transport: injected})
	if _, ok := provider.client.Transport.(roundTripFunc); !ok {
		t.Fatalf("injected transport was replaced: %T", provider.client.Transport)
	}
	original := httptest.NewRequest(http.MethodPost, "http://router.invalid/v1/responses", nil)
	response, err := provider.send(context.Background(), original,
		[]byte(`{"input":"hello","stream":true}`), "local-account", 1, "access-token", "account-id")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	const wantUserAgent = "codex-tui/0.135.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.135.0)"
	if captured.Get("User-Agent") != wantUserAgent || captured.Get("Originator") != "codex-tui" {
		t.Fatalf("Codex identity headers=%v", captured)
	}
	if values := captured.Values("Accept-Encoding"); len(values) != 0 {
		t.Fatalf("unexpected explicit Accept-Encoding: %v", values)
	}
	sessionID := captured.Get("Session-Id")
	compactSessionID := strings.ReplaceAll(sessionID, "-", "")
	decodedSessionID, decodeErr := hex.DecodeString(compactSessionID)
	if len(sessionID) != 36 || sessionID[8] != '-' || sessionID[13] != '-' || sessionID[18] != '-' ||
		sessionID[23] != '-' || decodeErr != nil || len(decodedSessionID) != 16 ||
		decodedSessionID[6]&0xf0 != 0x40 || decodedSessionID[8]&0xc0 != 0x80 {
		t.Fatalf("invalid generated Codex session-id %q", sessionID)
	}
	if captured.Get("X-Client-Request-Id") != sessionID || captured.Get("OpenAI-Beta") != codexOAuthResponsesBeta {
		t.Fatalf("Codex SSE session headers=%v", captured)
	}
	inbound := make(http.Header)
	inbound.Set("session-id", "existing-session")
	if got, err := codexRequestSessionID(inbound); err != nil || got != "existing-session" {
		t.Fatalf("existing Codex session-id=%q err=%v", got, err)
	}
}

func TestCodexWebSocketRetriesThenFallsBackToSSE(t *testing.T) {
	var dialCalls atomic.Int32
	var httpCalls atomic.Int32
	provider := newCodexProvider(&http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		httpCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"http\"}}\n\n",
			)),
			Request: request,
		}, nil
	})})
	provider.websockets = &codexWebSocketPool{}
	provider.websocketDial = func(context.Context, http.Header) (*websocket.Conn, error) {
		dialCalls.Add(1)
		return nil, errors.New("handshake failed")
	}
	original := httptest.NewRequest(http.MethodPost, "http://router.invalid/v1/responses", nil)
	original.Header.Set("session-id", "session-fallback")
	response, err := provider.send(context.Background(), original,
		[]byte(`{"input":"hello","stream":true}`), "local-account", 1, "access-token", "account-id")
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || !strings.Contains(string(body), `"id":"http"`) {
		t.Fatalf("fallback body=%s read=%v close=%v", body, readErr, closeErr)
	}
	if dialCalls.Load() != codexWebSocketHandshakeAttempts || httpCalls.Load() != 1 {
		t.Fatalf("WebSocket dials=%d HTTP calls=%d", dialCalls.Load(), httpCalls.Load())
	}
}

func TestCodexWebSocketRetriesReusesConnectionAndNormalizesCompletion(t *testing.T) {
	var connections atomic.Int32
	var requestCount atomic.Int32
	headers := make(chan http.Header, 1)
	requests := make(chan []byte, 2)
	serverDone := make(chan error, 1)
	allDone := make(chan struct{})
	var finishOnce sync.Once
	finish := func(err error) {
		finishOnce.Do(func() {
			serverDone <- err
			close(allDone)
		})
	}
	server := httptest.NewServer(websocket.Server{
		Handshake: func(_ *websocket.Config, request *http.Request) error {
			connections.Add(1)
			select {
			case headers <- request.Header.Clone():
			default:
			}
			return nil
		},
		Handler: func(connection *websocket.Conn) {
			go func() {
				<-allDone
				_ = connection.SetDeadline(time.Now())
			}()
			for {
				var payload string
				if err := websocket.Message.Receive(connection, &payload); err != nil {
					select {
					case <-allDone:
					default:
						finish(err)
					}
					return
				}
				index := int(requestCount.Add(1))
				if index > 2 {
					finish(errors.New("received more than two WebSocket requests"))
					return
				}
				requests <- []byte(payload)
				eventType := []string{"response.done", "response.incomplete"}[index-1]
				event := fmt.Sprintf(`{"type":%q,"response":{"id":"resp_%d","status":"completed","output":[]}}`, eventType, index)
				if err := websocket.Message.Send(connection, event); err != nil {
					finish(err)
					return
				}
				if index == 2 {
					finish(nil)
					return
				}
			}
		},
	})
	defer server.Close()

	var dialCalls atomic.Int32
	var httpCalls atomic.Int32
	actualDial := testCodexWebSocketDialer(server.URL)
	provider := newCodexProvider(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		httpCalls.Add(1)
		return nil, errors.New("unexpected HTTP fallback")
	})})
	provider.websockets = &codexWebSocketPool{}
	provider.websocketDial = func(ctx context.Context, header http.Header) (*websocket.Conn, error) {
		if dialCalls.Add(1) == 1 {
			return nil, errors.New("first handshake failed")
		}
		return actualDial(ctx, header)
	}

	send := func() string {
		t.Helper()
		original := httptest.NewRequest(http.MethodPost, "http://router.invalid/v1/responses", nil)
		original.Header.Set("session-id", "session-reuse")
		requestCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		response, err := provider.send(requestCtx, original,
			[]byte(`{"input":"hello","stream":true}`), "local-account", 3, "access-token", "account-id")
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		cancel()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read=%v close=%v", readErr, closeErr)
		}
		return string(body)
	}
	first := send()
	second := send()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for WebSocket requests")
	}
	if dialCalls.Load() != 2 || connections.Load() != 1 || httpCalls.Load() != 0 {
		t.Fatalf("dials=%d connections=%d HTTP=%d", dialCalls.Load(), connections.Load(), httpCalls.Load())
	}
	for _, body := range []string{first, second} {
		if !strings.Contains(body, `"type":"response.completed"`) ||
			strings.Contains(body, `"type":"response.done"`) || strings.Contains(body, `"type":"response.incomplete"`) {
			t.Fatalf("completion was not normalized: %s", body)
		}
	}
	captured := <-headers
	if captured.Get("OpenAI-Beta") != codexWebSocketBetaHeader ||
		captured.Get("Session-Id") != "session-reuse" ||
		captured.Get("X-Client-Request-Id") != "session-reuse" ||
		captured.Get("Authorization") != "Bearer access-token" ||
		captured.Get("ChatGPT-Account-ID") != "account-id" ||
		captured.Get("Originator") != codexOAuthOriginator || captured.Get("User-Agent") != codexOAuthUserAgent {
		t.Fatalf("Codex WebSocket headers=%v", captured)
	}
	for index := 0; index < 2; index++ {
		var requestBody map[string]any
		if err := json.Unmarshal(<-requests, &requestBody); err != nil || requestBody["type"] != "response.create" {
			t.Fatalf("WebSocket request=%#v err=%v", requestBody, err)
		}
	}
}

func TestCodexWebSocketDisconnectAfterSendDoesNotFallbackOrReplay(t *testing.T) {
	serverDone := make(chan error, 1)
	server := httptest.NewServer(websocket.Server{Handler: func(connection *websocket.Conn) {
		var payload string
		serverDone <- websocket.Message.Receive(connection, &payload)
	}})
	defer server.Close()

	var httpCalls atomic.Int32
	provider := newCodexProvider(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		httpCalls.Add(1)
		return nil, errors.New("unexpected HTTP fallback")
	})})
	provider.websockets = &codexWebSocketPool{}
	provider.websocketDial = testCodexWebSocketDialer(server.URL)
	original := httptest.NewRequest(http.MethodPost, "http://router.invalid/v1/responses", nil)
	original.Header.Set("session-id", "session-disconnect")
	requestCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := provider.send(requestCtx, original,
		[]byte(`{"input":"hello","stream":true}`), "local-account", 1, "access-token", "account-id")
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for WebSocket request")
	}
	var requestSent *codexWebSocketRequestSentError
	if !errors.As(readErr, &requestSent) || safeToReplayUpstreamError(http.MethodPost, readErr) {
		t.Fatalf("post-send error=%v replayable=%v", readErr, safeToReplayUpstreamError(http.MethodPost, readErr))
	}
	if httpCalls.Load() != 0 {
		t.Fatalf("HTTP fallback calls=%d", httpCalls.Load())
	}
}

func TestCodexWebSocketEventsBecomeSSE(t *testing.T) {
	for _, test := range []struct {
		name     string
		typeName string
		wantType string
		keep     bool
	}{
		{name: "done", typeName: "response.done", wantType: "response.completed", keep: true},
		{name: "incomplete", typeName: "response.incomplete", wantType: "response.completed", keep: true},
		{name: "completed", typeName: "response.completed", wantType: "response.completed", keep: true},
		{name: "error", typeName: "error", wantType: "error"},
		{name: "failed", typeName: "response.failed", wantType: "response.failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			frame, terminal, keep, err := codexWebSocketSSEFrame([]byte(fmt.Sprintf(`{"type":%q}`, test.typeName)))
			if err != nil || !terminal || keep != test.keep || !bytes.HasPrefix(frame, []byte("data: ")) || !bytes.HasSuffix(frame, []byte("\n\n")) {
				t.Fatalf("frame=%q terminal=%v keep=%v err=%v", frame, terminal, keep, err)
			}
			var event map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(bytes.TrimPrefix(frame, []byte("data:"))), &event); err != nil || event["type"] != test.wantType {
				t.Fatalf("event=%#v err=%v", event, err)
			}
		})
	}
	if _, _, _, err := codexWebSocketSSEFrame([]byte("not json")); err == nil {
		t.Fatal("invalid WebSocket JSON was accepted")
	}
}

func testCodexWebSocketDialer(serverURL string) codexWebSocketDialFunc {
	webSocketURL := "ws" + strings.TrimPrefix(serverURL, "http") + codexWebSocketPath
	return func(ctx context.Context, header http.Header) (*websocket.Conn, error) {
		config, err := websocket.NewConfig(webSocketURL, "http://router.invalid/")
		if err != nil {
			return nil, err
		}
		config.Header = header.Clone()
		return config.DialContext(ctx)
	}
}

func TestCodexCloudflareCookieJarFiltersCookies(t *testing.T) {
	jar := mustCodexCloudflareCookieJar()
	chatgptURL, err := url.Parse("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatal(err)
	}
	allowedNames := []string{
		"__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom",
		"_cfuvid", "cf_clearance", "cf_ob_info", "cf_use_ob", "cf_chl_test",
	}
	cookies := make([]*http.Cookie, 0, len(allowedNames)+1)
	for _, name := range allowedNames {
		cookies = append(cookies, &http.Cookie{Name: name, Value: name, Path: "/", Secure: true})
	}
	cookies = append(cookies, &http.Cookie{Name: "chatgpt_session", Value: "secret", Path: "/", Secure: true})
	jar.SetCookies(chatgptURL, cookies)
	jar.inner.SetCookies(chatgptURL, []*http.Cookie{
		{Name: "oai-auth-token", Value: "secret", Path: "/", Secure: true},
	})
	got := make(map[string]string)
	for _, cookie := range jar.Cookies(chatgptURL) {
		got[cookie.Name] = cookie.Value
	}
	if len(got) != len(allowedNames) {
		t.Fatalf("unexpected Codex Cloudflare cookies: %#v", got)
	}
	for _, name := range allowedNames {
		if got[name] != name {
			t.Fatalf("missing Codex Cloudflare cookie %q: %#v", name, got)
		}
	}

	otherURL, err := url.Parse("https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(otherURL, []*http.Cookie{{Name: "__cf_bm", Value: "other", Path: "/", Secure: true}})
	if cookies := jar.inner.Cookies(otherURL); len(cookies) != 0 {
		t.Fatalf("stored Cloudflare cookies for another host: %#v", cookies)
	}
	if cookies := jar.Cookies(otherURL); len(cookies) != 0 {
		t.Fatalf("returned Cloudflare cookies for another host: %#v", cookies)
	}

	insecureJar := mustCodexCloudflareCookieJar()
	insecureURL, err := url.Parse("http://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatal(err)
	}
	insecureJar.SetCookies(insecureURL, []*http.Cookie{{Name: "__cf_bm", Value: "insecure", Path: "/"}})
	if cookies := insecureJar.inner.Cookies(insecureURL); len(cookies) != 0 {
		t.Fatalf("stored Cloudflare cookies over HTTP: %#v", cookies)
	}
	if cookies := insecureJar.Cookies(insecureURL); len(cookies) != 0 {
		t.Fatalf("returned Cloudflare cookies over HTTP: %#v", cookies)
	}
}

func TestCodexHTTPProxyConnectPreservesBufferedTunnelData(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	serverErr := make(chan error, 1)
	go func() {
		request, err := http.ReadRequest(bufio.NewReader(server))
		if err == nil {
			_ = request.Body.Close()
			_, err = io.WriteString(server, "HTTP/1.1 200 Connection Established\r\n\r\nprefetched")
		}
		serverErr <- err
	}()

	connection, err := dialCodexHTTPProxy(context.Background(), "tcp", "chatgpt.com:443",
		&url.URL{Scheme: "http", Host: "proxy.invalid:8080"},
		func(context.Context, string, string) (net.Conn, error) { return client, nil })
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, ok := connection.(*codexBufferedConn); !ok {
		t.Fatalf("CONNECT response did not preserve buffered bytes: %T", connection)
	}
	buffer := make([]byte, len("prefetched"))
	if _, err = io.ReadFull(connection, buffer); err != nil || string(buffer) != "prefetched" {
		t.Fatalf("buffered tunnel data=%q err=%v", buffer, err)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestCodexHTTPProxyConnectHonorsCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := dialCodexHTTPProxy(ctx, "tcp", "chatgpt.com:443",
			&url.URL{Scheme: "http", Host: "proxy.invalid:8080"},
			func(context.Context, string, string) (net.Conn, error) {
				close(started)
				return client, nil
			})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled CONNECT returned no error")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled CONNECT remained blocked")
	}
}

func TestCodexResponsesSanitizerKeepsFirstVersionMinimal(t *testing.T) {
	body := []byte("{\"model\":\"gpt-test\",\"input\":\"hello\",\"stream\":false,\"store\":true," +
		"\"previous_response_id\":\"response-old\",\"temperature\":0.2}")
	cleaned, downstreamStream, err := sanitizeCodexResponsesRequest(body, false)
	if err != nil || downstreamStream {
		t.Fatalf("sanitize stream=%v err=%v", downstreamStream, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(cleaned, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["stream"] != true || payload["store"] != false || payload["previous_response_id"] != nil ||
		payload["temperature"] != nil {
		t.Fatalf("unexpected sanitized payload: %#v", payload)
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("string input was not converted: %#v", payload["input"])
	}
	include, ok := payload["include"].([]any)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("reasoning include missing: %#v", payload["include"])
	}
}

func TestCodexResponseHeadersDoNotForwardCookies(t *testing.T) {
	upstream := make(http.Header)
	upstream.Set("Content-Type", "text/event-stream")
	upstream.Set("OpenAI-Request-ID", "request-id")
	upstream.Set("Set-Cookie", "session=secret")
	upstream.Set("Location", "https://example.invalid/")
	header := codexResponseHeaders(upstream)
	if header.Get("Content-Type") != "text/event-stream" ||
		header.Get("OpenAI-Request-ID") != "request-id" ||
		header.Get("Cache-Control") != "no-store" {
		t.Fatalf("safe response headers missing: %#v", header)
	}
	if header.Get("Set-Cookie") != "" || header.Get("Location") != "" {
		t.Fatalf("unsafe response headers forwarded: %#v", header)
	}
	redacted := redactCodexResponseSecrets(
		[]byte(`{"error":{"message":"token access-secret account account-secret"}}`),
		"access-secret",
		"account-secret",
	)
	if strings.Contains(string(redacted), "access-secret") ||
		strings.Contains(string(redacted), "account-secret") {
		t.Fatalf("OAuth response secret was not redacted: %s", redacted)
	}
}

func TestCodexSSEAggregationHandlesCompletedAndUsageLimit(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	completed, statusCode, retryAfter, err := aggregateCodexSSE(
		[]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n"),
		now,
	)
	if err != nil || statusCode != http.StatusOK || retryAfter != nil ||
		!strings.Contains(string(completed), "\"id\":\"resp_1\"") {
		t.Fatalf("completed status=%d retry=%v body=%s err=%v", statusCode, retryAfter, completed, err)
	}
	limited, statusCode, retryAfter, err := aggregateCodexSSE(
		[]byte("data: {\"type\":\"error\",\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":3600}}\n\n"),
		now,
	)
	if err != nil || statusCode != http.StatusTooManyRequests || retryAfter == nil ||
		*retryAfter != time.Hour || !structuredQuotaError(limited) {
		t.Fatalf("limited status=%d retry=%v body=%s err=%v", statusCode, retryAfter, limited, err)
	}
}

func TestCodexBrowserSessionsAreBoundAndLimited(t *testing.T) {
	ownerA, err := newCodexAdminSessionID("owner-a")
	if err != nil {
		t.Fatal(err)
	}
	ownerB, err := newCodexAdminSessionID("owner-b")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	manager := newCodexAuthManager(&http.Client{}, 1)
	manager.now = func() time.Time { return now }
	const sessionID = "codex_session_a"
	session := &codexBrowserSession{
		owner: ownerA, accountID: "account-a", accountRevision: 7,
		state: "state-a", codeVerifier: "verifier-a",
		expiresAt: now.Add(codexOAuthSessionLifetime), status: codexBrowserPollPending,
	}
	manager.sessions[sessionID] = session
	result, err := manager.pollBrowserLogin(ownerA, sessionID)
	if err != nil || result.Status != codexBrowserPollPending || result.AccountID != "account-a" || result.AccountRevision != 7 {
		t.Fatalf("browser session binding was not preserved: result=%#v err=%v", result, err)
	}
	if _, err := manager.reserveBrowserSession("codex_session_b", &codexBrowserSession{
		owner: ownerB, accountID: "account-b", accountRevision: 1, status: codexBrowserPollPending,
	}); !errors.Is(err, errCodexOAuthSessionLimit) {
		t.Fatalf("session limit error=%v", err)
	}
	if _, err := manager.pollBrowserLogin(ownerB, sessionID); !errors.Is(err, errCodexOAuthSessionNotFound) {
		t.Fatalf("different owner accessed browser session: %v", err)
	}
	manager.expireBrowserSession(sessionID, session)
	if _, err := manager.pollBrowserLogin(ownerA, sessionID); !errors.Is(err, errCodexOAuthSessionNotFound) {
		t.Fatalf("deleted browser session remained accessible: %v", err)
	}
}

func TestCodexBrowserAuthorizationUsesPKCEAndFixedCallback(t *testing.T) {
	verifier, challenge, err := generateCodexPKCE()
	if err != nil {
		t.Fatal(err)
	}
	decodedVerifier, err := base64.RawURLEncoding.DecodeString(verifier)
	if err != nil || len(decodedVerifier) != codexOAuthVerifierRandomBytes {
		t.Fatalf("invalid PKCE verifier: bytes=%d err=%v", len(decodedVerifier), err)
	}
	digest := sha256.Sum256([]byte(verifier))
	if want := base64.RawURLEncoding.EncodeToString(digest[:]); challenge != want {
		t.Fatalf("PKCE challenge=%q, want %q", challenge, want)
	}

	authorizationURL, err := url.Parse(codexBrowserAuthorizationURL("state-value", challenge))
	if err != nil {
		t.Fatal(err)
	}
	if authorizationURL.Scheme != "https" || authorizationURL.Host != "auth.openai.com" ||
		authorizationURL.Path != "/oauth/authorize" || authorizationURL.User != nil || authorizationURL.Fragment != "" {
		t.Fatalf("unexpected authorization URL: %s", authorizationURL)
	}
	query := authorizationURL.Query()
	want := map[string]string{
		"client_id":             codexOAuthClientID,
		"response_type":         "code",
		"redirect_uri":          codexOAuthRedirectURI,
		"state":                 "state-value",
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
	}
	for key, value := range want {
		if got := query.Get(key); got != value {
			t.Fatalf("authorization query %s=%q, want %q", key, got, value)
		}
	}
	if query.Get("code_verifier") != "" {
		t.Fatal("authorization URL exposed the PKCE verifier")
	}
}

func TestOAuthAccountOnlyAcceptsResponsesHTTP(t *testing.T) {
	account := accountConfig{AuthType: accountAuthCodexOAuth}
	tests := []struct {
		method string
		target string
		want   bool
	}{
		{http.MethodPost, "http://router.invalid/v1/responses", true},
		{http.MethodPost, "http://router.invalid/v1/responses/compact", true},
		{http.MethodPost, "http://router.invalid/v1/chat/completions", false},
		{http.MethodGet, "http://router.invalid/v1/responses", false},
		{http.MethodPost, "http://router.invalid/v1/responses?target=other", false},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.target, nil)
		if got := accountSupportsProxyRequest(account, request); got != test.want {
			t.Fatalf("%s %s supported=%v want=%v", test.method, test.target, got, test.want)
		}
	}
	upgrade := httptest.NewRequest(http.MethodPost, "http://router.invalid/v1/responses", nil)
	upgrade.Header.Set("Connection", "Upgrade")
	upgrade.Header.Set("Upgrade", "websocket")
	if accountSupportsProxyRequest(account, upgrade) {
		t.Fatal("OAuth account accepted a WebSocket upgrade")
	}
}

func TestCodexSnippetContainsOnlyRequestedSettings(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now)
	want := "model_provider = \"quota_router\"\n\n" +
		"[model_providers.quota_router]\n" +
		"name = \"quota-router\"\n" +
		"base_url = \"http://127.0.0.1:4000/v1\"\n" +
		"wire_api = \"responses\"\n" +
		"experimental_bearer_token = \"gateway-token\"\n" +
		"requires_openai_auth = true\n"
	if got := app.codexSnippet(); got != want {
		t.Fatalf("Codex snippet = %q, want %q", got, want)
	}
}

func TestAdminCanRotateGatewayToken(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now)
	oldToken := app.cfg.GatewayTokens[0].Token
	response := adminJSON(app, http.MethodPut, "/admin/config", saveRequest{RotateGatewayToken: true}, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK {
		t.Fatalf("rotate token status=%d body=%s", response.Code, response.Body.String())
	}
	if app.cfg.GatewayTokens[0].Token == "" || app.cfg.GatewayTokens[0].Token == oldToken {
		t.Fatalf("gateway token was not rotated: %q", app.cfg.GatewayTokens[0].Token)
	}
	newToken := app.cfg.GatewayTokens[0].Token
	if !strings.Contains(response.Body.String(), newToken) {
		t.Fatal("rotate response did not include the updated Codex snippet")
	}
	saved, found, err := loadConfig(app.configPath)
	if err != nil || !found || len(saved.GatewayTokens) != 1 || saved.GatewayTokens[0].Token != newToken {
		t.Fatalf("rotated token was not persisted: found=%v tokens=%#v err=%v", found, saved.GatewayTokens, err)
	}
	proxyStatus := func(token string) int {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", strings.NewReader(`{}`))
		request.Host = listenAddress
		markLocalRequest(request)
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		result := httptest.NewRecorder()
		app.routes().ServeHTTP(result, request)
		return result.Code
	}
	if status := proxyStatus(oldToken); status != http.StatusUnauthorized {
		t.Fatalf("old gateway token status=%d, want %d", status, http.StatusUnauthorized)
	}
	if status := proxyStatus(newToken); status == http.StatusUnauthorized {
		t.Fatal("new gateway token was rejected")
	}
}

func TestAdminCanRotateNamedGatewayTokenByID(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now)
	defaultToken := app.cfg.GatewayTokens[0].Token
	created := adminJSON(app, http.MethodPut, "/admin/config",
		saveRequest{CreateGatewayTokenName: "第二个 Token"}, "http://127.0.0.1:4000")
	if created.Code != http.StatusOK {
		t.Fatalf("create token status=%d body=%s", created.Code, created.Body.String())
	}
	var createdPayload struct {
		GatewayToken   string `json:"gatewayToken"`
		GatewayTokenID string `json:"gatewayTokenId"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdPayload); err != nil {
		t.Fatal(err)
	}
	rotated := adminJSON(app, http.MethodPut, "/admin/config",
		saveRequest{RotateGatewayTokenID: createdPayload.GatewayTokenID}, "http://127.0.0.1:4000")
	if rotated.Code != http.StatusOK {
		t.Fatalf("rotate token status=%d body=%s", rotated.Code, rotated.Body.String())
	}
	var rotatedPayload struct {
		GatewayToken   string `json:"gatewayToken"`
		GatewayTokenID string `json:"gatewayTokenId"`
		CodexSnippet   string `json:"codexSnippet"`
	}
	if err := json.Unmarshal(rotated.Body.Bytes(), &rotatedPayload); err != nil {
		t.Fatal(err)
	}
	if rotatedPayload.GatewayTokenID != createdPayload.GatewayTokenID || rotatedPayload.GatewayToken == "" ||
		rotatedPayload.GatewayToken == createdPayload.GatewayToken ||
		!strings.Contains(rotatedPayload.CodexSnippet, rotatedPayload.GatewayToken) {
		t.Fatalf("unexpected rotate response: %#v", rotatedPayload)
	}
	if app.cfg.GatewayTokens[0].Token != defaultToken {
		t.Fatal("rotating a named token changed the default token")
	}
	if app.cfg.GatewayTokens[1].ID != createdPayload.GatewayTokenID ||
		app.cfg.GatewayTokens[1].Token != rotatedPayload.GatewayToken {
		t.Fatalf("wrong token was rotated: %#v", app.cfg.GatewayTokens)
	}
	proxyStatus := func(token string) int {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", strings.NewReader(`{}`))
		request.Host = listenAddress
		markLocalRequest(request)
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		app.routes().ServeHTTP(response, request)
		return response.Code
	}
	if status := proxyStatus(createdPayload.GatewayToken); status != http.StatusUnauthorized {
		t.Fatalf("old named token status=%d, want %d", status, http.StatusUnauthorized)
	}
	if status := proxyStatus(rotatedPayload.GatewayToken); status == http.StatusUnauthorized {
		t.Fatal("rotated named token was rejected")
	}
	persisted, found, err := loadConfig(app.configPath)
	if err != nil || !found || len(persisted.GatewayTokens) != 2 ||
		persisted.GatewayTokens[1].Token != rotatedPayload.GatewayToken {
		t.Fatalf("named token rotation was not persisted: found=%v tokens=%#v err=%v",
			found, persisted.GatewayTokens, err)
	}
}

func TestMultipleGatewayTokensCanBeCreatedAndDeleted(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now)
	created := adminJSON(app, http.MethodPut, "/admin/config", saveRequest{CreateGatewayTokenName: "第二个 Token"}, "http://127.0.0.1:4000")
	if created.Code != http.StatusOK {
		t.Fatalf("create token status=%d body=%s", created.Code, created.Body.String())
	}
	var payload struct {
		GatewayToken   string `json:"gatewayToken"`
		GatewayTokenID string `json:"gatewayTokenId"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.GatewayToken == "" || payload.GatewayTokenID == "" || len(app.cfg.GatewayTokens) != 2 {
		t.Fatalf("created token response=%#v config=%#v", payload, app.cfg.GatewayTokens)
	}
	proxyStatus := func(token string) int {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", strings.NewReader(`{}`))
		request.Host = listenAddress
		markLocalRequest(request)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		app.routes().ServeHTTP(response, request)
		return response.Code
	}
	if status := proxyStatus(payload.GatewayToken); status == http.StatusUnauthorized {
		t.Fatal("new token was rejected")
	}
	deleted := adminJSON(app, http.MethodPut, "/admin/config", saveRequest{DeleteGatewayTokenID: payload.GatewayTokenID}, "http://127.0.0.1:4000")
	if deleted.Code != http.StatusOK || len(app.cfg.GatewayTokens) != 1 {
		t.Fatalf("delete token status=%d body=%s tokens=%#v", deleted.Code, deleted.Body.String(), app.cfg.GatewayTokens)
	}
	if status := proxyStatus(payload.GatewayToken); status != http.StatusUnauthorized {
		t.Fatalf("deleted token status=%d, want 401", status)
	}
}

func TestPublicAccessRequiresAllowedHostAndAdminLogin(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now)
	passwordHash, err := hashAdminPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	app.cfg.AllowPublicAccess = true
	app.cfg.PublicBaseURL = "https://router.example.com"
	app.cfg.AllowPublicAdmin = true
	app.cfg.AdminPasswordHash = passwordHash
	app.mu.Unlock()

	publicRequest := func(method, path string, body io.Reader) *http.Request {
		request := httptest.NewRequest(method, "https://router.example.com"+path, body)
		request.Host = "router.example.com"
		request.RemoteAddr = "127.0.0.1:54321"
		request.Header.Set("X-Forwarded-For", "203.0.113.10")
		request.Header.Set("X-Forwarded-Proto", "https")
		return request
	}
	index := httptest.NewRecorder()
	app.routes().ServeHTTP(index, publicRequest(http.MethodGet, "/", nil))
	if index.Code != http.StatusOK || index.Header().Get("Strict-Transport-Security") == "" {
		t.Fatalf("public index status=%d headers=%v", index.Code, index.Header())
	}
	asset := httptest.NewRecorder()
	app.routes().ServeHTTP(asset, publicRequest(http.MethodGet, "/assets/js/main.mjs", nil))
	if asset.Code != http.StatusOK || asset.Header().Get("Strict-Transport-Security") == "" ||
		asset.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("public asset status=%d headers=%v body=%s", asset.Code, asset.Header(), asset.Body.String())
	}
	unauthorized := httptest.NewRecorder()
	app.routes().ServeHTTP(unauthorized, publicRequest(http.MethodGet, "/admin/bootstrap", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("public bootstrap status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}
	loginBody := strings.NewReader(`{"password":"correct horse battery staple"}`)
	loginRequest := publicRequest(http.MethodPost, "/admin/login", loginBody)
	loginRequest.Header.Set("Content-Type", "application/json")
	loginRequest.Header.Set("Origin", "https://router.example.com")
	login := httptest.NewRecorder()
	app.routes().ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) != 1 {
		t.Fatalf("login status=%d cookies=%v body=%s", login.Code, login.Result().Cookies(), login.Body.String())
	}
	if !login.Result().Cookies()[0].Secure {
		t.Fatal("public admin session cookie was not Secure")
	}
	bootstrapRequest := publicRequest(http.MethodGet, "/admin/bootstrap", nil)
	bootstrapRequest.AddCookie(login.Result().Cookies()[0])
	bootstrap := httptest.NewRecorder()
	app.routes().ServeHTTP(bootstrap, bootstrapRequest)
	if bootstrap.Code != http.StatusOK || strings.Contains(bootstrap.Body.String(), "correct horse") {
		t.Fatalf("authenticated bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	proxyRequest := publicRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	proxyRequest.Header.Set("Authorization", "Bearer gateway-token")
	proxy := httptest.NewRecorder()
	app.routes().ServeHTTP(proxy, proxyRequest)
	if proxy.Code == http.StatusForbidden || proxy.Code == http.StatusUnauthorized {
		t.Fatalf("authenticated public proxy status=%d body=%s", proxy.Code, proxy.Body.String())
	}
	unknownHost := publicRequest(http.MethodGet, "/healthz", nil)
	unknownHost.Host = "attacker.example"
	unknown := httptest.NewRecorder()
	app.routes().ServeHTTP(unknown, unknownHost)
	if unknown.Code != http.StatusForbidden {
		t.Fatalf("unknown public host status=%d", unknown.Code)
	}
	missingProtoRequest := publicRequest(http.MethodGet, "/healthz", nil)
	missingProtoRequest.Header.Del("X-Forwarded-Proto")
	missingProto := httptest.NewRecorder()
	app.routes().ServeHTTP(missingProto, missingProtoRequest)
	if missingProto.Code != http.StatusForbidden {
		t.Fatalf("public request without trusted HTTPS forwarding status=%d", missingProto.Code)
	}
	duplicateForwardedRequest := publicRequest(http.MethodGet, "/healthz", nil)
	duplicateForwardedRequest.Header["X-Forwarded-For"] = []string{"203.0.113.10", "198.51.100.20"}
	duplicateForwarded := httptest.NewRecorder()
	app.routes().ServeHTTP(duplicateForwarded, duplicateForwardedRequest)
	if duplicateForwarded.Code != http.StatusForbidden {
		t.Fatalf("public request with duplicate forwarding headers status=%d", duplicateForwarded.Code)
	}

	app.mu.Lock()
	app.cfg.AllowPublicAdmin = false
	app.mu.Unlock()
	for _, target := range []string{"/", "/assets/css/tokens.css", "/assets/js/main.mjs"} {
		hidden := httptest.NewRecorder()
		app.routes().ServeHTTP(hidden, publicRequest(http.MethodGet, target, nil))
		if hidden.Code != http.StatusNotFound {
			t.Errorf("public admin disabled %s status=%d, want 404", target, hidden.Code)
		}
	}
	hiddenPost := httptest.NewRecorder()
	app.routes().ServeHTTP(hiddenPost, publicRequest(http.MethodPost, "/assets/js/main.mjs", nil))
	if hiddenPost.Code != http.StatusNotFound {
		t.Errorf("public admin disabled asset POST status=%d, want 404", hiddenPost.Code)
	}
}

func TestPublicAccessAlsoProtectsLocalAdminWithPassword(t *testing.T) {
	passwordHash, err := hashAdminPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	cfg := storedConfig{AllowPublicAccess: true, PublicBaseURL: "https://router.example.com"}
	if err := normalizeAndValidateConfig(&cfg); err == nil {
		t.Fatal("public access without an administrator password was accepted")
	}
	app := newTestApplication(t, strategyPriority, time.Now)
	app.mu.Lock()
	app.cfg.AllowPublicAccess = true
	app.cfg.PublicBaseURL = "https://router.example.com"
	app.cfg.AdminPasswordHash = passwordHash
	app.mu.Unlock()
	localRequest := func(method, path string, body io.Reader) *http.Request {
		request := httptest.NewRequest(method, "http://127.0.0.1:4000"+path, body)
		request.Host = listenAddress
		markLocalRequest(request)
		return request
	}
	bootstrap := httptest.NewRecorder()
	app.routes().ServeHTTP(bootstrap, localRequest(http.MethodGet, "/admin/bootstrap", nil))
	if bootstrap.Code != http.StatusUnauthorized {
		t.Fatalf("unprotected local bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	loginRequest := localRequest(http.MethodPost, "/admin/login", strings.NewReader(`{"password":"correct horse battery staple"}`))
	loginRequest.Header.Set("Content-Type", "application/json")
	loginRequest.Header.Set("Origin", "http://127.0.0.1:4000")
	login := httptest.NewRecorder()
	app.routes().ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) != 1 || login.Result().Cookies()[0].Secure {
		t.Fatalf("local login status=%d cookies=%v body=%s", login.Code, login.Result().Cookies(), login.Body.String())
	}
	authorizedRequest := localRequest(http.MethodGet, "/admin/bootstrap", nil)
	authorizedRequest.AddCookie(login.Result().Cookies()[0])
	authorized := httptest.NewRecorder()
	app.routes().ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authenticated local bootstrap status=%d body=%s", authorized.Code, authorized.Body.String())
	}
}

func TestLoginClientKeyDoesNotTrustAppendedForwardedChain(t *testing.T) {
	keyFor := func(forwarded string) string {
		request := httptest.NewRequest(http.MethodPost, "https://router.example.com/admin/login", nil)
		request.RemoteAddr = "127.0.0.1:54321"
		request.Header.Set("X-Forwarded-For", forwarded)
		return loginClientKey(request)
	}
	if first, second := keyFor("203.0.113.10, 127.0.0.1"), keyFor("198.51.100.20, 127.0.0.1"); first != second {
		t.Fatalf("spoofed forwarded chains produced different limit keys: %q != %q", first, second)
	}
	if key := keyFor("203.0.113.10"); key != "203.0.113.10" {
		t.Fatalf("single trusted forwarded address key=%q", key)
	}
}

func TestGlobalLoginFailureBucketLimitsSpoofedClientKeys(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now)
	for index := 0; index < maxGlobalLoginFailures; index++ {
		app.recordLoginFailure(fmt.Sprintf("spoofed-%d", index))
	}
	if allowed, _ := app.loginAllowed("new-spoofed-client"); allowed {
		t.Fatal("global login failure bucket did not block new spoofed client keys")
	}
}

func TestPBKDF2SHA256KnownVector(t *testing.T) {
	got := hex.EncodeToString(pbkdf2SHA256([]byte("password"), []byte("salt"), 1, 32))
	const want = "120fb6cffcf8b32c43e7225256c4f837a86548c92ccc35480805987cb70be17b"
	if got != want {
		t.Fatalf("PBKDF2-SHA256 = %s, want %s", got, want)
	}
}

func TestGatewayTokenIndexHandlesLargeTokenSets(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now)
	app.mu.Lock()
	app.cfg.GatewayTokens = make([]gatewayTokenConfig, 10000)
	for index := range app.cfg.GatewayTokens {
		app.cfg.GatewayTokens[index] = gatewayTokenConfig{
			ID: fmt.Sprintf("tok_%d", index), Name: fmt.Sprintf("User %d", index), Token: fmt.Sprintf("secret-%d", index),
		}
	}
	app.rebuildGatewayTokenIndexLocked()
	app.mu.Unlock()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer secret-9999")
	if id, ok := app.gatewayTokenID(request); !ok || id != "tok_9999" {
		t.Fatalf("large token index id=%q ok=%v", id, ok)
	}
}

func TestBalanceStrategiesFallBackToLeastUsed(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyHighestBalance, func() time.Time { return now },
		testAccount("a", "https://a.invalid/v1", "key-a"),
		testAccount("b", "https://b.invalid/v1", "key-b"),
	)
	app.runtime["a"].Balance = balanceSnapshot{Status: "unsupported", UpdatedAt: now}
	app.runtime["b"].Balance = balanceSnapshot{Status: "ok", Amount: 100, Unit: "display_unit", UpdatedAt: now}
	app.runtime["a"].AssignedRequests = 5
	app.runtime["b"].AssignedRequests = 1
	status := app.status()
	if status.EffectiveStrategy != strategyLeastUsed || status.FallbackReason != "balance_unavailable" {
		t.Fatalf("unexpected fallback status: %#v", status)
	}
	account, ok := app.selectAccount(context.Background(), map[string]bool{})
	if !ok || account.ID != "b" {
		t.Fatalf("fallback selected %q ok=%v, want b", account.ID, ok)
	}

	app.runtime["a"].Balance = balanceSnapshot{
		Status: "ok", Amount: 100, Unit: "USD", Scope: balanceScopeTokenOnly, UpdatedAt: now,
	}
	app.runtime["b"].Balance = balanceSnapshot{
		Status: "ok", Amount: 1, Unit: "USD", Scope: balanceScopeActual, UpdatedAt: now,
	}
	status = app.status()
	if status.EffectiveStrategy != strategyLeastUsed || status.FallbackReason != "balance_account_unverified" {
		t.Fatalf("token-only balance did not fall back: %#v", status)
	}

	app.runtime["a"].Balance = balanceSnapshot{Status: "ok", Amount: 100, Unit: "USD", UpdatedAt: now}
	app.runtime["b"].Balance = balanceSnapshot{Status: "ok", Amount: 100, Unit: "CNY", UpdatedAt: now}
	status = app.status()
	if status.EffectiveStrategy != strategyLeastUsed || status.FallbackReason != "balance_unit_mismatch" {
		t.Fatalf("unit mismatch did not fall back: %#v", status)
	}

	app.cfg.Strategy = strategyLowestBalance
	status = app.status()
	if status.EffectiveStrategy != strategyLeastUsed || status.FallbackReason != "balance_unit_mismatch" {
		t.Fatalf("lowest balance unit mismatch did not fall back: %#v", status)
	}
}

func TestQuotaCoolsOneAccountAndTriesNext(t *testing.T) {
	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"insufficient_quota"}}`)
	}))
	defer first.Close()
	var secondCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		_, _ = io.WriteString(w, "second")
	}))
	defer second.Close()

	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", first.URL, "key-a"), testAccount("b", second.URL, "key-b"),
	)
	response := proxyRequest(app, `{"model":"test"}`)
	if response.Code != http.StatusOK || response.Body.String() != "second" {
		t.Fatalf("fallback response: status=%d body=%q", response.Code, response.Body.String())
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("calls: first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
	if app.cfg.Accounts[0].BlockedReason != "quota" {
		t.Fatalf("quota was not persisted in memory: %#v", app.cfg.Accounts[0])
	}
	if runtime := app.runtime["a"]; runtime.CooldownReason != "quota" ||
		runtime.CooldownUntil.Sub(runtime.LastRecoveryAt) != 10*time.Second {
		t.Fatalf("quota cooldown was not started: %#v", runtime)
	}
	persisted, found, err := loadConfig(app.configPath)
	if err != nil || !found || persisted.Accounts[0].BlockedReason != "quota" {
		t.Fatalf("quota was not persisted: found=%v cfg=%#v err=%v", found, persisted, err)
	}
	proxyRequest(app, `{}`)
	if firstCalls.Load() != 1 || secondCalls.Load() != 2 {
		t.Fatalf("cooling account was retried: first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
}

func TestQuotaCooldownProbesAndRecovers(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if firstCalls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":"insufficient_quota"}}`)
			return
		}
		_, _ = io.WriteString(w, "recovered")
	}))
	defer first.Close()
	var secondCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		_, _ = io.WriteString(w, "fallback")
	}))
	defer second.Close()

	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", first.URL, "key-a"), testAccount("b", second.URL, "key-b"))
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusOK || response.Body.String() != "fallback" {
		t.Fatalf("fallback status=%d body=%q", response.Code, response.Body.String())
	}
	now = app.runtime["a"].CooldownUntil
	response = proxyRequest(app, `{}`)
	if response.Code != http.StatusOK || response.Body.String() != "recovered" ||
		firstCalls.Load() != 2 || secondCalls.Load() != 1 || app.cfg.Accounts[0].BlockedReason != "" ||
		app.runtime["a"].CooldownReason != "" || app.runtime["a"].RecoveryFailures != 0 ||
		app.runtime["a"].ProbeInFlight {
		t.Fatalf("account did not recover: status=%d first=%d second=%d account=%#v runtime=%#v body=%q",
			response.Code, firstCalls.Load(), secondCalls.Load(), app.cfg.Accounts[0], app.runtime["a"], response.Body.String())
	}
	persisted, found, err := loadConfig(app.configPath)
	if err != nil || !found || persisted.Accounts[0].BlockedReason != "" {
		t.Fatalf("recovery was not persisted: found=%v cfg=%#v err=%v", found, persisted, err)
	}
}

func TestQuotaBalanceRecoveryWakesSingleProbe(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	account.BlockedReason = "quota"
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	runtime := app.runtime["a"]
	runtime.CooldownReason = "quota"
	runtime.CooldownUntil = now.Add(30 * time.Minute)
	runtime.RecoveryFailures = 5
	runtime.LastRecoveryAt = now

	_, applied := app.applyBalance(account, balanceSnapshot{
		Status: "ok", Amount: 1, Unit: "USD", Scope: balanceScopeActual, RefreshStatus: balanceRefreshOK,
		CheckedAt: now, UpdatedAt: now,
	})
	if !applied || !runtime.CooldownUntil.Equal(now) || app.cfg.Accounts[0].BlockedReason != "quota" {
		t.Fatalf("positive balance did not wake quota probe: applied=%v runtime=%#v", applied, runtime)
	}
	selected, ok, probe := app.selectProxyAccount(context.Background(), map[string]bool{})
	if !ok || !probe || selected.ID != "a" || !runtime.ProbeInFlight {
		t.Fatalf("woken account was not selected as probe: selected=%#v ok=%v probe=%v runtime=%#v",
			selected, ok, probe, runtime)
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if selected, ok, probe = app.selectProxyAccount(waitCtx, map[string]bool{}); ok || probe {
		t.Fatalf("second concurrent probe was selected: selected=%#v ok=%v probe=%v", selected, ok, probe)
	}
	app.finishAccountProbe(account, false)
}

func TestConcurrentRequestsWaitForQuotaRecoveryProbe(t *testing.T) {
	now := time.Date(2026, 7, 13, 8, 0, 0, 0, time.UTC)
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	account.BlockedReason = "quota"
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	runtime := app.runtime[account.ID]
	runtime.CooldownReason = "quota"
	runtime.CooldownUntil = now
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if call == 1 {
			close(started)
			<-release
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("response-" + strconv.Itoa(int(call)))), Request: r}, nil
	})}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- proxyRequest(app, `{}`) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("quota recovery probe did not start")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	selected, ok, probe := app.selectProxyAccount(waitCtx, map[string]bool{})
	waitErr := waitCtx.Err()
	cancel()
	if ok || probe || !errors.Is(waitErr, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("concurrent quota selection did not wait for the active probe: selected=%#v ok=%v probe=%v err=%v calls=%d",
			selected, ok, probe, waitErr, calls.Load())
	}
	go func() { secondDone <- proxyRequest(app, `{}`) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		app.mu.Lock()
		sequence := app.requestSequence
		app.mu.Unlock()
		if sequence >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second request did not enter the router")
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	for index, done := range []<-chan *httptest.ResponseRecorder{firstDone, secondDone} {
		select {
		case response := <-done:
			if response.Code != http.StatusOK {
				t.Fatalf("request %d status=%d body=%s", index+1, response.Code, response.Body.String())
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("request %d did not finish", index+1)
		}
	}
	if calls.Load() != 2 || app.cfg.Accounts[0].BlockedReason != "" || runtime.ProbeInFlight {
		t.Fatalf("quota recovery did not release waiting traffic: calls=%d account=%#v runtime=%#v",
			calls.Load(), app.cfg.Accounts[0], runtime)
	}
}

func TestBalanceRecoveryRequiresAccountEvidence(t *testing.T) {
	for _, test := range []struct {
		name    string
		balance balanceSnapshot
		want    bool
	}{
		{name: "actual balance", balance: balanceSnapshot{Status: "ok", Amount: 1, Scope: balanceScopeActual, RefreshStatus: balanceRefreshOK}, want: true},
		{name: "account-only partial", balance: balanceSnapshot{Status: "ok", Amount: 1, Scope: balanceScopeAccountOnly, RefreshStatus: balanceRefreshPartial}, want: true},
		{name: "token-only finite", balance: balanceSnapshot{Status: "ok", Amount: 1, Scope: balanceScopeTokenOnly, RefreshStatus: balanceRefreshOK}},
		{name: "token-only unlimited", balance: balanceSnapshot{Status: "ok", Unlimited: true, Scope: balanceScopeTokenOnly, RefreshStatus: balanceRefreshOK}},
		{name: "zero actual balance", balance: balanceSnapshot{Status: "ok", Scope: balanceScopeActual, RefreshStatus: balanceRefreshOK}},
		{name: "failed refresh", balance: balanceSnapshot{Status: balanceRefreshError, Amount: 1, Scope: balanceScopeActual, RefreshStatus: balanceRefreshError}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := balanceConfirmsRecovery(test.balance); got != test.want {
				t.Fatalf("balanceConfirmsRecovery(%#v)=%v, want %v", test.balance, got, test.want)
			}
		})
	}
}

func TestExplicitAccountRestrictionBlocksButModelMismatchRetries(t *testing.T) {
	t.Run("explicit restriction", func(t *testing.T) {
		var secondCalls atomic.Int32
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"account_suspended"}}`)
		}))
		defer first.Close()
		second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			secondCalls.Add(1)
			_, _ = io.WriteString(w, "ok")
		}))
		defer second.Close()
		app := newTestApplication(t, strategyPriority, time.Now,
			testAccount("a", first.URL, "key-a"), testAccount("b", second.URL, "key-b"))
		response := proxyRequest(app, `{}`)
		if response.Code != http.StatusOK || secondCalls.Load() != 1 || app.cfg.Accounts[0].BlockedReason != "restricted" {
			t.Fatalf("restriction was not isolated: status=%d second=%d account=%#v",
				response.Code, secondCalls.Load(), app.cfg.Accounts[0])
		}
		restarted, err := newApplication(app.configPath, app.client, time.Now)
		if err != nil || restarted.cfg.Accounts[0].BlockedReason != "restricted" {
			t.Fatalf("restriction did not survive restart: app=%#v err=%v", restarted, err)
		}
	})

	t.Run("model access mismatch", func(t *testing.T) {
		var secondCalls atomic.Int32
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":"model_access_denied"}}`)
		}))
		defer first.Close()
		second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			secondCalls.Add(1)
			_, _ = io.WriteString(w, "ok")
		}))
		defer second.Close()
		app := newTestApplication(t, strategyPriority, time.Now,
			testAccount("a", first.URL, "key-a"), testAccount("b", second.URL, "key-b"))
		response := proxyRequest(app, `{}`)
		if response.Code != http.StatusOK || response.Body.String() != "ok" || secondCalls.Load() != 1 ||
			app.cfg.Accounts[0].BlockedReason != "" || app.runtime["a"].CooldownReason != "" {
			t.Fatalf("model mismatch changed account state: status=%d second=%d account=%#v runtime=%#v body=%q",
				response.Code, secondCalls.Load(), app.cfg.Accounts[0], app.runtime["a"], response.Body.String())
		}
	})
}

func TestRecoveryProbeModelMismatchKeepsBlockAndTriesNextAccount(t *testing.T) {
	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"model gpt-image-2 is only supported on /v1/images/generations and /v1/images/edits"}}`)
	}))
	defer first.Close()
	var secondCalls atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		_, _ = io.WriteString(w, "ok")
	}))
	defer second.Close()

	account := testAccount("a", first.URL, "key-a")
	account.BlockedReason = "quota"
	app := newTestApplication(t, strategyPriority, time.Now,
		account, testAccount("b", second.URL, "key-b"))
	response := proxyRequest(app, `{"model":"gpt-image-2"}`)
	runtime := app.runtime["a"]
	if response.Code != http.StatusOK || response.Body.String() != "ok" || firstCalls.Load() != 1 || secondCalls.Load() != 1 ||
		app.cfg.Accounts[0].BlockedReason != "quota" || runtime.UpstreamFailures != 0 ||
		app.status().Accounts[0].HealthState != accountHealthUnverified || runtime.ProbeInFlight {
		t.Fatalf("model mismatch mishandled recovery: status=%d first=%d second=%d account=%#v runtime=%#v body=%q",
			response.Code, firstCalls.Load(), secondCalls.Load(), app.cfg.Accounts[0], runtime, response.Body.String())
	}
}

func TestVerifiedAndUnverified401Handling(t *testing.T) {
	for _, test := range []struct {
		name        string
		verified    bool
		wantBlock   string
		wantCooling bool
	}{
		{name: "verified cools", verified: true, wantBlock: "unauthorized", wantCooling: true},
		{name: "unverified cools", verified: false, wantBlock: "unauthorized", wantCooling: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"invalid token"}}`)
			}))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "ok")
			}))
			defer second.Close()
			account := testAccount("a", first.URL, "key-a")
			account.Verified = test.verified
			now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
			app := newTestApplication(t, strategyPriority, func() time.Time { return now },
				account, testAccount("b", second.URL, "key-b"),
			)
			response := proxyRequest(app, `{}`)
			if response.Code != http.StatusOK || response.Body.String() != "ok" {
				t.Fatalf("401 did not try next: status=%d body=%q", response.Code, response.Body.String())
			}
			if app.cfg.Accounts[0].BlockedReason != test.wantBlock {
				t.Fatalf("blocked reason = %q, want %q", app.cfg.Accounts[0].BlockedReason, test.wantBlock)
			}
			cooling := now.Before(app.runtime["a"].CooldownUntil)
			if cooling != test.wantCooling {
				t.Fatalf("cooling=%v, want %v", cooling, test.wantCooling)
			}
			now = app.runtime["a"].CooldownUntil
			response = proxyRequest(app, `{}`)
			if response.Code != http.StatusOK || app.cfg.Accounts[0].BlockedReason != "unauthorized" ||
				app.runtime["a"].RecoveryFailures != 2 {
				t.Fatalf("second 401 did not extend cooldown: status=%d account=%#v runtime=%#v",
					response.Code, app.cfg.Accounts[0], app.runtime["a"])
			}
		})
	}
}

func TestUnauthorizedBackoffResetsAfterIdle(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://a.invalid/v1", "key-a"))
	if action := app.handleAccountUnauthorized(app.cfg.Accounts[0]); action != "cooldown" {
		t.Fatalf("first 401 action=%q", action)
	}
	now = now.Add(upstreamFailureReset + time.Second)
	if action := app.handleAccountUnauthorized(app.cfg.Accounts[0]); action != "cooldown" ||
		app.cfg.Accounts[0].BlockedReason != "unauthorized" || app.runtime["a"].RecoveryFailures != 1 {
		t.Fatalf("expired 401 strike action=%q account=%#v", action, app.cfg.Accounts[0])
	}
}

func TestConcurrentUnauthorizedOnlyCountsOnce(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	startedAt := now
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://a.invalid/v1", "key-a"))
	if action := app.handleAccountUnauthorized(app.cfg.Accounts[0], startedAt); action != "cooldown" {
		t.Fatalf("first concurrent 401 action=%q", action)
	}
	now = now.Add(accountCooldown + time.Second)
	if action := app.handleAccountUnauthorized(app.cfg.Accounts[0], startedAt); action != "ignored" ||
		app.cfg.Accounts[0].BlockedReason != "unauthorized" {
		t.Fatalf("stale concurrent 401 action=%q account=%#v", action, app.cfg.Accounts[0])
	}
}

func TestConcurrentAccountResultsKeepBlocksAndIgnoreStaleFailures(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://a.invalid/v1", "key-a"),
	)
	selectedBeforeBlock := app.cfg.Accounts[0]
	app.blockAccount(selectedBeforeBlock, "quota")
	app.markAccountVerified(selectedBeforeBlock)
	if !app.cfg.Accounts[0].Verified || app.cfg.Accounts[0].BlockedReason != "quota" {
		t.Fatalf("late success cleared block: %#v", app.cfg.Accounts[0])
	}

	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app = newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://a.invalid/v1", "key-a"),
	)
	selectedWhileUnverified := app.cfg.Accounts[0]
	startedAt := now
	now = now.Add(time.Second)
	app.markAccountVerified(selectedWhileUnverified)
	now = now.Add(time.Second)
	if action := app.handleAccountUnauthorized(selectedWhileUnverified, startedAt); action != "ignored" ||
		app.cfg.Accounts[0].BlockedReason != "" || !app.runtime["a"].CooldownUntil.IsZero() {
		t.Fatalf("stale 401 changed successful account: action=%q account=%#v runtime=%#v",
			action, app.cfg.Accounts[0], app.runtime["a"])
	}
}

func TestOrdinary429CoolsAndNoNextForwardsLastError(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "90")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
	}))
	defer upstream.Close()
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", upstream.URL, "key-a"),
	)
	app.runtime["a"].UpstreamFailures = 2
	app.runtime["a"].LastFailureAt = now.Add(-time.Second)
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("last error was not forwarded: status=%d body=%q", response.Code, response.Body.String())
	}
	if !app.runtime["a"].CooldownUntil.Equal(now.Add(90*time.Second)) ||
		app.runtime["a"].CooldownReason != "rate_limit" || app.runtime["a"].UpstreamFailures != 0 ||
		app.cfg.Accounts[0].BlockedReason != "" {
		t.Fatalf("ordinary 429 state is wrong: runtime=%#v account=%#v", app.runtime["a"], app.cfg.Accounts[0])
	}
	second := proxyRequest(app, `{}`)
	var cooling struct {
		Error struct {
			RetryAfterSeconds int64  `json:"retryAfterSeconds"`
			RetryAt           string `json:"retryAt"`
		} `json:"error"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &cooling); err != nil {
		t.Fatalf("decode cooldown response: %v body=%s", err, second.Body.String())
	}
	wantRetryAt := now.Add(90 * time.Second).Format(time.RFC3339)
	status := app.status().Accounts[0]
	if second.Code != http.StatusServiceUnavailable || second.Header().Get("Retry-After") != "90" ||
		cooling.Error.RetryAfterSeconds != 90 || cooling.Error.RetryAt != wantRetryAt ||
		status.NextProbeAt != wantRetryAt || calls.Load() != 1 {
		t.Fatalf("cooldown response is incomplete: status=%d headers=%v payload=%#v account=%#v calls=%d",
			second.Code, second.Header(), cooling, status, calls.Load())
	}
	now = now.Add(91 * time.Second)
	proxyRequest(app, `{}`)
	if calls.Load() != 2 {
		t.Fatalf("account was not retried after cooldown: calls=%d", calls.Load())
	}
}

func Test402And429DoNotBlockOnBodyBeforeSwitching(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		wantBlock  string
		wantCool   bool
	}{
		{name: "payment required", statusCode: http.StatusPaymentRequired, wantBlock: "quota", wantCool: true},
		{name: "rate limited", statusCode: http.StatusTooManyRequests, wantCool: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			blockedBody, unblockBody := io.Pipe()
			defer unblockBody.Close()
			var firstCalls atomic.Int32
			var secondCalls atomic.Int32
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "first.invalid" {
					firstCalls.Add(1)
					return &http.Response{
						StatusCode: test.statusCode, Header: make(http.Header), Body: blockedBody, Request: r,
					}, nil
				}
				secondCalls.Add(1)
				return &http.Response{
					StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader("second")), Request: r,
				}, nil
			})
			now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
			app := newTestApplication(t, strategyPriority, func() time.Time { return now },
				testAccount("a", "https://first.invalid/v1", "key-a"),
				testAccount("b", "https://second.invalid/v1", "key-b"),
			)
			app.client = &http.Client{Transport: transport}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- proxyRequest(app, `{}`) }()
			var response *httptest.ResponseRecorder
			select {
			case response = <-done:
			case <-time.After(2 * time.Second):
				_ = unblockBody.Close()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
				}
				t.Fatal("error response body blocked account switching")
			}
			if response.Code != http.StatusOK || response.Body.String() != "second" ||
				firstCalls.Load() != 1 || secondCalls.Load() != 1 {
				t.Fatalf("response=%d body=%q first=%d second=%d",
					response.Code, response.Body.String(), firstCalls.Load(), secondCalls.Load())
			}
			if app.cfg.Accounts[0].BlockedReason != test.wantBlock {
				t.Fatalf("blocked reason=%q, want %q", app.cfg.Accounts[0].BlockedReason, test.wantBlock)
			}
			if cooling := now.Before(app.runtime["a"].CooldownUntil); cooling != test.wantCool {
				t.Fatalf("cooling=%v, want %v", cooling, test.wantCool)
			}
		})
	}
}

func TestRateLimitDoesNotRetrySameAccountPerRequest(t *testing.T) {
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","account":"first"}}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","account":"second"}}`)
	}))
	defer second.Close()
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", first.URL, "key-a"), testAccount("b", second.URL, "key-b"),
	)
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), `"account":"second"`) {
		t.Fatalf("last upstream error was not returned: status=%d body=%s", response.Code, response.Body.String())
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("an account was retried: first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
}

func TestCodexLocalTrafficLimitDoesNotFailOrSwitchAccounts(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	first := testCodexOAuthAccount("first", "real-first", now)
	second := testCodexOAuthAccount("second", "real-second", now)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, first, second)
	var calls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("transport must not be called")
	})}
	releaseFirst, err := app.codexTraffic.acquire(now, first)
	if err != nil {
		t.Fatal(err)
	}
	releaseSecond, err := app.codexTraffic.acquire(now, first)
	if err != nil {
		releaseFirst()
		t.Fatal(err)
	}
	defer releaseFirst()
	defer releaseSecond()

	now = now.Add(time.Second)
	response := proxyRequest(app, `{"input":"hello"}`)
	retryAfter, retryErr := strconv.Atoi(response.Header().Get("Retry-After"))
	statuses := app.status().Accounts
	if response.Code != http.StatusTooManyRequests || retryErr != nil || retryAfter < 1 ||
		!strings.Contains(response.Body.String(), "codex_router_rate_limited") || calls.Load() != 0 {
		t.Fatalf("local limit response=%d headers=%v calls=%d body=%s",
			response.Code, response.Header(), calls.Load(), response.Body.String())
	}
	if len(statuses) != 2 || statuses[0].FailedRequests != 0 || statuses[0].SuccessfulRequests != 0 ||
		statuses[0].UpstreamFailures != 0 || statuses[0].CooldownReason != "" ||
		statuses[0].HealthState != accountHealthUnverified || statuses[1].AssignedRequests != 0 ||
		app.lastRoutedAccountID != first.ID {
		t.Fatalf("local limit changed account state: statuses=%#v last=%q", statuses, app.lastRoutedAccountID)
	}
}

func TestCodexProviderRateLimitSkipsOAuthAccountsAndReturnsGate(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	first := testCodexOAuthAccount("first", "real-first", now)
	second := testCodexOAuthAccount("second", "real-second", now)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, first, second)
	app.codexTraffic.jitter = func(time.Duration) time.Duration { return 0 }
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.Header.Get("ChatGPT-Account-ID") {
		case first.CodexAccountID:
			firstCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded"}}`)),
				Request:    request,
			}, nil
		case second.CodexAccountID:
			secondCalls.Add(1)
			return nil, errors.New("second OAuth account must be skipped")
		default:
			return nil, fmt.Errorf("unexpected Codex account %q", request.Header.Get("ChatGPT-Account-ID"))
		}
	})}

	response := proxyRequest(app, `{"input":"hello"}`)
	if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "10" ||
		firstCalls.Load() != 1 || secondCalls.Load() != 0 ||
		!strings.Contains(response.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("first response=%d headers=%v first=%d second=%d body=%s",
			response.Code, response.Header(), firstCalls.Load(), secondCalls.Load(), response.Body.String())
	}
	blocked := proxyRequest(app, `{"input":"hello"}`)
	if blocked.Code != http.StatusServiceUnavailable || blocked.Header().Get("Retry-After") != "10" ||
		firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("provider gate response=%d headers=%v first=%d second=%d body=%s",
			blocked.Code, blocked.Header(), firstCalls.Load(), secondCalls.Load(), blocked.Body.String())
	}
}

func TestCodexProviderRateLimitStillAllowsAPIKeyFallback(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	first := testCodexOAuthAccount("first", "real-first", now)
	second := testCodexOAuthAccount("second", "real-second", now)
	fallback := testAccount("fallback", "https://fallback.invalid/v1", "key-fallback")
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, first, second, fallback)
	app.codexTraffic.jitter = func(time.Duration) time.Duration { return 0 }
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	var fallbackCalls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "fallback.invalid" {
			fallbackCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("fallback")),
				Request:    request,
			}, nil
		}
		switch request.Header.Get("ChatGPT-Account-ID") {
		case first.CodexAccountID:
			firstCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"rate_limit_exceeded"}}`)),
				Request:    request,
			}, nil
		case second.CodexAccountID:
			secondCalls.Add(1)
			return nil, errors.New("second OAuth account must be skipped")
		default:
			return nil, fmt.Errorf("unexpected upstream %s", request.URL)
		}
	})}

	response := proxyRequest(app, `{"input":"hello"}`)
	if response.Code != http.StatusOK || response.Body.String() != "fallback" ||
		firstCalls.Load() != 1 || secondCalls.Load() != 0 || fallbackCalls.Load() != 1 {
		t.Fatalf("fallback response=%d first=%d second=%d fallback=%d body=%q",
			response.Code, firstCalls.Load(), secondCalls.Load(), fallbackCalls.Load(), response.Body.String())
	}
}

func TestCodexUsageLimitStillSwitchesOAuthAccount(t *testing.T) {
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	first := testCodexOAuthAccount("first", "real-first", now)
	second := testCodexOAuthAccount("second", "real-second", now)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, first, second)
	app.codexTraffic.jitter = func(time.Duration) time.Duration { return 0 }
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.Header.Get("ChatGPT-Account-ID") {
		case first.CodexAccountID:
			firstCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"usage_limit_reached"}}`)),
				Request:    request,
			}, nil
		case second.CodexAccountID:
			secondCalls.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\",\"output\":[]}}\n\n",
				)),
				Request: request,
			}, nil
		default:
			return nil, fmt.Errorf("unexpected Codex account %q", request.Header.Get("ChatGPT-Account-ID"))
		}
	})}

	response := proxyRequest(app, `{"input":"hello"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"resp_2"`) ||
		firstCalls.Load() != 1 || secondCalls.Load() != 1 || app.cfg.Accounts[0].BlockedReason != "quota" ||
		!app.codexTraffic.providerRecoveryAt(now).IsZero() {
		t.Fatalf("usage limit response=%d first=%d second=%d account=%#v body=%s",
			response.Code, firstCalls.Load(), secondCalls.Load(), app.cfg.Accounts[0], response.Body.String())
	}
}

func TestSafeConnectionFailureRecoversWithinFiveAttempts(t *testing.T) {
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://first.invalid/v1", "key-a"),
		testAccount("b", "https://second.invalid/v1", "key-b"),
	)
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "first.invalid" {
			if firstCalls.Add(1) < 5 {
				return nil, &net.OpError{Op: "dial", Err: errors.New("network unreachable")}
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader("first recovered")), Request: r}, nil
		}
		secondCalls.Add(1)
		return nil, errors.New("second account must not be used")
	})}
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusOK || response.Body.String() != "first recovered" ||
		firstCalls.Load() != 5 || secondCalls.Load() != 0 ||
		app.runtime["a"].UpstreamFailures != 0 {
		t.Fatalf("response=%d first=%d second=%d runtime=%#v body=%q",
			response.Code, firstCalls.Load(), secondCalls.Load(), app.runtime["a"], response.Body.String())
	}
}

func TestSafeConnectionFailureCountsOnceBeforeSwitching(t *testing.T) {
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://first.invalid/v1", "key-a"),
		testAccount("b", "https://second.invalid/v1", "key-b"),
	)
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "first.invalid" {
			firstCalls.Add(1)
			return nil, &net.OpError{Op: "dial", Err: errors.New("network unreachable")}
		}
		secondCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("second")), Request: r}, nil
	})}
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusOK || response.Body.String() != "second" ||
		firstCalls.Load() != 5 || secondCalls.Load() != 1 ||
		!app.runtime["a"].CooldownUntil.Equal(now.Add(10*time.Second)) ||
		app.runtime["a"].UpstreamFailures != 1 || app.cfg.Accounts[0].BlockedReason != "" {
		t.Fatalf("response=%d first=%d second=%d runtime=%#v account=%#v",
			response.Code, firstCalls.Load(), secondCalls.Load(), app.runtime["a"], app.cfg.Accounts[0])
	}
	if health := app.status().Accounts[0].HealthState; health != accountHealthRecentFailure {
		t.Fatalf("failed upstream health=%q, want recent failure", health)
	}
}

func TestSafeDNSFailureRetriesFiveTimes(t *testing.T) {
	account := testAccount("a", "https://first.invalid/v1", "key-a")
	app := newTestApplication(t, strategyPriority, time.Now, account)
	var calls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, &net.DNSError{Err: "temporary lookup failure", Name: "first.invalid"}
	})}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", strings.NewReader(`{}`))
	if _, err := app.sendUpstream(request.Context(), request, []byte(`{}`), account); err == nil || calls.Load() != 5 {
		t.Fatalf("dns retry err=%v calls=%d", err, calls.Load())
	}
}

func TestAmbiguousPostHTTPFailureCoolsWithoutReplay(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{
		{name: "request timeout", status: http.StatusRequestTimeout},
		{name: "bad gateway", status: http.StatusBadGateway},
		{name: "service unavailable", status: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			var firstCalls atomic.Int32
			var secondCalls atomic.Int32
			now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
			app := newTestApplication(t, strategyPriority, func() time.Time { return now },
				testAccount("a", "https://first.invalid/v1", "key-a"),
				testAccount("b", "https://second.invalid/v1", "key-b"),
			)
			app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "first.invalid" {
					firstCalls.Add(1)
					return &http.Response{StatusCode: test.status, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader(test.name)), Request: r}, nil
				}
				secondCalls.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader("second")), Request: r}, nil
			})}
			response := proxyRequest(app, `{}`)
			runtime := app.runtime["a"]
			if response.Code != test.status || response.Body.String() != test.name ||
				firstCalls.Load() != 1 || secondCalls.Load() != 0 || runtime.UpstreamFailures != 1 ||
				runtime.CooldownReason != "upstream_failures" || !runtime.CooldownUntil.Equal(now.Add(10*time.Second)) {
				t.Fatalf("response=%d first=%d second=%d runtime=%#v body=%q",
					response.Code, firstCalls.Load(), secondCalls.Load(), runtime, response.Body.String())
			}
		})
	}
}

func TestAmbiguousPostFailureIsNotReplayed(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "connection reset after write", err: errors.New("connection reset after write")},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			var firstCalls atomic.Int32
			var secondCalls atomic.Int32
			now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
			app := newTestApplication(t, strategyPriority, func() time.Time { return now },
				testAccount("a", "https://first.invalid/v1", "key-a"),
				testAccount("b", "https://second.invalid/v1", "key-b"),
			)
			app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "first.invalid" {
					firstCalls.Add(1)
					return nil, test.err
				}
				secondCalls.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader("second")), Request: r}, nil
			})}
			response := proxyRequest(app, `{}`)
			if response.Code != http.StatusBadGateway || firstCalls.Load() != 1 || secondCalls.Load() != 0 ||
				!strings.Contains(response.Body.String(), "upstream_result_unknown") ||
				app.runtime["a"].UpstreamFailures != 1 || app.runtime["a"].CooldownReason != "upstream_failures" ||
				!app.runtime["a"].CooldownUntil.Equal(now.Add(10*time.Second)) {
				t.Fatalf("ambiguous request status=%d first=%d second=%d body=%s",
					response.Code, firstCalls.Load(), secondCalls.Load(), response.Body.String())
			}
		})
	}
}

func TestAllSoftCooledAccountsUseControlledProbe(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://first.invalid/v1", "key-a"),
		testAccount("b", "https://second.invalid/v1", "key-b"),
	)
	for id, failures := range map[string]int{"a": 4, "b": 3} {
		runtime := app.runtime[id]
		runtime.UpstreamFailures = failures
		runtime.LastFailureAt = now.Add(-maxSoftProbeDelay)
		runtime.CooldownUntil = now.Add(upstreamFailureCooldown(failures))
		runtime.CooldownReason = "upstream_failures"
	}
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "first.invalid" {
			firstCalls.Add(1)
			return nil, errors.New("higher failure account must not be probed first")
		}
		secondCalls.Add(1)
		app.mu.Lock()
		probing := app.runtime["b"].ProbeInFlight
		app.mu.Unlock()
		if !probing {
			return nil, errors.New("soft-cooled account was not marked as half-open")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("recovered")), Request: r}, nil
	})}
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusOK || response.Body.String() != "recovered" ||
		firstCalls.Load() != 0 || secondCalls.Load() != 1 || app.runtime["b"].UpstreamFailures != 0 ||
		app.runtime["b"].ProbeInFlight {
		t.Fatalf("response=%d first=%d second=%d runtime=%#v body=%q",
			response.Code, firstCalls.Load(), secondCalls.Load(), app.runtime["b"], response.Body.String())
	}
}

func TestConcurrentRequestsWaitForHalfOpenProbe(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://first.invalid/v1", "key-a"),
	)
	runtime := app.runtime["a"]
	runtime.UpstreamFailures = 3
	runtime.LastFailureAt = now.Add(-maxSoftProbeDelay)
	runtime.CooldownUntil = now.Add(5 * time.Minute)
	runtime.CooldownReason = "upstream_failures"
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if call == 1 {
			close(started)
			<-release
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("response-" + strconv.Itoa(int(call)))), Request: r}, nil
	})}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- proxyRequest(app, `{}`) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("half-open probe did not start")
	}
	go func() { secondDone <- proxyRequest(app, `{}`) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		app.mu.Lock()
		sequence := app.requestSequence
		app.mu.Unlock()
		if sequence >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second request did not enter the router")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case response := <-secondDone:
		t.Fatalf("second request returned before probe completed: status=%d body=%s", response.Code, response.Body.String())
	case <-time.After(50 * time.Millisecond):
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent request duplicated half-open probe: calls=%d", calls.Load())
	}
	close(release)
	for index, done := range []<-chan *httptest.ResponseRecorder{firstDone, secondDone} {
		select {
		case response := <-done:
			if response.Code != http.StatusOK {
				t.Fatalf("request %d status=%d body=%s", index+1, response.Code, response.Body.String())
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("request %d did not finish", index+1)
		}
	}
	if calls.Load() != 2 || app.runtime["a"].ProbeInFlight {
		t.Fatalf("calls=%d runtime=%#v", calls.Load(), app.runtime["a"])
	}
}

func TestHalfOpenFirstStreamPayloadReleasesWaitingRequests(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://first.invalid/v1", "key-a"),
	)
	runtime := app.runtime["a"]
	runtime.UpstreamFailures = 3
	runtime.LastFailureAt = now.Add(-maxSoftProbeDelay)
	runtime.CooldownUntil = now.Add(5 * time.Minute)
	runtime.CooldownReason = "upstream_failures"
	reader, writer := io.Pipe()
	defer writer.Close()
	firstStarted := make(chan struct{})
	allowHeaders := make(chan struct{})
	var calls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		if call == 1 {
			close(firstStarted)
			<-allowHeaders
			return &http.Response{StatusCode: http.StatusOK,
				Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader, Request: r}, nil
		}
		app.mu.Lock()
		failures := app.runtime["a"].UpstreamFailures
		reason := app.runtime["a"].CooldownReason
		probing := app.runtime["a"].ProbeInFlight
		app.mu.Unlock()
		if failures != 3 || reason != "" || probing {
			return nil, errors.New("first stream payload did not release the account as regular traffic")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("second")), Request: r}, nil
	})}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- proxyRequest(app, `{}`) }()
	select {
	case <-firstStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("half-open request did not reach the upstream")
	}
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- proxyRequest(app, `{}`) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		app.mu.Lock()
		sequence := app.requestSequence
		app.mu.Unlock()
		if sequence >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second request did not enter the router")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case response := <-secondDone:
		t.Fatalf("second request returned before the first response headers: status=%d body=%s", response.Code, response.Body.String())
	case <-time.After(50 * time.Millisecond):
	}
	if calls.Load() != 1 {
		t.Fatalf("half-open request was duplicated before response headers: calls=%d", calls.Load())
	}
	close(allowHeaders)
	select {
	case response := <-secondDone:
		t.Fatalf("second request returned before the first stream payload: status=%d body=%s", response.Code, response.Body.String())
	case <-time.After(50 * time.Millisecond):
	}
	if calls.Load() != 1 {
		t.Fatalf("response headers released the half-open account: calls=%d", calls.Load())
	}
	payloadWritten := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, "data: done\n\n")
		payloadWritten <- err
	}()
	select {
	case err := <-payloadWritten:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		_ = writer.Close()
		t.Fatal("first stream payload was not consumed")
	}
	select {
	case response := <-secondDone:
		if response.Code != http.StatusOK || response.Body.String() != "second" {
			t.Fatalf("second response status=%d body=%q", response.Code, response.Body.String())
		}
	case <-time.After(2 * time.Second):
		_ = writer.Close()
		t.Fatal("waiting request remained blocked after the first stream payload")
	}
	_ = writer.Close()
	select {
	case response := <-firstDone:
		if response.Code != http.StatusOK || response.Body.String() != "data: done\n\n" {
			t.Fatalf("first response status=%d body=%q", response.Code, response.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("half-open stream did not finish")
	}
	if calls.Load() != 2 || app.runtime["a"].ProbeInFlight {
		t.Fatalf("calls=%d runtime=%#v", calls.Load(), app.runtime["a"])
	}
}

func TestHalfOpenBodyFailureContinuesBackoff(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://first.invalid/v1", "key-a"),
	)
	runtime := app.runtime["a"]
	runtime.UpstreamFailures = 3
	runtime.LastFailureAt = now.Add(-maxSoftProbeDelay)
	runtime.CooldownUntil = now.Add(5 * time.Minute)
	runtime.CooldownReason = "upstream_failures"
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK,
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:   &oneChunkThenError{chunk: []byte("data: partial\n\n")}, Request: r}, nil
	})}
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusOK || response.Body.String() != "data: partial\n\n" ||
		runtime.UpstreamFailures != 4 || !runtime.CooldownUntil.Equal(now.Add(15*time.Minute)) ||
		runtime.CooldownReason != "upstream_failures" || runtime.ProbeInFlight {
		t.Fatalf("status=%d runtime=%#v body=%q", response.Code, runtime, response.Body.String())
	}
}

func TestCanceledSoftWaitClosesPreviousResponse(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://first.invalid/v1", "key-a"),
		testAccount("b", "https://second.invalid/v1", "key-b"),
	)
	secondRuntime := app.runtime["b"]
	secondRuntime.UpstreamFailures = 1
	secondRuntime.LastFailureAt = now
	secondRuntime.CooldownUntil = now.Add(10 * time.Second)
	secondRuntime.CooldownReason = "upstream_failures"
	secondRuntime.ProbeInFlight = true
	closed := make(chan struct{})
	upstreamReturned := make(chan struct{})
	body := &closeSignalBody{Reader: strings.NewReader("failed"), closed: closed}
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(upstreamReturned)
		return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header), Body: body, Request: r}, nil
	})}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/v1/models", nil)
	request.Host = listenAddress
	markLocalRequest(request)
	request.Header.Set("Authorization", "Bearer gateway-token")
	ctx, cancel := context.WithCancel(request.Context())
	done := make(chan struct{})
	go func() {
		app.routes().ServeHTTP(httptest.NewRecorder(), request.WithContext(ctx))
		close(done)
	}()
	select {
	case <-upstreamReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("first upstream response was not returned")
	}
	select {
	case <-done:
		t.Fatal("request returned instead of waiting for the soft-cooled account")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-closed:
		t.Fatal("previous response was closed before the wait was canceled")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled request did not stop")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("previous response body was not closed")
	}
}

func TestOldProbeCannotClearNewRevisionProbe(t *testing.T) {
	oldAccount := testAccount("a", "https://a.invalid/v1", "key-a")
	app := newTestApplication(t, strategyPriority, time.Now, oldAccount)
	app.runtime["a"].ProbeInFlight = true
	newAccount := oldAccount
	newAccount.Revision++
	app.cfg.Accounts[0] = newAccount
	cooldownUntil := time.Now().Add(time.Minute)
	app.runtime["a"] = &accountRuntime{
		Revision: newAccount.Revision, ProbeInFlight: true, UpstreamFailures: 2,
		CooldownReason: "upstream_failures", CooldownUntil: cooldownUntil,
	}
	changed := app.probeChanged
	app.finishAccountProbe(oldAccount, true)
	runtime := app.runtime["a"]
	if !runtime.ProbeInFlight || runtime.UpstreamFailures != 2 || runtime.CooldownReason != "upstream_failures" ||
		!runtime.CooldownUntil.Equal(cooldownUntil) {
		t.Fatalf("old probe changed the new revision runtime: %#v", runtime)
	}
	select {
	case <-changed:
	default:
		t.Fatal("old probe completion did not wake waiters")
	}
}

func TestHalfOpenFailureAdvancesBackoff(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	runtime := app.runtime["a"]
	runtime.UpstreamFailures = 3
	runtime.LastFailureAt = now.Add(-maxSoftProbeDelay)
	runtime.CooldownUntil = now.Add(5 * time.Minute)
	runtime.CooldownReason = "upstream_failures"
	var calls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("still failing")), Request: r}, nil
	})}
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusBadGateway || response.Body.String() != "still failing" || calls.Load() != 1 ||
		runtime.UpstreamFailures != 4 || !runtime.CooldownUntil.Equal(now.Add(15*time.Minute)) || runtime.ProbeInFlight {
		t.Fatalf("status=%d calls=%d runtime=%#v body=%q", response.Code, calls.Load(), runtime, response.Body.String())
	}
}

func TestUpstreamFailureCooldownBacksOffAndSuccessResets(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	wants := []time.Duration{10 * time.Second, 30 * time.Second, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute}
	for index, want := range wants {
		action := app.cooldownAccountForFailure(account)
		if got := app.runtime["a"].CooldownUntil.Sub(now); got != want {
			t.Fatalf("failure %d cooldown=%s, want %s", index+1, got, want)
		}
		if index >= 2 && action != "temporarily_disabled" {
			t.Fatalf("failure %d action=%q", index+1, action)
		} else if index < 2 && action != "cooldown" {
			t.Fatalf("failure %d action=%q", index+1, action)
		}
		now = app.runtime["a"].CooldownUntil
	}
	now = app.runtime["a"].CooldownUntil.Add(upstreamFailureReset + time.Second)
	app.cooldownAccountForFailure(account)
	if runtime := app.runtime["a"]; runtime.UpstreamFailures != 1 || runtime.CooldownUntil.Sub(now) != 10*time.Second {
		t.Fatalf("idle account did not reset backoff: %#v", runtime)
	}
	app.markAccountVerified(account)
	if runtime := app.runtime["a"]; runtime.UpstreamFailures != 0 || runtime.CooldownReason != "" ||
		!runtime.CooldownUntil.IsZero() || !runtime.LastFailureAt.IsZero() {
		t.Fatalf("successful request did not reset backoff: %#v", runtime)
	}
}

func TestInFlightFailuresOnlyCountOnce(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	startedAt := now
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	app.cooldownAccountForFailure(account, startedAt)
	now = now.Add(11 * time.Second)
	if action := app.cooldownAccountForFailure(account, startedAt); action != "ignored" {
		t.Fatalf("stale in-flight failure action=%q", action)
	}
	if runtime := app.runtime["a"]; runtime.UpstreamFailures != 1 {
		t.Fatalf("stale in-flight failure was counted twice: %#v", runtime)
	}
}

func TestSuccessfulResponseIgnoresOlderInFlightFailure(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	startedAt := now
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	now = now.Add(time.Second)
	app.markAccountVerified(account)
	now = now.Add(time.Second)
	if action := app.cooldownAccountForFailure(account, startedAt); action != "ignored" {
		t.Fatalf("old failure after success action=%q", action)
	}
	if runtime := app.runtime["a"]; runtime.UpstreamFailures != 0 || !runtime.CooldownUntil.IsZero() {
		t.Fatalf("old failure changed recovered account: %#v", runtime)
	}
}

func TestAccountResumeArmsSingleProbeWithoutClearingFailures(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://a.invalid/v1", "key-a"))
	for failure := range 3 {
		app.cooldownAccountForFailure(app.cfg.Accounts[0])
		if failure < 2 {
			now = app.runtime["a"].CooldownUntil
		}
	}
	if state := app.status().Accounts[0].State; state != "temporarily_disabled" {
		t.Fatalf("state=%q, want temporarily_disabled", state)
	}
	response := adminJSON(app, http.MethodPost, "/admin/accounts/resume",
		map[string]string{"id": "a"}, "http://127.0.0.1:4000")
	runtime := app.runtime["a"]
	if response.Code != http.StatusAccepted || runtime.UpstreamFailures != 3 ||
		runtime.CooldownReason != "upstream_failures" || !runtime.CooldownUntil.Equal(now) {
		t.Fatalf("resume status=%d runtime=%#v body=%s", response.Code, app.runtime["a"], response.Body.String())
	}
	selected, ok, probe := app.selectProxyAccount(context.Background(), map[string]bool{})
	if !ok || !probe || selected.ID != "a" || !runtime.ProbeInFlight {
		t.Fatalf("resumed account was not selected as probe: selected=%#v ok=%v probe=%v runtime=%#v",
			selected, ok, probe, runtime)
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if selected, ok, probe = app.selectProxyAccount(waitCtx, map[string]bool{}); ok || probe {
		t.Fatalf("second resumed probe was selected: selected=%#v ok=%v probe=%v", selected, ok, probe)
	}
	app.finishAccountProbe(app.cfg.Accounts[0], false)
}

func TestSSEFirstPayloadReplayBoundary(t *testing.T) {
	for _, test := range []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantSecond int32
	}{
		{name: "safe GET switches before first payload", method: http.MethodGet, path: "/v1/models", wantStatus: http.StatusOK, wantSecond: 1},
		{name: "POST stays on the first attempt", method: http.MethodPost, path: "/v1/responses", body: `{}`, wantStatus: http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			var firstCalls atomic.Int32
			var secondCalls atomic.Int32
			now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
			app := newTestApplication(t, strategyPriority, func() time.Time { return now },
				testAccount("a", "https://first.invalid/v1", "key-a"),
				testAccount("b", "https://second.invalid/v1", "key-b"),
			)
			app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "first.invalid" {
					firstCalls.Add(1)
					return &http.Response{
						StatusCode: http.StatusOK,
						Header: http.Header{
							"Content-Type": []string{"text/event-stream"},
							"X-Upstream":   []string{"first"},
						},
						Body: &oneChunkThenError{}, Request: r,
					}, nil
				}
				secondCalls.Add(1)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{
						"Content-Type": []string{"text/event-stream"},
						"X-Upstream":   []string{"second"},
					},
					Body: io.NopCloser(strings.NewReader("data: ok\n\n")), Request: r,
				}, nil
			})}
			var requestBody io.Reader
			if test.body != "" {
				requestBody = strings.NewReader(test.body)
			}
			request := httptest.NewRequest(test.method, "http://127.0.0.1:4000"+test.path, requestBody)
			request.Host = listenAddress
			markLocalRequest(request)
			request.Header.Set("Authorization", "Bearer gateway-token")
			if test.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			response := httptest.NewRecorder()
			app.routes().ServeHTTP(response, request)

			if response.Code != test.wantStatus || firstCalls.Load() != 1 || secondCalls.Load() != test.wantSecond {
				t.Fatalf("status=%d first=%d second=%d body=%s",
					response.Code, firstCalls.Load(), secondCalls.Load(), response.Body.String())
			}
			if test.wantSecond == 1 {
				if response.Body.String() != "data: ok\n\n" || response.Header().Get("X-Upstream") != "second" {
					t.Fatalf("safe retry leaked the first response: headers=%v body=%q", response.Header(), response.Body.String())
				}
			} else if !strings.Contains(response.Body.String(), "upstream_result_unknown") || response.Header().Get("X-Upstream") != "" {
				t.Fatalf("ambiguous POST response=%v body=%s", response.Header(), response.Body.String())
			}
			runtime := app.runtime["a"]
			if runtime.UpstreamFailures != 1 || runtime.CooldownReason != "upstream_failures" ||
				!runtime.CooldownUntil.Equal(now.Add(10*time.Second)) {
				t.Fatalf("first account was not cooled down: %#v", runtime)
			}
		})
	}
}

func TestStartedSSEStreamIsNeverReplayed(t *testing.T) {
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "first.invalid" {
			firstCalls.Add(1)
		} else if r.URL.Host == "second.invalid" {
			secondCalls.Add(1)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &oneChunkThenError{chunk: []byte("data: partial\n\n")}, Request: r,
		}, nil
	})
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://first.invalid/v1", "key-a"),
		testAccount("b", "https://second.invalid/v1", "key-b"),
	)
	var logs bytes.Buffer
	app.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	app.client = &http.Client{Transport: transport}
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusOK || response.Body.String() != "data: partial\n\n" {
		t.Fatalf("unexpected stream: status=%d body=%q", response.Code, response.Body.String())
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("started stream calls: first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
	if !response.Flushed {
		t.Fatal("stream response was not flushed")
	}
	if strings.Contains(logs.String(), "data: partial") || !strings.Contains(logs.String(), "terminal=upstream_read_error") {
		t.Fatalf("unsafe or incomplete stream log: %s", logs.String())
	}
	if !app.runtime["a"].CooldownUntil.Equal(now.Add(10*time.Second)) || app.cfg.Accounts[0].Verified ||
		app.status().Accounts[0].HealthState != accountHealthRecentFailure {
		t.Fatalf("failed stream state: runtime=%#v account=%#v", app.runtime["a"], app.cfg.Accounts[0])
	}
}

func TestCanceledRequestsDoNotChangeHealth(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	newApp := func(t *testing.T) *application {
		t.Helper()
		account := testAccount("a", "https://a.invalid/v1", "key-a")
		account.Verified = true
		app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
		app.runtime["a"].HealthState = accountHealthRecentSuccess
		app.runtime["a"].HealthCheckedAt = now
		return app
	}

	t.Run("proxy", func(t *testing.T) {
		app := newApp(t)
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", strings.NewReader(`{}`))
		request.Host = listenAddress
		markLocalRequest(request)
		request.Header.Set("Authorization", "Bearer gateway-token")
		request.Header.Set("Content-Type", "application/json")
		ctx, cancel := context.WithCancel(request.Context())
		cancel()
		app.routes().ServeHTTP(httptest.NewRecorder(), request.WithContext(ctx))
		accountStatus := app.status().Accounts[0]
		if accountStatus.HealthState != accountHealthRecentSuccess ||
			accountStatus.SuccessfulRequests != 0 || accountStatus.FailedRequests != 0 {
			t.Fatalf("canceled proxy changed account status: %#v", accountStatus)
		}
	})

	t.Run("admin models", func(t *testing.T) {
		app := newApp(t)
		body, err := json.Marshal(accountProbeRequest{AccountID: "a", AllowInsecureHTTP: boolPointer(true)})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/admin/models", bytes.NewReader(body))
		request.Host = listenAddress
		markLocalRequest(request)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", "http://127.0.0.1:4000")
		request.Header.Set("X-CSRF-Token", app.csrfToken)
		ctx, cancel := context.WithCancel(request.Context())
		cancel()
		app.routes().ServeHTTP(httptest.NewRecorder(), request.WithContext(ctx))
		if health := app.status().Accounts[0].HealthState; health != accountHealthRecentSuccess {
			t.Fatalf("canceled admin models changed health to %q", health)
		}
	})
}

func TestForwardResponseIdleTimeout(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	response := &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader,
	}
	done := make(chan error, 1)
	go func() { done <- forwardResponseWithIdleTimeout(httptest.NewRecorder(), response, 10*time.Millisecond) }()
	select {
	case err := <-done:
		if !errors.Is(err, errUpstreamIdleTimeout) {
			t.Fatalf("idle response error=%v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle response did not time out")
	}
}

func TestForwardResponseDownstreamWriteFailureIsLoggedWithoutUpstreamFailure(t *testing.T) {
	var logs bytes.Buffer
	app := &application{logger: slog.New(slog.NewTextHandler(&logs, nil))}
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("response")),
	}
	if err := app.forwardResponse(&writeErrorResponseWriter{}, response, 1, "account", 1, time.Now()); err != nil {
		t.Fatalf("downstream write failure was returned as upstream failure: %v", err)
	}
	if !strings.Contains(logs.String(), "terminal=downstream_write_error") {
		t.Fatalf("downstream write failure was not logged: %s", logs.String())
	}
}

func TestForwardResponseSetsAndClearsWriteDeadline(t *testing.T) {
	writer := &deadlineResponseWriter{header: make(http.Header)}
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("response")),
	}
	written, terminal, _, err := forwardResponseWithTimeoutsResult(writer, response, time.Second, 50*time.Millisecond)
	if err != nil || terminal != "eof" || written != int64(len("response")) {
		t.Fatalf("forward result written=%d terminal=%q err=%v", written, terminal, err)
	}
	if writer.writeWithoutDeadline {
		t.Fatal("response body was written without a downstream deadline")
	}
	if len(writer.deadlines) < 2 || writer.deadlines[0].IsZero() || !writer.deadlines[len(writer.deadlines)-1].IsZero() {
		t.Fatalf("write deadlines were not set and cleared: %#v", writer.deadlines)
	}
}

func TestAdminSaveDoesNotBlockRoutingWhilePersisting(t *testing.T) {
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	app := newTestApplication(t, strategyPriority, time.Now, account)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseSave := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseSave()
	app.persistConfig = func(string, storedConfig) error {
		close(entered)
		<-release
		return nil
	}
	strategy := strategyPriority
	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responseDone <- adminJSON(app, http.MethodPut, "/admin/api/config", saveRequest{Strategy: &strategy}, "http://127.0.0.1:4000")
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("configuration persistence did not start")
	}
	if !app.mu.TryLock() {
		t.Fatal("configuration persistence still holds application.mu")
	}
	app.mu.Unlock()
	selectedDone := make(chan accountConfig, 1)
	go func() {
		selected, _ := app.selectAccount(context.Background(), map[string]bool{})
		selectedDone <- selected
	}()
	select {
	case selected := <-selectedDone:
		if selected.ID != "a" {
			t.Fatalf("selected account=%q", selected.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("routing was blocked by configuration persistence")
	}
	releaseSave()
	select {
	case response := <-responseDone:
		if response.Code != http.StatusOK {
			t.Fatalf("save status=%d body=%s", response.Code, response.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("configuration save did not finish")
	}
	if assigned := app.status().Accounts[0].AssignedRequests; assigned != 1 {
		t.Fatalf("routing state was lost during config commit: assigned=%d", assigned)
	}
}

func TestVerifiedAccountSuccessDoesNotWaitForConfigPersistence(t *testing.T) {
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	account.Verified = true
	app := newTestApplication(t, strategyPriority, time.Now, account)
	app.configMu.Lock()
	locked := true
	defer func() {
		if locked {
			app.configMu.Unlock()
		}
	}()
	done := make(chan struct{})
	go func() {
		app.markAccountVerified(account)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("verified account success waited for configuration persistence")
	}
	app.mu.Lock()
	healthState := app.runtime[account.ID].HealthState
	app.mu.Unlock()
	if healthState != accountHealthRecentSuccess {
		t.Fatalf("health state=%q, want %q", healthState, accountHealthRecentSuccess)
	}
	app.configMu.Unlock()
	locked = false
}

func TestAccountStatePersistenceDoesNotHoldApplicationLock(t *testing.T) {
	tests := []struct {
		name   string
		update func(*application, accountConfig)
	}{
		{name: "verify", update: func(app *application, account accountConfig) { app.markAccountVerified(account) }},
		{name: "block", update: func(app *application, account accountConfig) { app.blockAccount(account, "quota") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			account := testAccount("a", "https://a.invalid/v1", "key-a")
			app := newTestApplication(t, strategyPriority, time.Now, account)
			entered := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			releasePersistence := func() { releaseOnce.Do(func() { close(release) }) }
			defer releasePersistence()
			app.persistConfig = func(string, storedConfig) error {
				close(entered)
				<-release
				return nil
			}
			done := make(chan struct{})
			go func() {
				test.update(app, account)
				close(done)
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("account state persistence did not start")
			}
			if !app.mu.TryLock() {
				t.Fatal("account state persistence held application.mu")
			}
			app.mu.Unlock()
			releasePersistence()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("account state update did not finish")
			}
		})
	}
}

func TestAdminSaveKeepsExistingKeysGeneratesIDsAndRedacts(t *testing.T) {
	first := testAccount("a", "https://a.invalid/v1", "secret-a")
	first.Verified = true
	first.BlockedReason = "quota"
	second := testAccount("b", "https://b.invalid/v1", "secret-b")
	app := newTestApplication(t, strategyPriority, time.Now, first, second)
	app.runtime["a"].AssignedRequests = 9
	enabled := true
	newProvider := "openai_responses"
	request := saveRequest{
		Accounts: &[]accountInput{
			{ID: "a", Name: "A renamed", BaseURL: "https://a.invalid/v1", Enabled: &enabled},
			{Name: "C", Provider: &newProvider, BaseURL: "https://c.invalid/v1", APIKey: "secret-c", Enabled: &enabled},
		},
	}
	response := adminJSON(app, http.MethodPut, "/admin/config", request, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", response.Code, response.Body.String())
	}
	for _, secret := range []string{"secret-a", "secret-b", "secret-c"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("save response leaked %q: %s", secret, response.Body.String())
		}
	}
	if len(app.cfg.Accounts) != 2 || app.cfg.Accounts[0].APIKey != "secret-a" || app.cfg.Accounts[1].ID == "" ||
		app.cfg.Accounts[1].Provider != "openai_responses" || app.cfg.Accounts[1].APIKey != "secret-c" {
		t.Fatalf("wrong saved accounts: %#v", app.cfg.Accounts)
	}
	if app.cfg.Accounts[0].Revision != 1 || app.runtime["a"].AssignedRequests != 9 {
		t.Fatalf("unchanged account lost revision/runtime: %#v %#v", app.cfg.Accounts[0], app.runtime["a"])
	}
	if app.accountIndexLocked("b") >= 0 {
		t.Fatal("omitted account was not deleted")
	}
}

func TestAdminProviderIsRequiredPreservedAndResetsProxyState(t *testing.T) {
	account := testAccount("a", "https://a.invalid/v1", "secret-a")
	account.Provider = "new_api"
	account.NewAPIAuthMode = newAPIAuthAccessToken
	account.NewAPIUserID = 42
	account.NewAPISecret = "account-secret"
	account.Revision = 4
	account.Verified = true
	account.BlockedReason = "quota"
	app := newTestApplication(t, strategyPriority, time.Now, account)
	app.runtime["a"].AssignedRequests = 9
	enabled := true

	preserve := saveRequest{Accounts: &[]accountInput{{
		ID: "a", Name: "A renamed", BaseURL: account.BaseURL, Enabled: &enabled,
		NewAPIAuthMode: newAPIAuthAccessToken, NewAPIUserID: 42,
	}}}
	response := adminJSON(app, http.MethodPut, "/admin/config", preserve, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || app.cfg.Accounts[0].Provider != "new_api" ||
		app.cfg.Accounts[0].Revision != 4 || app.cfg.Accounts[0].APIKey != "secret-a" ||
		app.cfg.Accounts[0].NewAPISecret != "account-secret" || app.runtime["a"].AssignedRequests != 9 ||
		strings.Contains(response.Body.String(), "account-secret") {
		t.Fatalf("nil provider was not preserved: status=%d account=%#v runtime=%#v body=%s",
			response.Code, app.cfg.Accounts[0], app.runtime["a"], response.Body.String())
	}

	nextProvider := "sub2api"
	change := saveRequest{Accounts: &[]accountInput{{
		ID: "a", Name: "A renamed", Provider: &nextProvider, BaseURL: account.BaseURL, Enabled: &enabled,
	}}}
	response = adminJSON(app, http.MethodPut, "/admin/config", change, "http://127.0.0.1:4000")
	changed := app.cfg.Accounts[0]
	if response.Code != http.StatusOK || changed.Provider != "sub2api" || changed.Revision != 5 ||
		changed.Verified || changed.BlockedReason != "" || changed.APIKey != "secret-a" ||
		changed.NewAPIAuthMode != newAPIAuthAPIKey || changed.NewAPIUsername != "" ||
		changed.NewAPIUserID != 0 || changed.NewAPISecret != "" ||
		app.runtime["a"].AssignedRequests != 0 || strings.Contains(response.Body.String(), "secret-a") ||
		strings.Contains(response.Body.String(), "account-secret") {
		t.Fatalf("provider change kept stale state or leaked the key: status=%d account=%#v runtime=%#v body=%s",
			response.Code, changed, app.runtime["a"], response.Body.String())
	}

	missingProvider := saveRequest{Accounts: &[]accountInput{{
		Name: "new", BaseURL: "https://new.invalid/v1", APIKey: "new-key", Enabled: &enabled,
	}}}
	response = adminJSON(app, http.MethodPut, "/admin/config", missingProvider, "http://127.0.0.1:4000")
	if response.Code != http.StatusBadRequest || len(app.cfg.Accounts) != 1 || app.cfg.Accounts[0].ID != "a" {
		t.Fatalf("new API Key account without provider status=%d config=%#v body=%s",
			response.Code, app.cfg.Accounts, response.Body.String())
	}
	autoProvider := "auto"
	legacyOnly := saveRequest{Accounts: &[]accountInput{{
		Name: "new", Provider: &autoProvider, BaseURL: "https://new.invalid/v1", APIKey: "new-key", Enabled: &enabled,
	}}}
	response = adminJSON(app, http.MethodPut, "/admin/config", legacyOnly, "http://127.0.0.1:4000")
	if response.Code != http.StatusBadRequest || len(app.cfg.Accounts) != 1 || app.cfg.Accounts[0].ID != "a" {
		t.Fatalf("new API Key account accepted legacy auto provider: status=%d config=%#v body=%s",
			response.Code, app.cfg.Accounts, response.Body.String())
	}
}

func TestAdminSaveURLOrKeyChangeResetsStateAndRejectsDuplicates(t *testing.T) {
	account := testAccount("a", "https://a.invalid/v1", "secret-a")
	account.Verified = true
	account.BlockedReason = "quota"
	app := newTestApplication(t, strategyPriority, time.Now, account)
	app.runtime["a"].AssignedRequests = 4
	enabled := true
	provider := "openai_responses"
	request := saveRequest{Accounts: &[]accountInput{
		{ID: "a", Name: "A", Provider: &provider, BaseURL: "https://changed.invalid/v1", APIKey: "new-secret", Enabled: &enabled},
	}}
	response := adminJSON(app, http.MethodPut, "/admin/config", request, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK {
		t.Fatalf("save failed: status=%d body=%s", response.Code, response.Body.String())
	}
	changed := app.cfg.Accounts[0]
	if changed.Revision != 2 || changed.Verified || changed.BlockedReason != "" ||
		app.runtime["a"].AssignedRequests != 0 {
		t.Fatalf("stale state survived config change: account=%#v runtime=%#v", changed, app.runtime["a"])
	}

	duplicates := saveRequest{Accounts: &[]accountInput{
		{ID: "a", Name: "A", BaseURL: changed.BaseURL, Enabled: &enabled},
		{ID: "a", Name: "A2", BaseURL: changed.BaseURL, Enabled: &enabled},
	}}
	duplicateResponse := adminJSON(app, http.MethodPut, "/admin/config", duplicates, "http://127.0.0.1:4000")
	if duplicateResponse.Code != http.StatusBadRequest {
		t.Fatalf("duplicate IDs status=%d body=%s", duplicateResponse.Code, duplicateResponse.Body.String())
	}
}

func TestSavedKeyCannotMoveToDifferentOriginImplicitly(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://old.invalid/v1", "secret-a"),
	)
	enabled := true
	crossOrigin := saveRequest{Accounts: &[]accountInput{
		{ID: "a", Name: "A", BaseURL: "https://new.invalid/v1", Enabled: &enabled},
	}}
	response := adminJSON(app, http.MethodPut, "/admin/config", crossOrigin, "http://127.0.0.1:4000")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "重新填写 API Key") {
		t.Fatalf("cross-origin save status=%d body=%s", response.Code, response.Body.String())
	}
	if app.cfg.Accounts[0].BaseURL != "https://old.invalid/v1" || app.cfg.Accounts[0].APIKey != "secret-a" {
		t.Fatalf("rejected save changed account: %#v", app.cfg.Accounts[0])
	}

	sameOrigin := saveRequest{Accounts: &[]accountInput{
		{ID: "a", Name: "A", BaseURL: "https://old.invalid/openai/v1", Enabled: &enabled},
	}}
	response = adminJSON(app, http.MethodPut, "/admin/config", sameOrigin, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || app.cfg.Accounts[0].APIKey != "secret-a" {
		t.Fatalf("same-origin path change failed: status=%d account=%#v", response.Code, app.cfg.Accounts[0])
	}

	var calls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("must not send saved key")
	})}
	test := accountProbeRequest{
		AccountID: "a",
		Candidate: &accountInput{BaseURL: "https://attacker.invalid/v1"},
	}
	response = adminJSON(app, http.MethodPost, "/admin/models", test, "http://127.0.0.1:4000")
	if response.Code != http.StatusBadRequest || calls.Load() != 0 ||
		!strings.Contains(response.Body.String(), "重新填写 API Key") {
		t.Fatalf("cross-origin test status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
	}
}

func TestAdminSaveUpdatesLastRoutedSummary(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://a.invalid/v1", "secret-a"),
		testAccount("b", "https://b.invalid/v1", "secret-b"),
	)
	app.lastRoutedAccountID = "a"
	app.lastRoutedAccountName = "A"
	enabled := true
	rename := saveRequest{Accounts: &[]accountInput{
		{ID: "a", Name: "A renamed", BaseURL: "https://a.invalid/v1", Enabled: &enabled},
		{ID: "b", Name: "B", BaseURL: "https://b.invalid/v1", Enabled: &enabled},
	}}
	response := adminJSON(app, http.MethodPut, "/admin/config", rename, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || app.lastRoutedAccountName != "A renamed" {
		t.Fatalf("rename summary status=%d id=%q name=%q", response.Code, app.lastRoutedAccountID, app.lastRoutedAccountName)
	}
	remove := saveRequest{Accounts: &[]accountInput{
		{ID: "b", Name: "B", BaseURL: "https://b.invalid/v1", Enabled: &enabled},
	}}
	response = adminJSON(app, http.MethodPut, "/admin/config", remove, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || app.lastRoutedAccountID != "" || app.lastRoutedAccountName != "" {
		t.Fatalf("delete summary status=%d id=%q name=%q", response.Code, app.lastRoutedAccountID, app.lastRoutedAccountName)
	}
}

func TestNewAccountRequiresKeyAndClearIsExplicit(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://a.invalid/v1", "secret-a"),
	)
	enabled := true
	provider := "openai_responses"
	missingKey := saveRequest{Accounts: &[]accountInput{
		{Name: "new", Provider: &provider, BaseURL: "https://new.invalid/v1", Enabled: &enabled},
	}}
	response := adminJSON(app, http.MethodPut, "/admin/config", missingKey, "http://127.0.0.1:4000")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("new account without key status=%d", response.Code)
	}
	clear := saveRequest{Accounts: &[]accountInput{
		{ID: "a", Name: "A", BaseURL: "https://a.invalid/v1", Enabled: &enabled, ClearAPIKey: true},
	}}
	response = adminJSON(app, http.MethodPut, "/admin/config", clear, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || app.cfg.Accounts[0].APIKey != "" || app.cfg.Accounts[0].Revision != 2 {
		t.Fatalf("explicit clear failed: status=%d account=%#v", response.Code, app.cfg.Accounts[0])
	}
}

func TestAdminNewAPISecretLifecycleIsRedactedAndOriginBound(t *testing.T) {
	account := testAccount("a", "https://old.invalid/v1", "model-key")
	account.Provider = "new_api"
	account.NewAPIAuthMode = newAPIAuthAccessToken
	account.NewAPIUserID = 42
	account.NewAPISecret = "access-secret"
	app := newTestApplication(t, strategyPriority, time.Now, account)
	enabled := true

	publicRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/admin/config", nil)
	publicRequest.Host = listenAddress
	markLocalRequest(publicRequest)
	publicResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(publicResponse, publicRequest)
	if publicResponse.Code != http.StatusOK || strings.Contains(publicResponse.Body.String(), "model-key") ||
		strings.Contains(publicResponse.Body.String(), "access-secret") ||
		strings.Contains(publicResponse.Body.String(), `"newApiSecret":`) ||
		!strings.Contains(publicResponse.Body.String(), `"newApiSecretConfigured":true`) ||
		!strings.Contains(publicResponse.Body.String(), `"provider":"new_api"`) {
		t.Fatalf("public config leaked or hid credential state: status=%d", publicResponse.Code)
	}

	preserve := saveRequest{Accounts: &[]accountInput{{
		ID: "a", Name: "A renamed", BaseURL: "https://old.invalid/v1", Enabled: &enabled,
		NewAPIAuthMode: newAPIAuthAccessToken, NewAPIUserID: 42,
	}}}
	response := adminJSON(app, http.MethodPut, "/admin/config", preserve, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || app.cfg.Accounts[0].NewAPISecret != "access-secret" ||
		app.cfg.Accounts[0].Revision != 1 || strings.Contains(response.Body.String(), "access-secret") {
		t.Fatalf("credential preservation failed: status=%d revision=%d",
			response.Code, app.cfg.Accounts[0].Revision)
	}

	crossOrigin := saveRequest{Accounts: &[]accountInput{{
		ID: "a", Name: "A renamed", BaseURL: "https://other.invalid/v1", APIKey: "model-key", Enabled: &enabled,
		NewAPIAuthMode: newAPIAuthAccessToken, NewAPIUserID: 42,
	}}}
	response = adminJSON(app, http.MethodPut, "/admin/config", crossOrigin, "http://127.0.0.1:4000")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "New API 余额凭据") ||
		app.cfg.Accounts[0].BaseURL != "https://old.invalid/v1" || app.cfg.Accounts[0].Revision != 1 {
		t.Fatalf("cross-origin credential reuse was not rejected: status=%d revision=%d body=%s",
			response.Code, app.cfg.Accounts[0].Revision, response.Body.String())
	}

	replace := saveRequest{Accounts: &[]accountInput{{
		ID: "a", Name: "A renamed", BaseURL: "https://old.invalid/v1", Enabled: &enabled,
		NewAPIAuthMode: newAPIAuthAccessToken, NewAPIUserID: 42, NewAPISecret: "replacement-secret",
	}}}
	response = adminJSON(app, http.MethodPut, "/admin/config", replace, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || app.cfg.Accounts[0].NewAPISecret != "replacement-secret" ||
		app.cfg.Accounts[0].Revision != 2 || strings.Contains(response.Body.String(), "replacement-secret") {
		t.Fatalf("credential replacement failed: status=%d revision=%d",
			response.Code, app.cfg.Accounts[0].Revision)
	}

	clear := saveRequest{Accounts: &[]accountInput{{
		ID: "a", Name: "A renamed", BaseURL: "https://old.invalid/v1", Enabled: &enabled,
		NewAPIAuthMode: newAPIAuthAPIKey, ClearNewAPISecret: true,
	}}}
	response = adminJSON(app, http.MethodPut, "/admin/config", clear, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || app.cfg.Accounts[0].NewAPISecret != "" ||
		app.cfg.Accounts[0].NewAPIAuthMode != newAPIAuthAPIKey || app.cfg.Accounts[0].Revision != 3 ||
		!strings.Contains(response.Body.String(), `"newApiSecretConfigured":false`) {
		t.Fatalf("credential clear failed: status=%d revision=%d mode=%q body=%s",
			response.Code, app.cfg.Accounts[0].Revision, app.cfg.Accounts[0].NewAPIAuthMode, response.Body.String())
	}
	for _, secret := range []string{"access-secret", "replacement-secret"} {
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("save response leaked a New API credential")
		}
	}
}

func TestProviderScopesBalanceEndpoints(t *testing.T) {
	t.Run("openai responses uses dashboard only", func(t *testing.T) {
		var calls atomic.Int32
		var apiCalls atomic.Int32
		var secretLeaks atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			body, _ := io.ReadAll(r.Body)
			if strings.HasPrefix(r.URL.Path, "/api/") {
				apiCalls.Add(1)
			}
			if strings.Contains(fmt.Sprint(r.Header), "account-secret") || strings.Contains(string(body), "account-secret") {
				secretLeaks.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/v1/dashboard/billing/subscription":
				_, _ = io.WriteString(w, `{"hard_limit_usd":10}`)
			case "/v1/dashboard/billing/usage":
				_, _ = io.WriteString(w, `{"total_usage":100}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()

		account := testAccount("a", server.URL+"/v1", "proxy-key")
		account.Provider = "openai_responses"
		account.NewAPIAuthMode = newAPIAuthPassword
		account.NewAPIUsername = "user"
		account.NewAPISecret = "account-secret"
		app := newTestApplication(t, strategyPriority, time.Now, account)
		balance := app.probeBalance(context.Background(), account)
		if balance.Status != balanceRefreshOK || calls.Load() != 2 || apiCalls.Load() != 0 || secretLeaks.Load() != 0 {
			t.Fatalf("openai_responses balance=%#v calls=%d api=%d secretLeaks=%d",
				balance, calls.Load(), apiCalls.Load(), secretLeaks.Load())
		}
	})

	t.Run("sub2api is unsupported without transport", func(t *testing.T) {
		account := testAccount("a", "https://sub2api.invalid/v1", "proxy-key")
		account.Provider = "sub2api"
		account.NewAPIAuthMode = newAPIAuthAccessToken
		account.NewAPIUserID = 42
		account.NewAPISecret = "account-secret"
		app := newTestApplication(t, strategyPriority, time.Now, account)
		var calls atomic.Int32
		app.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return nil, errors.New("transport must not be called")
		})}
		balance := app.probeBalance(context.Background(), account)
		if balance.Status != balanceRefreshUnsupported || balance.RefreshStatus != balanceRefreshUnsupported ||
			balance.ErrorCode != balanceErrorUnsupported || calls.Load() != 0 {
			t.Fatalf("sub2api balance=%#v calls=%d", balance, calls.Load())
		}
	})
}

func TestBalanceProbeNewAPIAndDashboardCompatibility(t *testing.T) {
	t.Run("New API", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer balance-key" {
				t.Fatalf("wrong authorization: %q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/usage/token/":
				_, _ = io.WriteString(w, `{"data":{"total_available":1000000,"unlimited_quota":false}}`)
			case "/api/status":
				_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		app := newTestApplication(t, strategyPriority, time.Now,
			testAccount("a", server.URL+"/v1", "balance-key"),
		)
		balance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
		if balance.Status != "ok" || balance.Amount != 2 || balance.Unit != "USD" ||
			balance.DisplayLabel != "USD" {
			t.Fatalf("unexpected New API balance: %#v", balance)
		}
	})

	for _, test := range []struct {
		name          string
		hardLimit     string
		statusBody    string
		wantAmount    float64
		wantUnit      string
		wantUnlimited bool
		wantScope     string
	}{
		{name: "unlimited token compatible bill stays unverified", hardLimit: "6", statusBody: `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`, wantAmount: 5, wantUnit: "USD", wantScope: balanceScopeTokenOnly},
		{name: "dashboard USD converts to CNY but stays unverified", hardLimit: "6", statusBody: `{"data":{"quota_per_unit":500000,"quota_display_type":"CNY","usd_exchange_rate":7}}`, wantAmount: 35, wantUnit: "CNY", wantScope: balanceScopeTokenOnly},
		{name: "unlimited token sentinel stays unverified", hardLimit: "100000000", statusBody: `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`, wantUnit: "USD", wantUnlimited: true, wantScope: balanceScopeTokenOnly},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/usage/token/":
					_, _ = io.WriteString(w, `{"data":{"unlimited_quota":true}}`)
				case "/api/status":
					_, _ = io.WriteString(w, test.statusBody)
				case "/v1/dashboard/billing/subscription":
					_, _ = io.WriteString(w, `{"hard_limit_usd":`+test.hardLimit+`}`)
				case "/v1/dashboard/billing/usage":
					_, _ = io.WriteString(w, `{"total_usage":100}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			app := newTestApplication(t, strategyPriority, time.Now,
				testAccount("a", server.URL+"/v1", "balance-key"),
			)
			balance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
			if balance.Status != "ok" || balance.Amount != test.wantAmount || balance.Unit != test.wantUnit || balance.Unlimited != test.wantUnlimited ||
				balance.Scope != test.wantScope {
				t.Fatalf("unexpected compatible unlimited balance: %#v", balance)
			}
		})
	}

	t.Run("dashboard fallback", func(t *testing.T) {
		var pathsMu sync.Mutex
		var paths []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pathsMu.Lock()
			paths = append(paths, r.URL.Path)
			pathsMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/usage/token/":
				http.NotFound(w, r)
			case "/v1/dashboard/billing/subscription":
				_, _ = io.WriteString(w, `{"hard_limit_usd":100}`)
			case "/v1/dashboard/billing/usage":
				if r.URL.Query().Get("start_date") == "" || r.URL.Query().Get("end_date") == "" {
					t.Fatal("billing usage dates are missing")
				}
				_, _ = io.WriteString(w, `{"total_usage":500}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		app := newTestApplication(t, strategyPriority, time.Now,
			testAccount("a", server.URL+"/v1", "balance-key"),
		)
		balance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
		if balance.Status != "ok" || balance.Amount != 95 || balance.Unit != "USD" ||
			balance.Scope != balanceScopeTokenOnly {
			t.Fatalf("unexpected dashboard balance: %#v paths=%v", balance, paths)
		}
	})

	t.Run("dashboard fallback unlimited sentinel stays unverified", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/usage/token/", "/api/status":
				http.NotFound(w, r)
			case "/v1/dashboard/billing/subscription":
				_, _ = io.WriteString(w, `{"hard_limit_usd":100000000}`)
			case "/v1/dashboard/billing/usage":
				_, _ = io.WriteString(w, `{"total_usage":0}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		app := newTestApplication(t, strategyPriority, time.Now,
			testAccount("a", server.URL+"/v1", "balance-key"),
		)
		balance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
		if balance.Status != "ok" || !balance.Unlimited || balance.Scope != balanceScopeTokenOnly {
			t.Fatalf("dashboard sentinel became an actual balance: %#v", balance)
		}
	})

	t.Run("dashboard raw quota is not treated as USD", func(t *testing.T) {
		var usageCalls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/usage/token/":
				http.NotFound(w, r)
			case "/v1/dashboard/billing/subscription":
				_, _ = io.WriteString(w, `{"total_available":1000000}`)
			case "/v1/dashboard/billing/usage":
				usageCalls.Add(1)
				_, _ = io.WriteString(w, `{"total_usage":0}`)
			case "/api/status":
				_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"CNY","usd_exchange_rate":7}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		app := newTestApplication(t, strategyPriority, time.Now,
			testAccount("a", server.URL+"/v1", "balance-key"),
		)
		balance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
		if balance.Status == "ok" || balance.Amount != 0 || balance.Unit != "" || usageCalls.Load() != 0 {
			t.Fatalf("raw dashboard quota was exposed as money: %#v usageCalls=%d", balance, usageCalls.Load())
		}
	})

	for name, usageBody := range map[string]string{
		"non JSON usage":  "not-json",
		"unmatched usage": `{"data":{"unexpected":1}}`,
	} {
		t.Run(name+" falls back to dashboard", func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/usage/token/":
					_, _ = io.WriteString(w, usageBody)
				case "/v1/dashboard/billing/subscription":
					_, _ = io.WriteString(w, `{"hard_limit_usd":50}`)
				case "/v1/dashboard/billing/usage":
					_, _ = io.WriteString(w, `{"total_usage":100}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			app := newTestApplication(t, strategyPriority, time.Now,
				testAccount("a", server.URL+"/v1", "balance-key"),
			)
			balance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
			if balance.Status != "ok" || balance.Amount != 49 || balance.Unit != "USD" {
				t.Fatalf("usage fallback failed: %#v", balance)
			}
		})
	}

	t.Run("unknown display unit is not comparable", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/usage/token/":
				_, _ = io.WriteString(w, `{"data":{"total_available":1000}}`)
			case "/api/status":
				_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500,"quota_display_type":1}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		app := newTestApplication(t, strategyPriority, time.Now,
			testAccount("a", server.URL+"/v1", "balance-key"),
		)
		balance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
		if balance.Status != balanceRefreshError || balance.Unit != "" || balance.Amount != 0 ||
			balance.ErrorStage != balanceStageQuotaMetadata {
			t.Fatalf("unknown unit was exposed as a readable balance: %#v", balance)
		}
	})

	t.Run("unsupported and error", func(t *testing.T) {
		statusCode := http.StatusNotFound
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(statusCode)
		}))
		defer server.Close()
		app := newTestApplication(t, strategyPriority, time.Now,
			testAccount("a", server.URL+"/v1", "balance-key"),
		)
		balance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
		if balance.Status != "unsupported" {
			t.Fatalf("404 status=%q, want unsupported", balance.Status)
		}
		statusCode = http.StatusUnauthorized
		balance = app.probeBalance(context.Background(), app.cfg.Accounts[0])
		if balance.Status != "auth_error" || balance.ErrorStage != balanceStageTokenUsage ||
			balance.ErrorCode != balanceErrorAPIKeyAuth {
			t.Fatalf("401 balance=%#v, want structured auth failure", balance)
		}
	})
}

func TestNewAPIAccessTokenUsesActualAccountBalance(t *testing.T) {
	t.Run("account funding preferences", func(t *testing.T) {
		for _, test := range []struct {
			name          string
			wallet        float64
			subscription  newAPISubscriptionBalance
			wantQuota     float64
			wantUnlimited bool
		}{
			{name: "wallet only", wallet: 100, subscription: newAPISubscriptionBalance{quota: 50, hasSubscriptions: true, preference: "wallet_only"}, wantQuota: 100},
			{name: "subscription only", wallet: 100, subscription: newAPISubscriptionBalance{quota: 50, hasSubscriptions: true, preference: "subscription_only"}, wantQuota: 50},
			{name: "wallet first combines", wallet: 100, subscription: newAPISubscriptionBalance{quota: 50, hasSubscriptions: true, preference: "wallet_first"}, wantQuota: 150},
			{name: "subscription first falls back to wallet without subscription", wallet: 100, subscription: newAPISubscriptionBalance{}, wantQuota: 100},
			{name: "subscription first combines when overflow is allowed", wallet: 100, subscription: newAPISubscriptionBalance{quota: 50, hasSubscriptions: true, allowWalletOverflow: true}, wantQuota: 150},
			{name: "subscription first blocks wallet overflow", wallet: 100, subscription: newAPISubscriptionBalance{quota: 50, hasSubscriptions: true}, wantQuota: 50},
			{name: "unlimited subscription", wallet: 100, subscription: newAPISubscriptionBalance{hasSubscriptions: true, unlimited: true, preference: "subscription_only"}, wantUnlimited: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				quota, unlimited, err := availableNewAPIAccountQuota(test.wallet, test.subscription)
				if err != nil || quota != test.wantQuota || unlimited != test.wantUnlimited {
					t.Fatalf("available quota=%v unlimited=%v err=%v", quota, unlimited, err)
				}
			})
		}
	})

	for _, test := range []struct {
		name           string
		tokenQuota     int
		tokenUnlimited bool
		wantAmount     float64
		wantLimitedBy  string
	}{
		{name: "unlimited token is limited by account", tokenUnlimited: true, wantAmount: 5, wantLimitedBy: "account"},
		{name: "finite token is the lower limit", tokenQuota: 1500000, wantAmount: 3, wantLimitedBy: "token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var selfCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/usage/token/":
					if r.Header.Get("Authorization") != "Bearer model-key" {
						t.Errorf("token usage authorization=%q", r.Header.Get("Authorization"))
					}
					_, _ = io.WriteString(w, `{"data":{"total_available":`+strconv.Itoa(test.tokenQuota)+
						`,"unlimited_quota":`+strconv.FormatBool(test.tokenUnlimited)+`}}`)
				case "/api/user/self":
					selfCalls.Add(1)
					if r.Header.Get("Authorization") != "Bearer access-token" ||
						r.Header.Get("New-Api-User") != "42" || r.Header.Get("Cookie") != "" {
						t.Errorf("self headers authorization=%q user=%q cookie=%q",
							r.Header.Get("Authorization"), r.Header.Get("New-Api-User"), r.Header.Get("Cookie"))
					}
					_, _ = io.WriteString(w, `{"success":true,"data":{"quota":2500000}}`)
				case "/api/status":
					if r.Header.Get("Authorization") != "Bearer model-key" {
						t.Errorf("status authorization=%q", r.Header.Get("Authorization"))
					}
					_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			account := testAccount("a", server.URL+"/v1", "model-key")
			account.NewAPIAuthMode = newAPIAuthAccessToken
			account.NewAPIUserID = 42
			account.NewAPISecret = "access-token"
			app := newTestApplication(t, strategyPriority, time.Now, account)
			balance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
			if balance.Status != "ok" || balance.Amount != test.wantAmount || balance.Unit != "USD" ||
				balance.Unlimited || balance.Scope != balanceScopeActual || balance.LimitedBy != test.wantLimitedBy ||
				selfCalls.Load() != 1 {
				t.Fatalf("unexpected actual balance: %#v selfCalls=%d", balance, selfCalls.Load())
			}
		})
	}

	t.Run("wallet and subscriptions are capped by API key quota", func(t *testing.T) {
		var subscriptionCalls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/usage/token/":
				_, _ = io.WriteString(w, `{"data":{"total_available":4000000,"unlimited_quota":false}}`)
			case "/api/user/self":
				_, _ = io.WriteString(w, `{"success":true,"data":{"quota":2000000}}`)
			case "/api/subscription/self":
				subscriptionCalls.Add(1)
				if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("New-Api-User") != "42" {
					t.Errorf("subscription authorization=%q user=%q", r.Header.Get("Authorization"), r.Header.Get("New-Api-User"))
				}
				_, _ = io.WriteString(w, `{"success":true,"data":{"billing_preference":"wallet_first","subscriptions":[{"subscription":{"amount_total":3000000,"amount_used":500000,"next_reset_time":1784044800,"allow_wallet_overflow":true}},{"subscription":{"amount_total":1000000,"amount_used":500000,"next_reset_time":1784131200,"allow_wallet_overflow":true}}]}}`)
			case "/api/status":
				_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()

		account := testAccount("a", server.URL+"/v1", "model-key")
		account.NewAPIAuthMode = newAPIAuthAccessToken
		account.NewAPIUserID = 42
		account.NewAPISecret = "access-token"
		app := newTestApplication(t, strategyPriority, time.Now, account)
		balance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
		if balance.Status != "ok" || balance.Amount != 8 || balance.Unit != "USD" || balance.Unlimited ||
			balance.Scope != balanceScopeActual || balance.LimitedBy != "token" || subscriptionCalls.Load() != 1 {
			t.Fatalf("unexpected combined balance: %#v subscriptionCalls=%d", balance, subscriptionCalls.Load())
		}
		public := publicBalanceAt(balance, time.Now())
		if public.Subscription == nil || public.Subscription.Total != 8 || public.Subscription.Remaining != 6 ||
			public.Subscription.Unit != "USD" || public.Subscription.Unlimited ||
			public.Subscription.ResetAt != "2026-07-14T16:00:00Z" {
			t.Fatalf("unexpected subscription quota: %#v", public.Subscription)
		}
	})

	t.Run("dashboard fallback unlimited token is limited by account", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/usage/token/":
				http.NotFound(w, r)
			case "/v1/dashboard/billing/subscription":
				_, _ = io.WriteString(w, `{"hard_limit_usd":100000000}`)
			case "/v1/dashboard/billing/usage":
				_, _ = io.WriteString(w, `{"total_usage":0}`)
			case "/api/status":
				_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
			case "/api/user/self":
				if r.Header.Get("Authorization") != "Bearer access-token" || r.Header.Get("New-Api-User") != "42" {
					t.Errorf("self authorization=%q user=%q", r.Header.Get("Authorization"), r.Header.Get("New-Api-User"))
				}
				_, _ = io.WriteString(w, `{"success":true,"data":{"quota":2500000}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()

		account := testAccount("a", server.URL+"/v1", "model-key")
		account.NewAPIAuthMode = newAPIAuthAccessToken
		account.NewAPIUserID = 42
		account.NewAPISecret = "access-token"
		app := newTestApplication(t, strategyPriority, time.Now, account)
		balance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
		if balance.Status != "ok" || balance.Amount != 5 || balance.Unit != "USD" || balance.Unlimited ||
			balance.Scope != balanceScopeActual || balance.LimitedBy != "account" {
			t.Fatalf("dashboard fallback did not use actual account quota: %#v", balance)
		}
	})

	t.Run("dashboard USD is converted before account comparison", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/usage/token/":
				http.NotFound(w, r)
			case "/v1/dashboard/billing/subscription":
				_, _ = io.WriteString(w, `{"hard_limit_usd":10}`)
			case "/v1/dashboard/billing/usage":
				_, _ = io.WriteString(w, `{"total_usage":0}`)
			case "/api/status":
				_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"CNY","usd_exchange_rate":7}}`)
			case "/api/user/self":
				_, _ = io.WriteString(w, `{"success":true,"data":{"quota":50000000}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()

		account := testAccount("a", server.URL+"/v1", "model-key")
		account.NewAPIAuthMode = newAPIAuthAccessToken
		account.NewAPIUserID = 42
		account.NewAPISecret = "access-token"
		app := newTestApplication(t, strategyPriority, time.Now, account)
		balance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
		if balance.Status != "ok" || balance.Amount != 70 || balance.Unit != "CNY" || balance.Unlimited ||
			balance.Scope != balanceScopeActual || balance.LimitedBy != "token" {
			t.Fatalf("dashboard USD was not converted before comparison: %#v", balance)
		}
	})
}

func TestNewAPIPasswordSessionsAreIsolatedPerAccount(t *testing.T) {
	var aliceLogins atomic.Int32
	var bobLogins atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/usage/token/":
			if authorization := r.Header.Get("Authorization"); authorization != "Bearer model-alice" && authorization != "Bearer model-bob" {
				t.Errorf("token usage authorization=%q", authorization)
			}
			_, _ = io.WriteString(w, `{"data":{"total_available":0,"unlimited_quota":true}}`)
		case "/api/user/login":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Cookie") != "" {
				t.Errorf("login method=%s content-type=%q cookie=%q", r.Method, r.Header.Get("Content-Type"), r.Header.Get("Cookie"))
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode login body: %v", err)
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			var userID int
			var expectedPassword string
			var session string
			switch body["username"] {
			case "alice":
				aliceLogins.Add(1)
				userID, expectedPassword, session = 11, "alice-password", "alice-session"
			case "bob":
				bobLogins.Add(1)
				userID, expectedPassword, session = 22, "bob-password", "bob-session"
			default:
				t.Errorf("unexpected login body: %#v", body)
				http.Error(w, "unknown user", http.StatusUnauthorized)
				return
			}
			if len(body) != 2 || body["password"] != expectedPassword {
				t.Errorf("wrong login body for %q: %#v", body["username"], body)
				http.Error(w, "wrong password", http.StatusUnauthorized)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: session, Path: "/", HttpOnly: true})
			_, _ = io.WriteString(w, `{"success":true,"data":{"id":`+strconv.Itoa(userID)+`}}`)
		case "/api/user/self":
			var expectedCookie string
			var quota int
			switch r.Header.Get("New-Api-User") {
			case "11":
				expectedCookie, quota = "session=alice-session", 2500000
			case "22":
				expectedCookie, quota = "session=bob-session", 1500000
			default:
				t.Errorf("unexpected self user header=%q", r.Header.Get("New-Api-User"))
				http.Error(w, "bad user", http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Cookie") != expectedCookie || r.Header.Get("Authorization") != "" {
				t.Errorf("self user=%q cookie=%q authorization=%q",
					r.Header.Get("New-Api-User"), r.Header.Get("Cookie"), r.Header.Get("Authorization"))
				http.Error(w, "wrong session", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"success":true,"data":{"quota":`+strconv.Itoa(quota)+`}}`)
		case "/api/status":
			_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	alice := testAccount("alice", server.URL+"/v1", "model-alice")
	alice.NewAPIAuthMode, alice.NewAPIUsername, alice.NewAPISecret = newAPIAuthPassword, "alice", "alice-password"
	bob := testAccount("bob", server.URL+"/v1", "model-bob")
	bob.NewAPIAuthMode, bob.NewAPIUsername, bob.NewAPISecret = newAPIAuthPassword, "bob", "bob-password"
	app := newTestApplication(t, strategyPriority, time.Now, alice, bob)

	aliceBalance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
	bobBalance := app.probeBalance(context.Background(), app.cfg.Accounts[1])
	aliceAgain := app.probeBalance(context.Background(), app.cfg.Accounts[0])
	if aliceBalance.Amount != 5 || bobBalance.Amount != 3 || aliceAgain.Amount != 5 ||
		aliceLogins.Load() != 1 || bobLogins.Load() != 1 ||
		app.runtime["alice"].NewAPISession != "session=alice-session" ||
		app.runtime["bob"].NewAPISession != "session=bob-session" {
		t.Fatalf("password sessions crossed accounts: amounts=%v/%v/%v logins=%d/%d",
			aliceBalance.Amount, bobBalance.Amount, aliceAgain.Amount, aliceLogins.Load(), bobLogins.Load())
	}
}

func TestNewAPILoginRejectsOutOfRangeUserID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "session-value", Path: "/"})
		_, _ = io.WriteString(w, `{"success":true,"data":{"id":9223372036854775808}}`)
	}))
	defer server.Close()
	account := testAccount("a", server.URL+"/v1", "model-key")
	account.NewAPIAuthMode, account.NewAPIUsername, account.NewAPISecret = newAPIAuthPassword, "user", "password"
	app := newTestApplication(t, strategyPriority, time.Now, account)
	if _, _, err := app.loginNewAPI(context.Background(), account); err == nil {
		t.Fatal("out-of-range New API user ID was accepted")
	}
}

func TestNewAPIAuthFailureIsTokenOnlyAndDisablesHighestBalance(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	var selfStatus atomic.Int32
	selfStatus.Store(http.StatusUnauthorized)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/usage/token/":
			_, _ = io.WriteString(w, `{"data":{"total_available":50000000,"unlimited_quota":true}}`)
		case "/api/user/self":
			w.WriteHeader(int(selfStatus.Load()))
			_, _ = io.WriteString(w, `{"success":false,"message":"invalid access token"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	failing := testAccount("failing", server.URL+"/v1", "model-key")
	failing.NewAPIAuthMode = newAPIAuthAccessToken
	failing.NewAPIUserID = 42
	failing.NewAPISecret = "bad-access-token"
	good := testAccount("good", "https://good.invalid/v1", "good-key")
	app := newTestApplication(t, strategyHighestBalance, func() time.Time { return now }, failing, good)
	failingBalance := app.probeBalance(context.Background(), app.cfg.Accounts[0])
	if failingBalance.Status != "auth_error" || failingBalance.Scope != balanceScopeTokenOnly {
		t.Fatalf("authentication failure balance=%#v", failingBalance)
	}
	selfStatus.Store(http.StatusInternalServerError)
	transientFailure := app.probeBalance(context.Background(), app.cfg.Accounts[0])
	if transientFailure.Status != "error" {
		t.Fatalf("transient upstream failure was misclassified: %#v", transientFailure)
	}
	app.runtime["failing"].Balance = failingBalance
	app.runtime["failing"].AssignedRequests = 10
	app.runtime["good"].Balance = balanceSnapshot{
		Status: "ok", Amount: 5, Unit: "USD", Scope: balanceScopeActual, LimitedBy: "account", UpdatedAt: now,
	}
	status := app.status()
	selected, ok := app.selectAccount(context.Background(), map[string]bool{})
	if status.EffectiveStrategy != strategyLeastUsed || status.FallbackReason != "balance_unavailable" ||
		!ok || selected.ID != "good" {
		t.Fatalf("auth-only balance participated in highest balance: status=%#v selected=%q ok=%v", status, selected.ID, ok)
	}
}

func TestNumberValueRejectsNonFiniteValues(t *testing.T) {
	for _, value := range []any{"NaN", "+Inf", math.NaN(), math.Inf(1)} {
		if number, ok := numberValue(value); ok {
			t.Fatalf("numberValue(%v)=%v, want rejected", value, number)
		}
	}
}

func TestConvertNewAPIQuotaDisplayTypes(t *testing.T) {
	for _, test := range []struct {
		name       string
		status     map[string]any
		wantAmount float64
		wantUnit   string
		wantLabel  string
	}{
		{name: "USD", status: map[string]any{"quota_display_type": "USD", "quota_per_unit": 500000.0}, wantAmount: 2, wantUnit: "USD", wantLabel: "USD"},
		{name: "CNY", status: map[string]any{"quota_display_type": "CNY", "quota_per_unit": 500000.0, "usd_exchange_rate": 7.0}, wantAmount: 14, wantUnit: "CNY", wantLabel: "CNY"},
		{name: "TOKENS", status: map[string]any{"quota_display_type": "TOKENS"}, wantAmount: 1000000, wantUnit: "TOKENS", wantLabel: "TOKENS"},
		{name: "CUSTOM", status: map[string]any{"quota_display_type": "CUSTOM", "quota_per_unit": 500000.0, "custom_currency_exchange_rate": 2.0, "custom_currency_symbol": "点"}, wantAmount: 4, wantUnit: "CUSTOM:点:2", wantLabel: "点"},
	} {
		t.Run(test.name, func(t *testing.T) {
			amount, unit, label, ok := convertNewAPIQuota(1000000, test.status)
			if !ok || amount != test.wantAmount || unit != test.wantUnit || label != test.wantLabel {
				t.Fatalf("conversion=%v %q %q ok=%v", amount, unit, label, ok)
			}
		})
	}
	if _, _, _, ok := convertNewAPIQuota(1000000, map[string]any{
		"quota_display_type": "CNY", "quota_per_unit": 500000.0,
	}); ok {
		t.Fatal("CNY conversion without exchange rate was accepted")
	}
}

func TestBalanceStrategiesRefreshStaleAccountsBeforeSelection(t *testing.T) {
	var usageCalls atomic.Int32
	var selfCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/usage/token/":
			usageCalls.Add(1)
			amount := 500000
			if r.Header.Get("Authorization") == "Bearer key-b" {
				amount = 1500000
			}
			_, _ = io.WriteString(w, `{"data":{"total_available":`+strconv.Itoa(amount)+`}}`)
		case "/api/user/self":
			selfCalls.Add(1)
			expectedToken := "Bearer access-a"
			if r.Header.Get("New-Api-User") == "22" {
				expectedToken = "Bearer access-b"
			}
			if r.Header.Get("Authorization") != expectedToken ||
				(r.Header.Get("New-Api-User") != "11" && r.Header.Get("New-Api-User") != "22") {
				t.Errorf("self authorization=%q user=%q", r.Header.Get("Authorization"), r.Header.Get("New-Api-User"))
			}
			_, _ = io.WriteString(w, `{"success":true,"data":{"quota":10000000}}`)
		case "/api/subscription/self":
			_, _ = io.WriteString(w, `{"success":true,"data":{"billing_preference":"subscription_first","subscriptions":[]}}`)
		case "/api/status":
			_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))
	defer server.Close()
	first := testAccount("a", server.URL+"/v1", "key-a")
	first.NewAPIAuthMode, first.NewAPIUserID, first.NewAPISecret = newAPIAuthAccessToken, 11, "access-a"
	second := testAccount("b", server.URL+"/v1", "key-b")
	second.NewAPIAuthMode, second.NewAPIUserID, second.NewAPISecret = newAPIAuthAccessToken, 22, "access-b"
	app := newTestApplication(t, strategyHighestBalance, time.Now,
		first, second,
	)
	account, ok := app.selectAccount(context.Background(), map[string]bool{})
	if !ok || account.ID != "b" || usageCalls.Load() != 2 || selfCalls.Load() != 2 {
		t.Fatalf("highest balance selection=%q ok=%v usageCalls=%d selfCalls=%d",
			account.ID, ok, usageCalls.Load(), selfCalls.Load())
	}
	if app.status().EffectiveStrategy != strategyHighestBalance {
		t.Fatalf("fresh comparable balances did not enable highest_balance: %#v", app.status())
	}

	app.cfg.Strategy = strategyLowestBalance
	app.runtime["a"].Balance = balanceSnapshot{}
	app.runtime["b"].Balance = balanceSnapshot{}
	usageCalls.Store(0)
	selfCalls.Store(0)
	account, ok = app.selectAccount(context.Background(), map[string]bool{})
	if !ok || account.ID != "a" || usageCalls.Load() != 2 || selfCalls.Load() != 2 {
		t.Fatalf("lowest balance selection=%q ok=%v usageCalls=%d selfCalls=%d",
			account.ID, ok, usageCalls.Load(), selfCalls.Load())
	}
	if app.status().EffectiveStrategy != strategyLowestBalance {
		t.Fatalf("fresh comparable balances did not enable lowest_balance: %#v", app.status())
	}
}

func TestBalanceRefreshContextRespectsParentCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	ctx, cancel := newBalanceRefreshContext(parent)
	defer cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("balance context error=%v, want canceled", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("balance context ignored parent cancellation")
	}
}

func TestAutomaticBalanceRefreshSkipsFreshAndDisabledAccounts(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	enabled := testAccount("a", "https://gateway.invalid/v1", "key-a")
	disabled := testAccount("b", "https://gateway.invalid/v1", "key-b")
	disabled.Enabled = false
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, enabled, disabled)
	app.runtime["a"].Balance = balanceSnapshot{Status: "ok", Amount: 1, Unit: "USD", UpdatedAt: now}
	var calls atomic.Int32
	var disabledCalls atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Header.Get("Authorization") == "Bearer key-b" {
			disabledCalls.Add(1)
		}
		body := `{"data":{"total_available":1000000}}`
		if r.URL.Path == "/api/status" {
			body = `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})}

	response := adminJSON(app, http.MethodPost, "/admin/balances/refresh?automatic=1", map[string]any{}, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || calls.Load() != 0 {
		t.Fatalf("fresh automatic refresh status=%d calls=%d", response.Code, calls.Load())
	}

	app.runtime["a"].Balance.UpdatedAt = now.Add(-balanceAutoTTL - time.Second)
	response = adminJSON(app, http.MethodPost, "/admin/balances/refresh?automatic=1", map[string]any{}, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || calls.Load() != 2 || disabledCalls.Load() != 0 || app.runtime["a"].Balance.Amount != 2 {
		t.Fatalf("automatic refresh status=%d calls=%d disabled=%d balance=%#v", response.Code, calls.Load(), disabledCalls.Load(), app.runtime["a"].Balance)
	}
}

func TestAutomaticBalanceRefreshOrdersOldestAccountsFirst(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	accounts := []accountConfig{
		testAccount("recent", "https://recent.invalid/v1", "key-recent"),
		testAccount("never", "https://never.invalid/v1", "key-never"),
		testAccount("old", "https://old.invalid/v1", "key-old"),
	}
	sortAccountsByBalanceAge(accounts, map[string]time.Time{
		"recent": now.Add(-2 * time.Minute),
		"old":    now.Add(-time.Hour),
	})
	want := []string{"never", "old", "recent"}
	for index, id := range want {
		if accounts[index].ID != id {
			t.Fatalf("sorted account %d=%q, want %q", index, accounts[index].ID, id)
		}
	}
}

func TestCanceledBalanceRefreshKeepsLastSuccessfulValue(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://a.invalid/v1", "key-a"),
	)
	previous := balanceSnapshot{Status: "ok", Amount: 9, Unit: "USD", UpdatedAt: now.Add(-time.Hour)}
	app.runtime["a"].Balance = previous
	entered := make(chan struct{})
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := ""
		switch r.URL.Path {
		case "/api/usage/token/":
			status = http.StatusNotFound
			body = "not found"
		case "/v1/dashboard/billing/subscription":
			body = `{"hard_limit_usd":10}`
		case "/v1/dashboard/billing/usage":
			body = `{"total_usage":0}`
		case "/api/status":
			close(entered)
			<-r.Context().Done()
			return nil, r.Context().Err()
		default:
			return nil, errors.New("unexpected balance path: " + r.URL.Path)
		}
		return &http.Response{
			StatusCode: status, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		app.refreshBalances(ctx, nil, 0)
		close(done)
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled refresh did not finish")
	}
	if app.runtime["a"].Balance != previous {
		t.Fatalf("canceled refresh replaced balance: %#v", app.runtime["a"].Balance)
	}
}

func TestBalanceRefreshHasConcurrencyLimit(t *testing.T) {
	accounts := make([]accountConfig, 0, balanceWorkers+4)
	for index := 0; index < balanceWorkers+4; index++ {
		id := "a" + strconv.Itoa(index)
		accounts = append(accounts, testAccount(id, "https://"+id+".invalid/v1", "key-"+id))
	}
	app := newTestApplication(t, strategyPriority, time.Now, accounts...)
	var active atomic.Int32
	var maximum atomic.Int32
	entered := make(chan struct{}, len(accounts)*balanceProbeParallelism*2)
	release := make(chan struct{})
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		return &http.Response{
			StatusCode: http.StatusNotFound, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("not found")), Request: r,
		}, nil
	})}
	done := make(chan struct{}, 2)
	for refresh := 0; refresh < 2; refresh++ {
		go func() {
			app.refreshBalances(context.Background(), nil, 0)
			done <- struct{}{}
		}()
	}
	initialParallelRequests := balanceWorkers * 2
	for index := 0; index < initialParallelRequests; index++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("parallel balance stages did not start")
		}
	}
	select {
	case <-entered:
		t.Fatal("balance refresh started another account before a worker was free")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for refresh := 0; refresh < 2; refresh++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("balance refresh did not finish")
		}
	}
	if maximum.Load() > balanceWorkers*balanceProbeParallelism {
		t.Fatalf("maximum balance concurrency=%d, want <=%d", maximum.Load(), balanceWorkers*balanceProbeParallelism)
	}
}

func TestNewAPIAccessTokenBalanceStagesRunInParallel(t *testing.T) {
	entered := make(chan string, balanceProbeParallelism)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.URL.Path
		<-release
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/usage/token/":
			_, _ = io.WriteString(w, `{"data":{"total_available":0,"unlimited_quota":true}}`)
		case "/api/user/self":
			_, _ = io.WriteString(w, `{"success":true,"data":{"quota":2500000}}`)
		case "/api/status":
			_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	account := testAccount("a", server.URL+"/v1", "model-key")
	account.NewAPIAuthMode = newAPIAuthAccessToken
	account.NewAPIUserID = 42
	account.NewAPISecret = "access-token"
	app := newTestApplication(t, strategyPriority, time.Now, account)
	result := make(chan balanceSnapshot, 1)
	go func() { result <- app.probeBalance(context.Background(), account) }()

	seen := make(map[string]bool)
	for len(seen) < balanceProbeParallelism {
		select {
		case path := <-entered:
			seen[path] = true
		case <-time.After(time.Second):
			t.Fatalf("balance stages did not start in parallel: %#v", seen)
		}
	}
	releaseAll()
	select {
	case balance := <-result:
		if balance.Status != "ok" || balance.RefreshStatus != balanceRefreshOK || balance.Amount != 5 ||
			balance.Scope != balanceScopeActual || balance.LimitedBy != "account" {
			t.Fatalf("parallel balance=%#v", balance)
		}
	case <-time.After(time.Second):
		t.Fatal("parallel balance probe did not finish")
	}
}

func TestTokenUsageSuccessFalseIsAPIKeyAuthenticationFailure(t *testing.T) {
	var dashboardCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/usage/token/":
			_, _ = io.WriteString(w, `{"success":false,"message":"invalid token"}`)
		case "/api/status":
			_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
		case "/dashboard/billing/subscription", "/dashboard/billing/usage":
			dashboardCalls.Add(1)
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	account := testAccount("a", server.URL+"/v1", "bad-model-key")
	app := newTestApplication(t, strategyPriority, time.Now, account)
	balance := app.probeBalance(context.Background(), account)
	if balance.Status != balanceRefreshAuthError || balance.ErrorStage != balanceStageTokenUsage ||
		balance.ErrorCode != balanceErrorAPIKeyAuth || dashboardCalls.Load() != 0 {
		t.Fatalf("token authentication failure=%#v dashboardCalls=%d", balance, dashboardCalls.Load())
	}
}

func TestAccountOnlyPartialUsesDisplayConversion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/usage/token/":
			http.Error(w, "temporary", http.StatusServiceUnavailable)
		case "/api/user/self":
			_, _ = io.WriteString(w, `{"success":true,"data":{"quota":2500000}}`)
		case "/api/status":
			_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	account := testAccount("a", server.URL+"/v1", "model-key")
	account.NewAPIAuthMode = newAPIAuthAccessToken
	account.NewAPIUserID = 42
	account.NewAPISecret = "access-token"
	app := newTestApplication(t, strategyPriority, time.Now, account)
	balance := app.probeBalance(context.Background(), account)
	if balance.Status != "ok" || balance.RefreshStatus != balanceRefreshPartial ||
		balance.Scope != balanceScopeAccountOnly || balance.Amount != 5 || balance.Unit != "USD" {
		t.Fatalf("account-only partial was not converted safely: %#v", balance)
	}
}

func TestMetadataFailureDoesNotExposeRawQuotaAsMoney(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/usage/token/":
			_, _ = io.WriteString(w, `{"data":{"total_available":0,"unlimited_quota":true}}`)
		case "/api/user/self":
			_, _ = io.WriteString(w, `{"success":true,"data":{"quota":395732745}}`)
		case "/api/status":
			http.Error(w, "temporary", http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	account := testAccount("a", server.URL+"/v1", "model-key")
	account.NewAPIAuthMode = newAPIAuthAccessToken
	account.NewAPIUserID = 131
	account.NewAPISecret = "access-token"
	app := newTestApplication(t, strategyPriority, time.Now, account)
	balance := app.probeBalance(context.Background(), account)
	if balance.Status != balanceRefreshError || balance.ErrorStage != balanceStageQuotaMetadata ||
		balance.Amount != 0 || !balance.UpdatedAt.IsZero() {
		t.Fatalf("raw New API quota was exposed without conversion metadata: %#v", balance)
	}
}

func TestInvalidMetadataDoesNotExposeRawQuotaAsMoney(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/usage/token/":
			_, _ = io.WriteString(w, `{"data":{"total_available":1000000}}`)
		case "/api/status":
			_, _ = io.WriteString(w, `{"data":{"quota_display_type":"USD"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	account := testAccount("a", server.URL+"/v1", "model-key")
	app := newTestApplication(t, strategyPriority, time.Now, account)
	balance := app.probeBalance(context.Background(), account)
	if balance.Status != balanceRefreshError || balance.ErrorStage != balanceStageQuotaMetadata ||
		balance.Amount != 0 || !balance.UpdatedAt.IsZero() {
		t.Fatalf("raw quota was exposed after metadata conversion failed: %#v", balance)
	}
}

func TestBalanceRefreshUsesIndependentAccountTimeouts(t *testing.T) {
	slow := testAccount("slow", "https://slow.invalid/v1", "slow-key")
	fast := testAccount("fast", "https://fast.invalid/v1", "fast-key")
	app := newTestApplication(t, strategyPriority, time.Now, slow, fast)
	app.balanceTimeout = 40 * time.Millisecond
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "slow.invalid" {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		body := `{"data":{"total_available":1000000}}`
		if r.URL.Path == "/api/status" {
			body = `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})}
	reports := app.refreshBalances(context.Background(), nil, 0)
	if len(reports) != 2 {
		t.Fatalf("reports=%#v", reports)
	}
	if app.runtime["fast"].Balance.Status != "ok" || app.runtime["fast"].Balance.Amount != 2 {
		t.Fatalf("fast account lost to slow timeout: %#v", app.runtime["fast"].Balance)
	}
	if app.runtime["slow"].Balance.RefreshStatus != balanceRefreshError ||
		app.runtime["slow"].Balance.ErrorCode != balanceErrorTimeout || !app.runtime["slow"].Balance.Retryable {
		t.Fatalf("slow timeout was not classified safely: %#v", app.runtime["slow"].Balance)
	}
}

func TestBalanceRefreshCanTargetRetryAccounts(t *testing.T) {
	first := testAccount("a", "https://a.invalid/v1", "key-a")
	second := testAccount("b", "https://b.invalid/v1", "key-b")
	app := newTestApplication(t, strategyPriority, time.Now, first, second)
	var firstUsage atomic.Int32
	var secondUsage atomic.Int32
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/usage/token/" {
			if r.URL.Host == "a.invalid" {
				firstUsage.Add(1)
			} else if r.URL.Host == "b.invalid" {
				secondUsage.Add(1)
			}
		}
		body := `{"data":{"total_available":1000000}}`
		if r.URL.Path == "/api/status" {
			body = `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})}
	response := adminJSON(app, http.MethodPost, "/admin/balances/refresh", balanceRefreshRequest{
		AccountIDs: []string{"a"},
	}, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || firstUsage.Load() != 1 || secondUsage.Load() != 0 ||
		!strings.Contains(response.Body.String(), `"accountId":"a"`) ||
		strings.Contains(response.Body.String(), `"accountId":"b"`) {
		t.Fatalf("targeted refresh status=%d first=%d second=%d body=%s",
			response.Code, firstUsage.Load(), secondUsage.Load(), response.Body.String())
	}
}

func TestBalanceAttemptPreservesLastGoodAndInvalidatesOnAuthFailure(t *testing.T) {
	now := time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)
	previous := balanceSnapshot{
		Status: "ok", Amount: 7, Unit: "USD", Scope: balanceScopeActual, LimitedBy: "account",
		UpdatedAt: now, CheckedAt: now, RefreshStatus: balanceRefreshOK,
	}
	transient := balanceAttemptFromFailure(now.Add(time.Second), balanceFailure{
		Status: balanceRefreshError, Stage: balanceStageAccountQuota, Code: balanceErrorUpstream, Retryable: true,
	})
	merged := mergeBalanceAttempt(previous, transient)
	if merged.Status != "ok" || merged.Amount != 7 || merged.UpdatedAt != now ||
		merged.RefreshStatus != balanceRefreshError || merged.ErrorCode != balanceErrorUpstream || merged.NextRetryAt.IsZero() {
		t.Fatalf("transient failure replaced last good: %#v", merged)
	}
	partial := previous
	partial.Amount = 9000000
	partial.Unit = ""
	partial.UpdatedAt = now.Add(time.Second)
	partial.CheckedAt = now.Add(time.Second)
	partial.RefreshStatus = balanceRefreshPartial
	partial.ErrorStage = balanceStageQuotaMetadata
	partial.ErrorCode = balanceErrorUpstream
	partial.Retryable = true
	mergedPartial := mergeBalanceAttempt(previous, partial)
	if mergedPartial.Amount != previous.Amount || mergedPartial.Unit != previous.Unit ||
		mergedPartial.UpdatedAt != previous.UpdatedAt || mergedPartial.Status != "ok" ||
		mergedPartial.RefreshStatus != balanceRefreshPartial {
		t.Fatalf("partial refresh replaced the last fully converted value: %#v", mergedPartial)
	}
	permanentPartial := partial
	permanentPartial.Retryable = false
	if invalid := mergeBalanceAttempt(previous, permanentPartial); invalid.Status == "ok" {
		t.Fatalf("non-retryable partial kept a stale value routable: %#v", invalid)
	}
	fatalPartial := mergeBalanceAttempt(previous, permanentPartial)
	counts := countBalanceReports([]balanceRefreshReport{{Balance: publicBalanceAt(fatalPartial, now)}})
	if counts.Failed != 1 || counts.Partial != 0 {
		t.Fatalf("fatal partial was reported as partial success: %#v", counts)
	}
	counts = countBalanceReports([]balanceRefreshReport{{Balance: publicBalanceAt(mergedPartial, now)}})
	if counts.Partial != 1 || counts.Failed != 0 {
		t.Fatalf("retryable partial was not reported as partial success: %#v", counts)
	}
	permanent := balanceAttemptFromFailure(now.Add(time.Second), balanceFailure{
		Status: balanceRefreshError, Stage: balanceStageAccountQuota, Code: balanceErrorMissingQuota,
	})
	if invalid := mergeBalanceAttempt(previous, permanent); invalid.Status == "ok" {
		t.Fatalf("non-retryable failure kept a stale value routable: %#v", invalid)
	}
	authPrevious := previous
	authPrevious.Status = balanceRefreshAuthError
	authPrevious.ErrorCode = balanceErrorAPIKeyAuth
	partial.Scope = balanceScopeAccountOnly
	partial.ErrorStage = balanceStageTokenUsage
	unresolved := mergeBalanceAttempt(authPrevious, partial)
	if unresolved.Status != balanceRefreshAuthError {
		t.Fatalf("account-only partial incorrectly cleared API Key authentication failure: %#v", unresolved)
	} else if unresolved.RefreshStatus != balanceRefreshAuthError || unresolved.ErrorCode != balanceErrorAPIKeyAuth {
		t.Fatalf("unresolved API Key authentication failure lost its source: %#v", unresolved)
	}
	tokenProvenPartial := partial
	tokenProvenPartial.Scope = balanceScopeTokenOnly
	tokenProvenPartial.ErrorStage = balanceStageQuotaMetadata
	if stillInvalid := mergeBalanceAttempt(unresolved, tokenProvenPartial); stillInvalid.Status != balanceRefreshAuthError {
		t.Fatalf("partial refresh cleared a fatal state without a complete success: %#v", stillInvalid)
	}
	unsupportedPrevious := previous
	unsupportedPrevious.Status = balanceRefreshUnsupported
	unsupportedPrevious.RefreshStatus = balanceRefreshUnsupported
	unsupportedPrevious.ErrorCode = balanceErrorUnsupported
	if stillInvalid := mergeBalanceAttempt(unsupportedPrevious, partial); stillInvalid.Status != balanceRefreshUnsupported {
		t.Fatalf("partial refresh cleared an unsupported state without a complete success: %#v", stillInvalid)
	}
	auth := balanceAttemptFromFailure(now.Add(2*time.Second), balanceFailure{
		Status: balanceRefreshAuthError, Stage: balanceStageAccountQuota, Code: balanceErrorAccountAuth,
	})
	merged = mergeBalanceAttempt(merged, auth)
	if merged.Status != balanceRefreshAuthError || merged.Amount != 7 || merged.RefreshStatus != balanceRefreshAuthError {
		t.Fatalf("auth failure did not invalidate preserved value: %#v", merged)
	}
	account := testAccount("a", "https://a.invalid/v1", "key")
	app := newTestApplication(t, strategyHighestBalance, func() time.Time { return now.Add(2 * time.Second) }, account)
	app.runtime["a"].Balance = merged
	status := app.status()
	if status.EffectiveStrategy != strategyLeastUsed || status.FallbackReason != "balance_unavailable" {
		t.Fatalf("auth-invalid balance participated in routing: %#v", status)
	}
}

func TestApplyBalanceRejectsOlderProbeResult(t *testing.T) {
	now := time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)
	account := testAccount("a", "https://a.invalid/v1", "key")
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	newer := balanceSnapshot{
		Status: "ok", Amount: 9, Unit: "USD", Scope: balanceScopeActual,
		UpdatedAt: now.Add(2 * time.Second), CheckedAt: now.Add(2 * time.Second), RefreshStatus: balanceRefreshOK,
	}
	app.runtime["a"].Balance = newer
	older := balanceSnapshot{
		Status: "ok", Amount: 1, Unit: "USD", Scope: balanceScopeActual,
		UpdatedAt: now.Add(time.Second), CheckedAt: now.Add(time.Second), RefreshStatus: balanceRefreshOK,
	}
	if current, applied := app.applyBalance(account, older); applied || current.Amount != newer.Amount ||
		app.runtime["a"].Balance.Amount != newer.Amount {
		t.Fatalf("older probe overwrote newer balance: applied=%v current=%#v runtime=%#v",
			applied, current, app.runtime["a"].Balance)
	}
}

func TestBalanceTestIsIndependentAndDoesNotLeakSecrets(t *testing.T) {
	var modelCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses":
			modelCalls.Add(1)
			http.Error(w, "model should not be called", http.StatusInternalServerError)
		case "/api/usage/token/":
			_, _ = io.WriteString(w, `{"data":{"total_available":0,"unlimited_quota":true}}`)
		case "/api/status":
			_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
		case "/api/user/self":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"success":false,"message":"New-Api-User does not match token user access-secret model-secret"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	account := testAccount("a", server.URL+"/v1", "model-secret")
	account.NewAPIAuthMode = newAPIAuthAccessToken
	account.NewAPIUserID = 42
	account.NewAPISecret = "access-secret"
	app := newTestApplication(t, strategyPriority, time.Now, account)
	response := adminJSON(app, http.MethodPost, "/admin/balances/test", accountProbeRequest{AccountID: "a"}, "http://127.0.0.1:4000")
	body := response.Body.String()
	if response.Code != http.StatusOK || modelCalls.Load() != 0 ||
		!strings.Contains(body, `"errorStage":"account_quota"`) ||
		!strings.Contains(body, `"errorCode":"user_id_mismatch"`) {
		t.Fatalf("independent balance test failed: status=%d calls=%d body=%s", response.Code, modelCalls.Load(), body)
	}
	if strings.Contains(body, "access-secret") || strings.Contains(body, "model-secret") || strings.Contains(body, "message") {
		t.Fatalf("balance test leaked upstream secrets/body: %s", body)
	}
}

func TestAccessTokenAuthErrorCode(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{name: "missing user id", body: `{"message":"New-Api-User header not provided"}`, want: balanceErrorUserIDRequired},
		{name: "mismatched user id", body: `{"message":"New-Api-User does not match token user"}`, want: balanceErrorUserIDMismatch},
		{name: "invalid access token", body: `{"message":"Access token is invalid"}`, want: balanceErrorAccessTokenAuth},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := accessTokenAuthErrorCode([]byte(test.body)); got != test.want {
				t.Fatalf("error code=%q, want %q", got, test.want)
			}
		})
	}
}

func TestHighestBalanceRoutingUsesShortRefreshBudget(t *testing.T) {
	now := time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)
	first := testAccount("a", "https://a.invalid/v1", "key-a")
	second := testAccount("b", "https://b.invalid/v1", "key-b")
	app := newTestApplication(t, strategyHighestBalance, func() time.Time { return now }, first, second)
	app.balanceRoutingTimeout = 30 * time.Millisecond
	app.runtime["a"].Balance = balanceSnapshot{Status: "ok", Amount: 2, Unit: "USD", Scope: balanceScopeActual, UpdatedAt: now.Add(-time.Hour)}
	app.runtime["b"].Balance = balanceSnapshot{Status: "ok", Amount: 1, Unit: "USD", Scope: balanceScopeActual, UpdatedAt: now.Add(-time.Hour)}
	var calls atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		select {
		case <-release:
			return nil, errors.New("released")
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})}
	started := time.Now()
	selected, ok := app.selectAccount(context.Background(), map[string]bool{})
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("routing waited too long for balance refresh: %v", elapsed)
	}
	if !ok || selected.ID != "a" || app.status().EffectiveStrategy != strategyLeastUsed {
		t.Fatalf("short-budget routing selected=%q ok=%v status=%#v", selected.ID, ok, app.status())
	}
	firstCalls := calls.Load()
	selected, ok = app.selectAccount(context.Background(), map[string]bool{})
	if !ok || selected.ID != "a" || calls.Load() != firstCalls {
		t.Fatalf("routing backoff selected=%q ok=%v calls=%d wantCalls=%d", selected.ID, ok, calls.Load(), firstCalls)
	}
	releaseAll()
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !app.acquireBalanceRefresh(waitCtx) {
		t.Fatal("route-triggered background refresh did not stop")
	}
	app.releaseBalanceRefresh()
}

func TestRouteTimeoutKeepsItsRefreshRunningInBackground(t *testing.T) {
	base := time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)
	var tick atomic.Int64
	now := func() time.Time { return base.Add(time.Duration(tick.Add(1)) * time.Millisecond) }
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	app := newTestApplication(t, strategyHighestBalance, now, account)
	app.balanceRoutingTimeout = 30 * time.Millisecond
	app.runtime["a"].Balance = balanceSnapshot{
		Status: "ok", Amount: 1, Unit: "USD", Scope: balanceScopeActual,
		UpdatedAt: base.Add(-time.Hour), CheckedAt: base.Add(-time.Hour), RefreshStatus: balanceRefreshOK,
	}
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		body := `{"data":{"total_available":1000000}}`
		if r.URL.Path == "/api/status" {
			body = `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})}
	selected, ok := app.selectAccount(context.Background(), map[string]bool{})
	if !ok || selected.ID != "a" {
		t.Fatalf("route selection failed while starting refresh: selected=%q ok=%v", selected.ID, ok)
	}
	for count := 0; count < 2; count++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("route-triggered refresh did not start")
		}
	}
	releaseAll()
	deadline := time.Now().Add(time.Second)
	for {
		app.mu.Lock()
		balance := app.runtime["a"].Balance
		app.mu.Unlock()
		if balance.Status == "ok" && balance.RefreshStatus == balanceRefreshOK && balance.Amount == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("route-triggered background refresh did not finish: %#v", balance)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPreparedRouteRefreshIgnoresLaterBackoff(t *testing.T) {
	now := time.Date(2026, 7, 12, 1, 0, 0, 0, time.UTC)
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	app := newTestApplication(t, strategyHighestBalance, func() time.Time { return now }, account)
	app.runtime["a"].Balance = balanceSnapshot{
		Status: "ok", Amount: 1, Unit: "USD", Scope: balanceScopeActual,
		UpdatedAt: now.Add(-time.Hour), CheckedAt: now.Add(-time.Hour), RefreshStatus: balanceRefreshOK,
	}
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"data":{"total_available":1000000}}`
		if r.URL.Path == "/api/status" {
			body = `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: r,
		}, nil
	})}
	ids := map[string]bool{"a": true}
	accounts, order := app.prepareBalanceRefresh(ids, balanceTTL)
	if len(accounts) != 1 {
		t.Fatalf("prepared accounts=%d, want 1", len(accounts))
	}
	app.markBalanceRouteTimeout(ids)
	reports := app.refreshBalanceAccounts(context.Background(), accounts, order)
	if len(reports) != 1 || reports[0].Balance.Status != "ok" || reports[0].Balance.Amount != 2 {
		t.Fatalf("prepared refresh was suppressed by later backoff: %#v", reports)
	}
}

func TestAdminModelsUsesSavedKeyAndPreservesAccountState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer saved-key" {
			t.Errorf("wrong authorization: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Errorf("wrong accept header: %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":" gpt-a "},"gpt-b",{"id":"gpt-a"}," ",{"id":"gpt-c"},{"id":"leak-saved-key"},{"id":"leak-access-secret"},{"name":"ignored"},42]}`)
	}))
	defer server.Close()
	now := time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC)
	account := testAccount("a", server.URL+"/v1", "saved-key")
	account.NewAPIAuthMode = newAPIAuthAccessToken
	account.NewAPIUserID = 42
	account.NewAPISecret = "access-secret"
	account.Verified = true
	account.BlockedReason = "quota"
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	app.runtime["a"].AssignedRequests = 7
	app.runtime["a"].LastUsedAt = now.Add(-time.Minute)
	app.runtime["a"].CooldownUntil = now.Add(time.Minute)
	app.runtime["a"].HealthState = accountHealthRecentFailure
	app.runtime["a"].HealthCheckedAt = now
	request := accountProbeRequest{
		AccountID: "a", Candidate: &accountInput{BaseURL: server.URL + "/v1"},
		AllowInsecureHTTP: boolPointer(true),
	}
	response := adminJSON(app, http.MethodPost, "/admin/models", request, "http://127.0.0.1:4000")
	var result struct {
		OK     bool     `json:"ok"`
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode models response: %v body=%s", err, response.Body.String())
	}
	if response.Code != http.StatusOK || !result.OK || strings.Join(result.Models, ",") != "gpt-a,gpt-b,gpt-c" {
		t.Fatalf("models status=%d result=%#v body=%s", response.Code, result, response.Body.String())
	}
	if !app.cfg.Accounts[0].Verified || app.cfg.Accounts[0].BlockedReason != "quota" ||
		app.runtime["a"].AssignedRequests != 7 || !app.runtime["a"].LastUsedAt.Equal(now.Add(-time.Minute)) ||
		!app.runtime["a"].CooldownUntil.Equal(now.Add(time.Minute)) ||
		app.runtime["a"].HealthState != accountHealthRecentFailure || !app.runtime["a"].HealthCheckedAt.Equal(now) {
		t.Fatalf("models probe changed account state: account=%#v runtime=%#v", app.cfg.Accounts[0], app.runtime["a"])
	}
}

func TestParseUpstreamModelsDoesNotHideModelsForShortKeys(t *testing.T) {
	models, err := parseUpstreamModels([]byte(`{"data":[{"id":"test-model"},{"id":"test"}]}`), "test")
	if err != nil || strings.Join(models, ",") != "test-model" {
		t.Fatalf("short key filtering models=%v err=%v", models, err)
	}
}

func TestAccountResetArmsRealRequestProbeWithoutUpstreamCall(t *testing.T) {
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer server.Close()

	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	account := testAccount("a", server.URL+"/v1", "key-a")
	account.BlockedReason = "quota"
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	runtime := app.runtime["a"]
	runtime.CooldownReason = "quota"
	runtime.CooldownUntil = now.Add(accountCooldown)
	runtime.RecoveryFailures = 2
	runtime.LastRecoveryAt = now.Add(-time.Second)
	runtime.HealthState = accountHealthRecentFailure
	runtime.HealthCheckedAt = now
	beforeAccount := app.cfg.Accounts[0]
	if err := saveConfig(app.configPath, app.cfg); err != nil {
		t.Fatal(err)
	}

	response := adminJSON(app, http.MethodPost, "/admin/accounts/reset",
		map[string]string{"id": "a"}, "http://127.0.0.1:4000")
	if response.Code != http.StatusAccepted || upstreamCalls.Load() != 0 || app.cfg.Accounts[0] != beforeAccount ||
		!runtime.CooldownUntil.Equal(now) || !runtime.FailureBarrierAt.Equal(now) ||
		runtime.CooldownReason != "quota" || runtime.RecoveryFailures != 2 ||
		runtime.HealthState != accountHealthUnverified || !runtime.HealthCheckedAt.IsZero() {
		t.Fatalf("reset did not arm real probe: status=%d calls=%d account=%#v runtime=%#v body=%s",
			response.Code, upstreamCalls.Load(), app.cfg.Accounts[0], runtime, response.Body.String())
	}
	persisted, found, err := loadConfig(app.configPath)
	if err != nil || !found || persisted.Accounts[0] != beforeAccount {
		t.Fatalf("arming probe changed persisted config: found=%v cfg=%#v err=%v", found, persisted, err)
	}
	selected, ok, probe := app.selectProxyAccount(context.Background(), map[string]bool{})
	if !ok || !probe || selected.ID != "a" || !runtime.ProbeInFlight {
		t.Fatalf("armed account was not selected as probe: selected=%#v ok=%v probe=%v runtime=%#v",
			selected, ok, probe, runtime)
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if selected, ok, probe = app.selectProxyAccount(waitCtx, map[string]bool{}); ok || probe {
		t.Fatalf("second concurrent probe was selected: selected=%#v ok=%v probe=%v", selected, ok, probe)
	}
	app.finishAccountProbe(account, false)
}

func TestAccountResetClearsBlockOnlyAfterMatchingRealRequestSucceeds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var payload map[string]any
		if r.URL.Path != "/v1/responses" || json.NewDecoder(r.Body).Decode(&payload) != nil || payload["model"] != "gpt-image-2" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()

	now := time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC)
	account := testAccount("a", server.URL+"/v1", "key-a")
	account.BlockedReason = "quota"
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	app.runtime["a"].CooldownReason = "quota"
	app.runtime["a"].CooldownUntil = now.Add(accountCooldown)
	if response := adminJSON(app, http.MethodPost, "/admin/accounts/reset",
		map[string]string{"id": "a"}, "http://127.0.0.1:4000"); response.Code != http.StatusAccepted {
		t.Fatalf("arm status=%d body=%s", response.Code, response.Body.String())
	}
	now = now.Add(time.Millisecond)
	response := proxyRequest(app, `{"model":"gpt-image-2"}`)
	if response.Code != http.StatusOK || response.Body.String() != "ok" || calls.Load() != 1 ||
		app.cfg.Accounts[0].BlockedReason != "" || app.runtime["a"].ProbeInFlight {
		t.Fatalf("real request did not recover account: status=%d calls=%d account=%#v runtime=%#v body=%q",
			response.Code, calls.Load(), app.cfg.Accounts[0], app.runtime["a"], response.Body.String())
	}
	persisted, found, err := loadConfig(app.configPath)
	if err != nil || !found || persisted.Accounts[0].BlockedReason != "" {
		t.Fatalf("real recovery was not persisted: found=%v cfg=%#v err=%v", found, persisted, err)
	}
}

func TestAccountResetRealQuotaFailureKeepsBlockAndBackoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"insufficient_quota"}}`)
	}))
	defer server.Close()

	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	account := testAccount("a", server.URL+"/v1", "key-a")
	account.BlockedReason = "quota"
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	runtime := app.runtime["a"]
	runtime.CooldownReason = "quota"
	runtime.CooldownUntil = now.Add(accountCooldown)
	runtime.RecoveryFailures = 1
	runtime.LastRecoveryAt = now.Add(-time.Second)
	if response := adminJSON(app, http.MethodPost, "/admin/accounts/reset",
		map[string]string{"id": "a"}, "http://127.0.0.1:4000"); response.Code != http.StatusAccepted {
		t.Fatalf("arm status=%d body=%s", response.Code, response.Body.String())
	}
	now = now.Add(time.Millisecond)
	response := proxyRequest(app, `{"model":"requested-model"}`)
	if response.Code != http.StatusServiceUnavailable || app.cfg.Accounts[0].BlockedReason != "quota" ||
		runtime.RecoveryFailures != 2 || runtime.CooldownReason != "quota" || !runtime.CooldownUntil.After(now) || runtime.ProbeInFlight {
		t.Fatalf("quota failure did not keep recovery backoff: status=%d account=%#v runtime=%#v body=%s",
			response.Code, app.cfg.Accounts[0], runtime, response.Body.String())
	}
}

func TestAdminNeverLeaksKeysAndRejectsCrossSiteWrites(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://a.invalid/v1", "secret-a"),
		testAccount("b", "https://b.invalid/v1", "secret-b"),
	)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/admin/config", nil)
	request.Host = listenAddress
	markLocalRequest(request)
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "secret-a") ||
		strings.Contains(response.Body.String(), "secret-b") {
		t.Fatalf("public config leaked a key: status=%d body=%s", response.Code, response.Body.String())
	}
	crossSite := adminJSON(app, http.MethodPut, "/admin/config", saveRequest{}, "https://attacker.invalid")
	if crossSite.Code != http.StatusForbidden {
		t.Fatalf("cross-site write status=%d, want 403", crossSite.Code)
	}
}

func TestProxyBodyBudgetWaitsAndCancels(t *testing.T) {
	newApp := func(t *testing.T) (*application, *atomic.Int32) {
		t.Helper()
		account := testAccount("a", "https://a.invalid/v1", "key-a")
		account.Verified = true
		app := newTestApplication(t, strategyPriority, time.Now, account)
		app.proxyBodyBudget = newByteBudget(1)
		calls := &atomic.Int32{}
		app.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				Request:    request,
			}, nil
		})}
		return app, calls
	}

	t.Run("waits for release", func(t *testing.T) {
		app, calls := newApp(t)
		if !app.proxyBodyBudget.acquire(context.Background(), 1) {
			t.Fatal("failed to reserve the body budget")
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- proxyRequest(app, "x") }()
		select {
		case response := <-done:
			t.Fatalf("request bypassed body budget: status=%d", response.Code)
		case <-time.After(50 * time.Millisecond):
		}
		if calls.Load() != 0 {
			t.Fatalf("upstream called before budget release: calls=%d", calls.Load())
		}
		app.proxyBodyBudget.release(1)
		select {
		case response := <-done:
			if response.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("released request status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
			}
		case <-time.After(time.Second):
			t.Fatal("request did not continue after budget release")
		}
		app.proxyBodyBudget.mu.Lock()
		used := app.proxyBodyBudget.used
		app.proxyBodyBudget.mu.Unlock()
		if used != 0 {
			t.Fatalf("body budget was not returned after request: used=%d", used)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		app, calls := newApp(t)
		if !app.proxyBodyBudget.acquire(context.Background(), 1) {
			t.Fatal("failed to reserve the body budget")
		}
		ctx, cancel := context.WithCancel(context.Background())
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", strings.NewReader("x")).WithContext(ctx)
		request.Host = listenAddress
		markLocalRequest(request)
		request.Header.Set("Authorization", "Bearer gateway-token")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			app.routes().ServeHTTP(response, request)
			close(done)
		}()
		time.Sleep(50 * time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("canceled request remained blocked on body budget")
		}
		if calls.Load() != 0 {
			t.Fatalf("canceled request reached upstream: calls=%d", calls.Load())
		}
		app.proxyBodyBudget.release(1)
		if next := proxyRequest(app, "x"); next.Code != http.StatusOK || calls.Load() != 1 {
			t.Fatalf("budget leaked after cancellation: status=%d calls=%d body=%s", next.Code, calls.Load(), next.Body.String())
		}
	})
}

func TestProxyRejectsOversizedBodyBeforeUpstream(t *testing.T) {
	var calls atomic.Int32
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://a.invalid/v1", "key-a"),
	)
	app.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("must not be called")
	})}
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", strings.NewReader("small"))
	request.Host = listenAddress
	markLocalRequest(request)
	request.ContentLength = maxProxyBody + 1
	request.Header.Set("Authorization", "Bearer gateway-token")
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || calls.Load() != 0 {
		t.Fatalf("oversized body was forwarded: status=%d calls=%d", response.Code, calls.Load())
	}
}

func TestCompletelyUnavailablePoolReturns503(t *testing.T) {
	account := testAccount("a", "https://a.invalid/v1", "key-a")
	account.Enabled = false
	app := newTestApplication(t, strategyPriority, time.Now, account)
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServeWithStopLoopCancelsActiveRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	requestStarted := make(chan struct{})
	requestCanceled := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(requestStarted)
		<-request.Context().Done()
		close(requestCanceled)
	})}
	t.Cleanup(func() {
		_ = server.Close()
	})
	clientDone := make(chan struct{})
	loop := func(requestStop func(), serverDone <-chan struct{}) error {
		go func() {
			defer close(clientDone)
			response, requestErr := http.Get("http://" + listener.Addr().String())
			if requestErr == nil {
				response.Body.Close()
			}
		}()
		select {
		case <-requestStarted:
		case <-time.After(time.Second):
			t.Fatal("request did not reach the server")
		}
		requestStop()
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Fatal("server did not stop")
		}
		return nil
	}
	if err := serveWithStopLoop(server, listener, loop); err != nil {
		t.Fatalf("serveWithStopLoop returned %v", err)
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("active request context was not canceled")
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatal("client did not finish after shutdown")
	}
}

func TestServeWithStopLoopRejectsUnexpectedLoopReturn(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.NotFoundHandler()}
	t.Cleanup(func() {
		_ = server.Close()
	})
	err = serveWithStopLoop(server, listener, func(func(), <-chan struct{}) error { return nil })
	if !errors.Is(err, errSystemTrayStopped) {
		t.Fatalf("serveWithStopLoop returned %v", err)
	}
}

func TestServeWithStopLoopReturnsLoopError(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.NotFoundHandler()}
	t.Cleanup(func() {
		_ = server.Close()
	})
	want := errors.New("tray loop failed")
	err = serveWithStopLoop(server, listener, func(func(), <-chan struct{}) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("serveWithStopLoop returned %v", err)
	}
}

func TestApplicationLogPathForPlatforms(t *testing.T) {
	temporaryRoot := t.TempDir()
	runtimeRoot := t.TempDir()
	configPath := filepath.Join(t.TempDir(), configDirectory, configFilename)
	for _, test := range []struct {
		name        string
		goos        string
		runtimeRoot string
		want        string
	}{
		{name: "linux runtime", goos: "linux", runtimeRoot: runtimeRoot,
			want: filepath.Join(runtimeRoot, configDirectory, logFilename)},
		{name: "linux fallback", goos: "linux", runtimeRoot: "relative",
			want: filepath.Join(temporaryRoot, configDirectory+"-"+logReference(filepath.Clean(configPath)), logFilename)},
		{name: "darwin", goos: "darwin",
			want: filepath.Join(temporaryRoot, configDirectory+"-"+logReference(filepath.Clean(configPath)), logFilename)},
		{name: "windows", goos: "windows", want: filepath.Join(filepath.Dir(configPath), logFilename)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := applicationLogPath(test.goos, temporaryRoot, test.runtimeRoot, configPath); got != test.want {
				t.Fatalf("applicationLogPath(%s)=%q, want %q", test.goos, got, test.want)
			}
		})
	}
}

func TestLogWriterKeepsOneBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), logFilename)
	writer, err := openRotatingLogWriter(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	write := func(value string) {
		t.Helper()
		if written, writeErr := writer.Write([]byte(value)); writeErr != nil || written != len(value) {
			t.Fatalf("write %q = %d, %v", value, written, writeErr)
		}
	}
	write("first\n")
	write("second\n")
	write("third\n")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	backup := path + ".1"
	data, err := os.ReadFile(backup)
	if err != nil || string(data) != "second\n" {
		t.Fatalf("backup=%q err=%v", data, err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "third\n" {
		t.Fatalf("current=%q err=%v", data, err)
	}
	if _, err := os.Stat(path + ".2"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected second backup: %v", err)
	}
}

func TestLogWriterFallsBackWhenBackupCannotBeReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), logFilename)
	writer, err := openRotatingLogWriter(path, 128)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.Repeat("a", 120)
	if _, err := writer.Write([]byte(first)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".1", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("second-line\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("third\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("overflow-payload-that-must-not-be-written\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("dropped-again\n")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	logged := string(data)
	if !strings.Contains(logged, first) || !strings.Contains(logged, "msg=log_rotation_fallback") ||
		!strings.Contains(logged, "second-line") || !strings.Contains(logged, "third") {
		t.Fatalf("unexpected fallback log: %q", logged)
	}
	if strings.Count(logged, "msg=log_writes_suspended") != 1 ||
		strings.Contains(logged, "overflow-payload") || strings.Contains(logged, "dropped-again") {
		t.Fatalf("unexpected suspension log: %q", logged)
	}
	if err := os.Remove(path + ".1"); err != nil {
		t.Fatal(err)
	}
	writer.mu.Lock()
	writer.retryAt = time.Time{}
	writer.mu.Unlock()
	if _, err := writer.Write([]byte("resumed\n")); err != nil {
		t.Fatal(err)
	}
	writer.mu.Lock()
	suspended := writer.suspended
	writer.mu.Unlock()
	if suspended {
		t.Fatal("log writer did not resume after rotation recovered")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || string(data) != "resumed\n" {
		t.Fatalf("resumed log=%q err=%v", data, err)
	}
	backupData, err := os.ReadFile(path + ".1")
	if err != nil || !strings.Contains(string(backupData), "msg=log_writes_suspended") {
		t.Fatalf("recovered backup=%q err=%v", backupData, err)
	}
}

func TestProxyLogsDoNotContainSecrets(t *testing.T) {
	first := testAccount("account-id-secret\nfirst", "https://first.invalid/v1", "api-secret-first")
	first.Name = "account-name-secret"
	first.NewAPISecret = "access-secret"
	second := testAccount("second", "https://second.invalid/v1", "api-secret-second")
	app := newTestApplication(t, strategyPriority, time.Now, first, second)
	var logs bytes.Buffer
	app.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	app.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "first.invalid" {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"error":{"code":"rate_limit_exceeded","message":"response-secret"}}`,
				)),
				Request: request,
			}, nil
		}
		return nil, errors.New("network-secret api-secret-second query-secret")
	})}
	request := httptest.NewRequest(http.MethodPost,
		"http://127.0.0.1:4000/v1/responses?token=query-secret", strings.NewReader("body-secret"))
	request.Host = listenAddress
	markLocalRequest(request)
	request.Header.Set("Authorization", "Bearer gateway-token")
	request.Header.Set("Cookie", "cookie-secret")
	request.Header.Set("X-Debug", "header-secret")
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	logged := logs.String()
	for _, secret := range []string{
		"account-id-secret", "account-name-secret", "api-secret-first", "api-secret-second",
		"access-secret", "gateway-token", "query-secret", "body-secret", "cookie-secret",
		"header-secret", "response-secret", "network-secret",
	} {
		if strings.Contains(logged, secret) {
			t.Fatalf("log leaked %q: %s", secret, logged)
		}
	}
	if !strings.Contains(logged, "msg=proxy_request_started") ||
		!strings.Contains(logged, "reason=rate_limit") ||
		!strings.Contains(logged, "msg=proxy_upstream_failed") ||
		!strings.Contains(logged, "account_ref=") {
		t.Fatalf("diagnostic events missing: %s", logged)
	}
}

func TestStructuredQuotaError(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{`{"error":{"code":"insufficient_quota"}}`, true},
		{`{"details":{"reason":"credit-balance-exhausted"}}`, true},
		{`{"error":{"code":"rate_limit_exceeded"}}`, false},
		{`{"error":{"message":"当前用户余额不足"}}`, true},
	}
	for _, test := range tests {
		if got := structuredQuotaError([]byte(test.body)); got != test.want {
			t.Fatalf("structuredQuotaError(%s)=%v, want %v", test.body, got, test.want)
		}
	}
}

func TestStructuredAccountRestrictionError(t *testing.T) {
	for _, test := range []struct {
		body string
		want bool
	}{
		{`{"error":{"code":"account_suspended"}}`, true},
		{`{"details":{"reason":"organization-deactivated"}}`, true},
		{`{"error":{"message":"账号已封禁"}}`, true},
		{`{"error":{"code":"model_access_denied"}}`, false},
		{`{"error":{"message":"permission denied"}}`, false},
	} {
		if got := structuredAccountRestrictionError([]byte(test.body)); got != test.want {
			t.Fatalf("structuredAccountRestrictionError(%s)=%v, want %v", test.body, got, test.want)
		}
	}
}

func TestStructuredRequestCompatibilityError(t *testing.T) {
	for _, test := range []struct {
		body string
		want bool
	}{
		{`{"error":{"code":"model_access_denied"}}`, true},
		{`{"error":{"code":"unsupported_model"}}`, true},
		{`{"error":{"message":"model gpt-image-2 is only supported on /v1/images/generations and /v1/images/edits"}}`, true},
		{`{"error":{"message":"模型仅支持图片生成接口"}}`, true},
		{`{"error":{"code":"internal_error","message":"model service overloaded"}}`, false},
		{`{"error":{"message":"upstream temporarily unavailable"}}`, false},
	} {
		if got := structuredRequestCompatibilityError([]byte(test.body)); got != test.want {
			t.Fatalf("structuredRequestCompatibilityError(%s)=%v, want %v", test.body, got, test.want)
		}
	}
}

func TestRetryAfterDuration(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"90", 90 * time.Second},
		{now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute},
		{"invalid", accountCooldown},
		{"2678401", maxRetryAfter},
		{"9223372036", maxRetryAfter},
	} {
		if got := retryAfterDuration(test.value, now); got != test.want {
			t.Fatalf("retryAfterDuration(%q)=%s, want %s", test.value, got, test.want)
		}
	}
}

func TestRedactSecretsHandlesOverlappingValues(t *testing.T) {
	message := redactSecrets("credential abc123", "abc", "abc123")
	if message != "credential [已隐藏]" {
		t.Fatalf("overlapping secret was only partially redacted: %q", message)
	}
}

func testAccount(id, baseURL, key string) accountConfig {
	return accountConfig{
		ID: id, Name: strings.ToUpper(id), Provider: "auto", BaseURL: baseURL, APIKey: key, Enabled: true, Revision: 1,
	}
}

func testCodexOAuthAccount(id, realAccountID string, now time.Time) accountConfig {
	return accountConfig{
		ID: id, Name: strings.ToUpper(id), AuthType: accountAuthCodexOAuth,
		CodexAccessToken: "access-" + id, CodexRefreshToken: "refresh-" + id,
		CodexAccountID: realAccountID, CodexExpiresAt: now.Add(time.Hour).Format(time.RFC3339),
		Enabled: true, Revision: 1,
	}
}

func newTestApplication(t *testing.T, strategy string, now func() time.Time, accounts ...accountConfig) *application {
	t.Helper()
	if now == nil {
		now = time.Now
	}
	client := &http.Client{}
	app := &application{
		cfg: storedConfig{
			Version: configVersion, Accounts: accounts, Strategy: strategy,
			AllowInsecureHTTP: true,
			GatewayTokens:     []gatewayTokenConfig{{ID: "tok_test", Name: "Test Token", Token: "gateway-token"}},
		},
		configPath:            filepath.Join(t.TempDir(), "config.dat"),
		csrfToken:             "csrf-token",
		client:                client,
		now:                   now,
		persistConfig:         saveConfig,
		balanceTimeout:        balanceRefreshTime,
		balanceRoutingTimeout: balanceRoutingTime,
		proxyBodyBudget:       newByteBudget(maxProxyBody),
		runtime:               make(map[string]*accountRuntime),
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
	for _, account := range accounts {
		app.runtime[account.ID] = &accountRuntime{Revision: account.Revision}
	}
	return app
}

func proxyRequest(app *application, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", strings.NewReader(body))
	request.Host = listenAddress
	markLocalRequest(request)
	request.Header.Set("Authorization", "Bearer gateway-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	return response
}

func adminJSON(app *application, method, path string, value any, origin string) *httptest.ResponseRecorder {
	body, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	request := httptest.NewRequest(method, "http://127.0.0.1:4000"+path, strings.NewReader(string(body)))
	request.Host = listenAddress
	markLocalRequest(request)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	request.Header.Set("X-CSRF-Token", app.csrfToken)
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	return response
}

func markLocalRequest(request *http.Request) {
	request.RemoteAddr = "127.0.0.1:54321"
}

func boolPointer(value bool) *bool {
	return &value
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type deadlineResponseWriter struct {
	header               http.Header
	deadline             time.Time
	deadlines            []time.Time
	writeWithoutDeadline bool
	body                 bytes.Buffer
}

func (writer *deadlineResponseWriter) Header() http.Header {
	return writer.header
}

func (writer *deadlineResponseWriter) WriteHeader(int) {}

func (writer *deadlineResponseWriter) Write(data []byte) (int, error) {
	if writer.deadline.IsZero() {
		writer.writeWithoutDeadline = true
	}
	return writer.body.Write(data)
}

func (writer *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	writer.deadline = deadline
	writer.deadlines = append(writer.deadlines, deadline)
	return nil
}

type oneChunkThenError struct {
	chunk []byte
	done  bool
}

type closeSignalBody struct {
	io.Reader
	once   sync.Once
	closed chan struct{}
}

func (body *closeSignalBody) Close() error {
	body.once.Do(func() { close(body.closed) })
	return nil
}

type writeErrorResponseWriter struct {
	header http.Header
}

func (w *writeErrorResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (*writeErrorResponseWriter) WriteHeader(int) {}

func (*writeErrorResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("downstream write failed")
}

func (body *oneChunkThenError) Read(destination []byte) (int, error) {
	if body.done {
		return 0, io.ErrUnexpectedEOF
	}
	body.done = true
	return copy(destination, body.chunk), io.ErrUnexpectedEOF
}

func (body *oneChunkThenError) Close() error {
	return nil
}
