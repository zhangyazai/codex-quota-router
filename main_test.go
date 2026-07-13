package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHealthAndEmbeddedPageExposeAutoRefreshRelease(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now)
	healthRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/healthz", nil)
	healthRequest.Host = listenAddress
	healthResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(healthResponse, healthRequest)
	if healthResponse.Code != http.StatusOK || !strings.Contains(healthResponse.Body.String(), `"version":"0.2.5"`) {
		t.Fatalf("health status=%d body=%s", healthResponse.Code, healthResponse.Body.String())
	}

	indexRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/", nil)
	indexRequest.Host = listenAddress
	indexResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(indexResponse, indexRequest)
	body := indexResponse.Body.String()
	if indexResponse.Code != http.StatusOK || !strings.Contains(body, "balancePollInterval = 60000") ||
		!strings.Contains(body, "?automatic=1") || !strings.Contains(body, "<dt>可参与路由账号</dt>") ||
		!strings.Contains(body, `available: "可参与路由"`) || !strings.Contains(body, `recent_success: "近期成功"`) ||
		!strings.Contains(body, `recent_failure: "最近失败"`) ||
		!strings.Contains(body, "曾成功响应（历史记录），当前状态待验证") ||
		!strings.Contains(body, "账号请求失败过多，暂时停用至") || !strings.Contains(body, "账号受限或停用") ||
		!strings.Contains(body, "立即恢复账号") ||
		!strings.Contains(body, `value="lowest_balance"`) || !strings.Contains(body, "验证并恢复账号") ||
		!strings.Contains(body, "账面余额") || !strings.Contains(body, `data-slot="models"`) ||
		!strings.Contains(body, `api("/admin/models"`) || strings.Contains(body, "testModel") {
		t.Fatalf("embedded auto-refresh page missing: status=%d", indexResponse.Code)
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

func TestLowestBalanceStrategyIsValid(t *testing.T) {
	cfg := storedConfig{Strategy: " LOWEST_BALANCE "}
	if err := normalizeAndValidateConfig(&cfg); err != nil || cfg.Strategy != strategyLowestBalance {
		t.Fatalf("lowest balance strategy rejected: strategy=%q err=%v", cfg.Strategy, err)
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
	oldToken := app.cfg.GatewayToken
	response := adminJSON(app, http.MethodPut, "/admin/config", saveRequest{RotateGatewayToken: true}, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK {
		t.Fatalf("rotate token status=%d body=%s", response.Code, response.Body.String())
	}
	if app.cfg.GatewayToken == "" || app.cfg.GatewayToken == oldToken {
		t.Fatalf("gateway token was not rotated: %q", app.cfg.GatewayToken)
	}
	newToken := app.cfg.GatewayToken
	if !strings.Contains(response.Body.String(), newToken) {
		t.Fatal("rotate response did not include the updated Codex snippet")
	}
	saved, found, err := loadConfig(app.configPath)
	if err != nil || !found || saved.GatewayToken != newToken {
		t.Fatalf("rotated token was not persisted: found=%v token=%q err=%v", found, saved.GatewayToken, err)
	}
	proxyStatus := func(token string) int {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", strings.NewReader(`{}`))
		request.Host = listenAddress
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

func TestQuotaBlocksOneAccountAndTriesNext(t *testing.T) {
	var firstCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
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
	persisted, found, err := loadConfig(app.configPath)
	if err != nil || !found || persisted.Accounts[0].BlockedReason != "quota" {
		t.Fatalf("quota was not persisted: found=%v cfg=%#v err=%v", found, persisted, err)
	}
	proxyRequest(app, `{}`)
	if firstCalls.Load() != 1 || secondCalls.Load() != 2 {
		t.Fatalf("blocked account was retried: first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
}

func TestExplicitAccountRestrictionBlocksButGeneric403DoesNot(t *testing.T) {
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

	t.Run("generic forbidden", func(t *testing.T) {
		var secondCalls atomic.Int32
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":"model_access_denied"}}`)
		}))
		defer first.Close()
		second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			secondCalls.Add(1)
			_, _ = io.WriteString(w, "unexpected")
		}))
		defer second.Close()
		app := newTestApplication(t, strategyPriority, time.Now,
			testAccount("a", first.URL, "key-a"), testAccount("b", second.URL, "key-b"))
		response := proxyRequest(app, `{}`)
		if response.Code != http.StatusForbidden || secondCalls.Load() != 0 || app.cfg.Accounts[0].BlockedReason != "" {
			t.Fatalf("generic 403 blocked whole account: status=%d second=%d account=%#v",
				response.Code, secondCalls.Load(), app.cfg.Accounts[0])
		}
	})
}

func TestVerifiedAndUnverified401Handling(t *testing.T) {
	for _, test := range []struct {
		name        string
		verified    bool
		wantBlock   string
		wantCooling bool
	}{
		{name: "verified blocks", verified: true, wantBlock: "unauthorized"},
		{name: "unverified cools", verified: false, wantCooling: true},
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
			if !test.verified {
				now = app.runtime["a"].CooldownUntil
				response = proxyRequest(app, `{}`)
				if response.Code != http.StatusOK || app.cfg.Accounts[0].BlockedReason != "unauthorized" {
					t.Fatalf("second unverified 401 did not block: status=%d account=%#v", response.Code, app.cfg.Accounts[0])
				}
			}
		})
	}
}

func TestExplicitAccountTest401AlwaysBlocks(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://a.invalid/v1", "key-a"),
	)
	app.handleAccountTestFailed(app.cfg.Accounts[0], http.StatusUnauthorized, nil)
	if app.cfg.Accounts[0].BlockedReason != "unauthorized" {
		t.Fatalf("explicit 401 did not block account: %#v", app.cfg.Accounts[0])
	}
}

func TestUnverified401StrikeExpires(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://a.invalid/v1", "key-a"))
	if action := app.handleAccountUnauthorized(app.cfg.Accounts[0]); action != "cooldown" {
		t.Fatalf("first 401 action=%q", action)
	}
	now = now.Add(upstreamFailureReset + time.Second)
	if action := app.handleAccountUnauthorized(app.cfg.Accounts[0]); action != "cooldown" || app.cfg.Accounts[0].BlockedReason != "" {
		t.Fatalf("expired 401 strike action=%q account=%#v", action, app.cfg.Accounts[0])
	}
}

func TestConcurrentUnverified401OnlyCountsOnce(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	startedAt := now
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://a.invalid/v1", "key-a"))
	if action := app.handleAccountUnauthorized(app.cfg.Accounts[0], startedAt); action != "cooldown" {
		t.Fatalf("first concurrent 401 action=%q", action)
	}
	now = now.Add(accountCooldown + time.Second)
	if action := app.handleAccountUnauthorized(app.cfg.Accounts[0], startedAt); action != "ignored" ||
		app.cfg.Accounts[0].BlockedReason != "" {
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
	if second.Code != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatalf("cooldown was ignored: status=%d calls=%d", second.Code, calls.Load())
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
		{name: "payment required", statusCode: http.StatusPaymentRequired, wantBlock: "quota"},
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

func TestPostServerErrorIsNotReplayed(t *testing.T) {
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
			return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader("gateway failed")), Request: r}, nil
		}
		secondCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader("second")), Request: r}, nil
	})}
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusBadGateway || response.Body.String() != "gateway failed" ||
		firstCalls.Load() != 1 || secondCalls.Load() != 0 || app.runtime["a"].UpstreamFailures != 1 {
		t.Fatalf("response=%d first=%d second=%d runtime=%#v body=%q",
			response.Code, firstCalls.Load(), secondCalls.Load(), app.runtime["a"], response.Body.String())
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

func TestHalfOpenHeadersReleaseWaitingRequests(t *testing.T) {
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
			return nil, errors.New("response headers did not release the account as regular traffic")
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
		if response.Code != http.StatusOK || response.Body.String() != "second" {
			t.Fatalf("second response status=%d body=%q", response.Code, response.Body.String())
		}
	case <-time.After(2 * time.Second):
		_ = writer.Close()
		t.Fatal("waiting request remained blocked after successful response headers")
	}
	_, _ = io.WriteString(writer, "data: done\n\n")
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

func TestAccountResumeClearsTemporaryDisableWithoutProbe(t *testing.T) {
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
	if response.Code != http.StatusOK || app.status().Accounts[0].State != "available" ||
		app.runtime["a"].UpstreamFailures != 0 || !app.runtime["a"].CooldownUntil.IsZero() {
		t.Fatalf("resume status=%d runtime=%#v body=%s", response.Code, app.runtime["a"], response.Body.String())
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
		request.Header.Set("Authorization", "Bearer gateway-token")
		request.Header.Set("Content-Type", "application/json")
		ctx, cancel := context.WithCancel(request.Context())
		cancel()
		app.routes().ServeHTTP(httptest.NewRecorder(), request.WithContext(ctx))
		if health := app.status().Accounts[0].HealthState; health != accountHealthRecentSuccess {
			t.Fatalf("canceled proxy changed health to %q", health)
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

func TestAdminSaveKeepsExistingKeysGeneratesIDsAndRedacts(t *testing.T) {
	first := testAccount("a", "https://a.invalid/v1", "secret-a")
	first.Verified = true
	first.BlockedReason = "quota"
	second := testAccount("b", "https://b.invalid/v1", "secret-b")
	app := newTestApplication(t, strategyPriority, time.Now, first, second)
	app.runtime["a"].AssignedRequests = 9
	enabled := true
	request := saveRequest{
		Accounts: &[]accountInput{
			{ID: "a", Name: "A renamed", BaseURL: "https://a.invalid/v1", Enabled: &enabled},
			{Name: "C", BaseURL: "https://c.invalid/v1", APIKey: "secret-c", Enabled: &enabled},
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
		app.cfg.Accounts[1].APIKey != "secret-c" {
		t.Fatalf("wrong saved accounts: %#v", app.cfg.Accounts)
	}
	if app.cfg.Accounts[0].Revision != 1 || app.runtime["a"].AssignedRequests != 9 {
		t.Fatalf("unchanged account lost revision/runtime: %#v %#v", app.cfg.Accounts[0], app.runtime["a"])
	}
	if app.accountIndexLocked("b") >= 0 {
		t.Fatal("omitted account was not deleted")
	}
}

func TestAdminSaveURLOrKeyChangeResetsStateAndRejectsDuplicates(t *testing.T) {
	account := testAccount("a", "https://a.invalid/v1", "secret-a")
	account.Verified = true
	account.BlockedReason = "quota"
	app := newTestApplication(t, strategyPriority, time.Now, account)
	app.runtime["a"].AssignedRequests = 4
	enabled := true
	request := saveRequest{Accounts: &[]accountInput{
		{ID: "a", Name: "A", BaseURL: "https://changed.invalid/v1", APIKey: "new-secret", Enabled: &enabled},
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
	missingKey := saveRequest{Accounts: &[]accountInput{
		{Name: "new", BaseURL: "https://new.invalid/v1", Enabled: &enabled},
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
	account.NewAPIAuthMode = newAPIAuthAccessToken
	account.NewAPIUserID = 42
	account.NewAPISecret = "access-secret"
	app := newTestApplication(t, strategyPriority, time.Now, account)
	enabled := true

	publicRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/admin/config", nil)
	publicRequest.Host = listenAddress
	publicResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(publicResponse, publicRequest)
	if publicResponse.Code != http.StatusOK || strings.Contains(publicResponse.Body.String(), "access-secret") ||
		strings.Contains(publicResponse.Body.String(), `"newApiSecret":`) ||
		!strings.Contains(publicResponse.Body.String(), `"newApiSecretConfigured":true`) {
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
		{name: "unlimited token is capped by compatible user bill", hardLimit: "6", statusBody: `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`, wantAmount: 5, wantUnit: "USD", wantScope: balanceScopeActual},
		{name: "dashboard USD converts to CNY before limiting", hardLimit: "6", statusBody: `{"data":{"quota_per_unit":500000,"quota_display_type":"CNY","usd_exchange_rate":7}}`, wantAmount: 35, wantUnit: "CNY", wantScope: balanceScopeActual},
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
			balance.Scope != balanceScopeActual {
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

func TestNewAPIAccessTokenUsesActualAccountQuota(t *testing.T) {
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

func TestAccountResetVerifiesRequestBeforeClearingBlock(t *testing.T) {
	var modelCalls atomic.Int32
	var responseCalls atomic.Int32
	var balanceCalls atomic.Int32
	var probeStage atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			modelCalls.Add(1)
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer key-a" ||
				!probeStage.CompareAndSwap(0, 1) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, `{"data":[{"id":"recovery-model"},{"id":"other-model"}]}`)
		case "/v1/responses":
			responseCalls.Add(1)
			var payload map[string]any
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer key-a" ||
				!probeStage.CompareAndSwap(1, 2) || json.NewDecoder(r.Body).Decode(&payload) != nil ||
				payload["model"] != "recovery-model" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"bad test request"}}`)
				return
			}
			_, _ = io.WriteString(w, `{}`)
		case "/api/usage/token/":
			balanceCalls.Add(1)
			_, _ = io.WriteString(w, `{"data":{"total_available":500000}}`)
		case "/api/status":
			_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	account := testAccount("a", server.URL+"/v1", "key-a")
	account.BlockedReason = "quota"
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
	app.runtime["a"].CooldownUntil = now.Add(accountCooldown)
	app.runtime["a"].HealthState = accountHealthRecentFailure
	app.runtime["a"].HealthCheckedAt = now
	staleRequestAccount := app.cfg.Accounts[0]
	response := adminJSON(app, http.MethodPost, "/admin/accounts/reset",
		map[string]string{"id": "a"}, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || modelCalls.Load() != 1 || responseCalls.Load() != 1 ||
		probeStage.Load() != 2 || balanceCalls.Load() != 1 ||
		!app.cfg.Accounts[0].Verified || app.cfg.Accounts[0].BlockedReason != "" ||
		!app.runtime["a"].CooldownUntil.IsZero() || app.runtime["a"].Balance.Status != "ok" ||
		app.cfg.Accounts[0].Revision != staleRequestAccount.Revision+1 ||
		app.status().Accounts[0].HealthState != accountHealthRecentSuccess {
		t.Fatalf("reset failed: status=%d account=%#v runtime=%#v models=%d responses=%d balances=%d stage=%d body=%s",
			response.Code, app.cfg.Accounts[0], app.runtime["a"], modelCalls.Load(), responseCalls.Load(),
			balanceCalls.Load(), probeStage.Load(), response.Body.String())
	}
	app.blockAccount(staleRequestAccount, "quota")
	if app.cfg.Accounts[0].BlockedReason != "" {
		t.Fatalf("pre-reset request reblocked account: %#v", app.cfg.Accounts[0])
	}
}

func TestAccountResetModelsFailureOrEmptyDoesNotChangeState(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "request failed", statusCode: http.StatusServiceUnavailable, body: `{"error":"unavailable"}`},
		{name: "empty list", statusCode: http.StatusOK, body: `{"data":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var modelCalls atomic.Int32
			var responseCalls atomic.Int32
			var balanceCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v1/models":
					modelCalls.Add(1)
					w.WriteHeader(test.statusCode)
					_, _ = io.WriteString(w, test.body)
				case "/v1/responses":
					responseCalls.Add(1)
				case "/api/usage/token/", "/api/status":
					balanceCalls.Add(1)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			now := time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC)
			account := testAccount("a", server.URL+"/v1", "key-a")
			account.BlockedReason = "quota"
			app := newTestApplication(t, strategyPriority, func() time.Time { return now }, account)
			app.runtime["a"].CooldownUntil = now.Add(accountCooldown)
			app.runtime["a"].HealthState = accountHealthRecentFailure
			app.runtime["a"].HealthCheckedAt = now
			app.runtime["a"].AssignedRequests = 3
			app.runtime["a"].Balance = balanceSnapshot{Status: "ok", Amount: 1.46, Unit: "USD", UpdatedAt: now}
			if err := saveConfig(app.configPath, app.cfg); err != nil {
				t.Fatal(err)
			}
			beforeAccount := app.cfg.Accounts[0]
			beforeRuntime := *app.runtime["a"]
			response := adminJSON(app, http.MethodPost, "/admin/accounts/reset",
				map[string]string{"id": "a"}, "http://127.0.0.1:4000")
			if response.Code != http.StatusBadGateway || modelCalls.Load() != 1 ||
				responseCalls.Load() != 0 || balanceCalls.Load() != 0 ||
				app.cfg.Accounts[0] != beforeAccount || *app.runtime["a"] != beforeRuntime {
				t.Fatalf("models failure changed state: status=%d account=%#v runtime=%#v models=%d responses=%d balances=%d body=%s",
					response.Code, app.cfg.Accounts[0], app.runtime["a"], modelCalls.Load(),
					responseCalls.Load(), balanceCalls.Load(), response.Body.String())
			}
			persisted, found, err := loadConfig(app.configPath)
			if err != nil || !found || persisted.Accounts[0] != beforeAccount {
				t.Fatalf("models failure changed persisted state: found=%v cfg=%#v err=%v", found, persisted, err)
			}
		})
	}
}

func TestAccountResetFailedRequestBlocksQuotaDespitePositiveBalance(t *testing.T) {
	var modelCalls atomic.Int32
	var responseCalls atomic.Int32
	var balanceCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			modelCalls.Add(1)
			_, _ = io.WriteString(w, `{"data":[{"id":"recovery-model"}]}`)
		case "/v1/responses":
			responseCalls.Add(1)
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = io.WriteString(w, `{"error":{"message":"余额不足"}}`)
		case "/api/usage/token/", "/api/status":
			balanceCalls.Add(1)
			http.Error(w, "balance must not be refreshed", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	account := testAccount("a", server.URL+"/v1", "key-a")
	account.Verified = true
	app := newTestApplication(t, strategyPriority, time.Now, account)
	app.runtime["a"].Balance = balanceSnapshot{
		Status: "ok", Amount: 1.46, Unit: "USD", UpdatedAt: time.Now(),
	}
	if err := saveConfig(app.configPath, app.cfg); err != nil {
		t.Fatal(err)
	}
	originalRevision := app.cfg.Accounts[0].Revision
	response := adminJSON(app, http.MethodPost, "/admin/accounts/reset", map[string]string{"id": "a"}, "http://127.0.0.1:4000")
	if response.Code != http.StatusBadGateway || modelCalls.Load() != 1 || responseCalls.Load() != 1 || balanceCalls.Load() != 0 ||
		app.cfg.Accounts[0].BlockedReason != "quota" || !app.cfg.Accounts[0].Verified ||
		app.cfg.Accounts[0].Revision != originalRevision || app.runtime["a"].Balance.Amount != 1.46 ||
		app.status().Accounts[0].HealthState != accountHealthRecentFailure ||
		!strings.Contains(response.Body.String(), "已更新账号状态") {
		t.Fatalf("failed reset did not block quota: status=%d account=%#v runtime=%#v models=%d responses=%d balances=%d body=%s",
			response.Code, app.cfg.Accounts[0], app.runtime["a"], modelCalls.Load(), responseCalls.Load(),
			balanceCalls.Load(), response.Body.String())
	}
	persisted, found, err := loadConfig(app.configPath)
	if err != nil || !found || persisted.Accounts[0].BlockedReason != "quota" ||
		persisted.Accounts[0].Revision != originalRevision {
		t.Fatalf("failed reset changed persisted state: found=%v cfg=%#v err=%v", found, persisted, err)
	}
}

func TestConcurrentAccountResetSendsOneProbe(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var modelCalls atomic.Int32
	var responseCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			modelCalls.Add(1)
			_, _ = io.WriteString(w, `{"data":[{"id":"recovery-model"}]}`)
		case "/v1/responses":
			responseCalls.Add(1)
			entered <- struct{}{}
			<-release
			_, _ = io.WriteString(w, `{}`)
		case "/api/usage/token/":
			_, _ = io.WriteString(w, `{"data":{"total_available":500000}}`)
		case "/api/status":
			_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	account := testAccount("a", server.URL+"/v1", "key-a")
	account.BlockedReason = "quota"
	app := newTestApplication(t, strategyPriority, time.Now, account)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- adminJSON(app, http.MethodPost, "/admin/accounts/reset",
			map[string]string{"id": "a"}, "http://127.0.0.1:4000")
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("first recovery probe did not start")
	}
	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		secondDone <- adminJSON(app, http.MethodPost, "/admin/accounts/reset",
			map[string]string{"id": "a"}, "http://127.0.0.1:4000")
	}()
	select {
	case second := <-secondDone:
		if second.Code != http.StatusConflict || modelCalls.Load() != 1 || responseCalls.Load() != 1 {
			close(release)
			t.Fatalf("concurrent reset status=%d models=%d responses=%d body=%s",
				second.Code, modelCalls.Load(), responseCalls.Load(), second.Body.String())
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("concurrent reset started a duplicate probe")
	}
	close(release)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK || modelCalls.Load() != 1 || responseCalls.Load() != 1 {
			t.Fatalf("first reset status=%d models=%d responses=%d body=%s",
				first.Code, modelCalls.Load(), responseCalls.Load(), first.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first reset did not finish")
	}
}

func TestAdminNeverLeaksKeysAndRejectsCrossSiteWrites(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://a.invalid/v1", "secret-a"),
		testAccount("b", "https://b.invalid/v1", "secret-b"),
	)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/admin/config", nil)
	request.Host = listenAddress
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
	if response.Code != http.StatusServiceUnavailable {
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

func TestRetryAfterDuration(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"90", 90 * time.Second},
		{now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute},
		{"invalid", accountCooldown},
		{"999999", maxRetryAfter},
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
		ID: id, Name: strings.ToUpper(id), BaseURL: baseURL, APIKey: key, Enabled: true, Revision: 1,
	}
}

func newTestApplication(t *testing.T, strategy string, now func() time.Time, accounts ...accountConfig) *application {
	t.Helper()
	if now == nil {
		now = time.Now
	}
	app := &application{
		cfg: storedConfig{
			Version: configVersion, Accounts: accounts, Strategy: strategy,
			AllowInsecureHTTP: true, GatewayToken: "gateway-token",
		},
		configPath: filepath.Join(t.TempDir(), "config.dat"),
		csrfToken:  "csrf-token", client: &http.Client{}, now: now,
		balanceTimeout: balanceRefreshTime, balanceRoutingTimeout: balanceRoutingTime,
		runtime:            make(map[string]*accountRuntime),
		balanceRefreshGate: make(chan struct{}, 1),
		recovering:         make(map[string]bool),
		probeChanged:       make(chan struct{}),
	}
	for _, account := range accounts {
		app.runtime[account.ID] = &accountRuntime{Revision: account.Revision}
	}
	return app
}

func proxyRequest(app *application, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:4000/v1/responses", strings.NewReader(body))
	request.Host = listenAddress
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
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", origin)
	request.Header.Set("X-CSRF-Token", app.csrfToken)
	response := httptest.NewRecorder()
	app.routes().ServeHTTP(response, request)
	return response
}

func boolPointer(value bool) *bool {
	return &value
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
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
