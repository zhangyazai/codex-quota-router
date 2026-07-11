package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
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
	if healthResponse.Code != http.StatusOK || !strings.Contains(healthResponse.Body.String(), `"version":"0.2.1"`) {
		t.Fatalf("health status=%d body=%s", healthResponse.Code, healthResponse.Body.String())
	}

	indexRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4000/", nil)
	indexRequest.Host = listenAddress
	indexResponse := httptest.NewRecorder()
	app.routes().ServeHTTP(indexResponse, indexRequest)
	body := indexResponse.Body.String()
	if indexResponse.Code != http.StatusOK || !strings.Contains(body, "balancePollInterval = 60000") ||
		!strings.Contains(body, "?automatic=1") {
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
	plain := []byte(`{"version":3,"accounts":[],"strategy":"priority","futureField":"keep-me"}`)
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
		{name: "least used", strategy: strategyLeastUsed, want: []string{"a", "b", "a"}},
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

func TestCodexSnippetDisablesClientRetries(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now)
	snippet := app.codexSnippet()
	for _, setting := range []string{"request_max_retries = 0", "stream_max_retries = 0"} {
		if !strings.Contains(snippet, setting) {
			t.Fatalf("Codex snippet missing %q", setting)
		}
	}
}

func TestHighestBalanceFallsBackToLeastUsed(t *testing.T) {
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

	app.runtime["a"].Balance = balanceSnapshot{Status: "ok", Amount: 100, Unit: "USD", UpdatedAt: now}
	app.runtime["b"].Balance = balanceSnapshot{Status: "ok", Amount: 100, Unit: "CNY", UpdatedAt: now}
	status = app.status()
	if status.EffectiveStrategy != strategyLeastUsed || status.FallbackReason != "balance_unit_mismatch" {
		t.Fatalf("unit mismatch did not fall back: %#v", status)
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
		})
	}
}

func TestConcurrentAccountResultsKeepBlocksAndUseCurrentVerification(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://a.invalid/v1", "key-a"),
	)
	selectedBeforeBlock := app.cfg.Accounts[0]
	app.blockAccount(selectedBeforeBlock, "quota")
	app.markAccountVerified(selectedBeforeBlock)
	if !app.cfg.Accounts[0].Verified || app.cfg.Accounts[0].BlockedReason != "quota" {
		t.Fatalf("late success cleared block: %#v", app.cfg.Accounts[0])
	}

	app = newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://a.invalid/v1", "key-a"),
	)
	selectedWhileUnverified := app.cfg.Accounts[0]
	app.markAccountVerified(selectedWhileUnverified)
	app.handleAccountUnauthorized(selectedWhileUnverified)
	if app.cfg.Accounts[0].BlockedReason != "unauthorized" || !app.runtime["a"].CooldownUntil.IsZero() {
		t.Fatalf("401 used stale verification state: account=%#v runtime=%#v", app.cfg.Accounts[0], app.runtime["a"])
	}
}

func TestOrdinary429CoolsAndNoNextForwardsLastError(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
	}))
	defer upstream.Close()
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", upstream.URL, "key-a"),
	)
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("last error was not forwarded: status=%d body=%q", response.Code, response.Body.String())
	}
	if !now.Before(app.runtime["a"].CooldownUntil) || app.cfg.Accounts[0].BlockedReason != "" {
		t.Fatalf("ordinary 429 state is wrong: runtime=%#v account=%#v", app.runtime["a"], app.cfg.Accounts[0])
	}
	second := proxyRequest(app, `{}`)
	if second.Code != http.StatusServiceUnavailable || calls.Load() != 1 {
		t.Fatalf("cooldown was ignored: status=%d calls=%d", second.Code, calls.Load())
	}
	now = now.Add(accountCooldown + time.Second)
	proxyRequest(app, `{}`)
	if calls.Load() != 2 {
		t.Fatalf("account was not retried after cooldown: calls=%d", calls.Load())
	}
}

func TestEachAccountIsAttemptedAtMostOncePerRequest(t *testing.T) {
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

func TestNetworkAnd5xxAreNeverReplayed(t *testing.T) {
	for _, test := range []struct {
		name       string
		firstReply func(*http.Request) (*http.Response, error)
		wantStatus int
	}{
		{
			name: "network error",
			firstReply: func(*http.Request) (*http.Response, error) {
				return nil, errors.New("network failed")
			},
			wantStatus: http.StatusBadGateway,
		},
		{
			name: "server error",
			firstReply: func(r *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusInternalServerError, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader("server failed")), Request: r,
				}, nil
			},
			wantStatus: http.StatusInternalServerError,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var secondCalls atomic.Int32
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "first.invalid" {
					return test.firstReply(r)
				}
				secondCalls.Add(1)
				return &http.Response{
					StatusCode: http.StatusOK, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader("second")), Request: r,
				}, nil
			})
			app := newTestApplication(t, strategyPriority, time.Now,
				testAccount("a", "https://first.invalid/v1", "key-a"),
				testAccount("b", "https://second.invalid/v1", "key-b"),
			)
			app.client = &http.Client{Transport: transport}
			response := proxyRequest(app, `{}`)
			if response.Code != test.wantStatus || secondCalls.Load() != 0 {
				t.Fatalf("response=%d secondCalls=%d", response.Code, secondCalls.Load())
			}
		})
	}
}

func TestStartedSSEStreamIsNeverReplayed(t *testing.T) {
	var secondCalls atomic.Int32
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "second.invalid" {
			secondCalls.Add(1)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       &oneChunkThenError{chunk: []byte("data: partial\n\n")}, Request: r,
		}, nil
	})
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://first.invalid/v1", "key-a"),
		testAccount("b", "https://second.invalid/v1", "key-b"),
	)
	app.client = &http.Client{Transport: transport}
	response := proxyRequest(app, `{}`)
	if response.Code != http.StatusOK || response.Body.String() != "data: partial\n\n" {
		t.Fatalf("unexpected stream: status=%d body=%q", response.Code, response.Body.String())
	}
	if secondCalls.Load() != 0 {
		t.Fatalf("started stream was replayed %d time(s)", secondCalls.Load())
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
	test := testRequest{
		AccountID: "a",
		Candidate: &accountInput{BaseURL: "https://attacker.invalid/v1"},
		TestModel: "test-model",
	}
	response = adminJSON(app, http.MethodPost, "/admin/test", test, "http://127.0.0.1:4000")
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
		if balance.Status != "ok" || balance.Amount != 95 || balance.Unit != "USD" {
			t.Fatalf("unexpected dashboard balance: %#v paths=%v", balance, paths)
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
		if balance.Status != "ok" || balance.Unit != "" {
			t.Fatalf("unknown unit became comparable: %#v", balance)
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
		if balance.Status != "error" {
			t.Fatalf("401 status=%q, want error", balance.Status)
		}
	})
}

func TestNumberValueRejectsNonFiniteValues(t *testing.T) {
	for _, value := range []any{"NaN", "+Inf", math.NaN(), math.Inf(1)} {
		if number, ok := numberValue(value); ok {
			t.Fatalf("numberValue(%v)=%v, want rejected", value, number)
		}
	}
}

func TestHighestBalanceRefreshesStaleAccountsBeforeSelection(t *testing.T) {
	var usageCalls atomic.Int32
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
		case "/api/status":
			_, _ = io.WriteString(w, `{"data":{"quota_per_unit":500000,"quota_display_type":"USD"}}`)
		default:
			_, _ = io.WriteString(w, "ok")
		}
	}))
	defer server.Close()
	app := newTestApplication(t, strategyHighestBalance, time.Now,
		testAccount("a", server.URL+"/v1", "key-a"),
		testAccount("b", server.URL+"/v1", "key-b"),
	)
	account, ok := app.selectAccount(context.Background(), map[string]bool{})
	if !ok || account.ID != "b" || usageCalls.Load() != 2 {
		t.Fatalf("selection=%q ok=%v usageCalls=%d", account.ID, ok, usageCalls.Load())
	}
	if app.status().EffectiveStrategy != strategyHighestBalance {
		t.Fatalf("fresh comparable balances did not enable highest_balance: %#v", app.status())
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
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
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
	entered := make(chan struct{}, len(accounts)*4)
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
	for index := 0; index < balanceWorkers; index++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("balance workers did not start")
		}
	}
	select {
	case <-entered:
		t.Fatal("balance refresh exceeded worker limit")
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
	if maximum.Load() > balanceWorkers {
		t.Fatalf("maximum balance concurrency=%d, want <=%d", maximum.Load(), balanceWorkers)
	}
}

func TestAdminTestInheritsSavedKeyAndOnlyMatchingCandidateVerifies(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer saved-key" {
			t.Fatalf("wrong authorization: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses":
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
	account := testAccount("a", server.URL+"/v1", "saved-key")
	account.BlockedReason = "quota"
	app := newTestApplication(t, strategyPriority, time.Now, account)
	request := testRequest{
		AccountID: "a", Candidate: &accountInput{BaseURL: server.URL + "/v1"},
		TestModel: "test-model", AllowInsecureHTTP: boolPointer(true),
	}
	response := adminJSON(app, http.MethodPost, "/admin/test", request, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok":true`) ||
		!strings.Contains(response.Body.String(), `"amount":1`) {
		t.Fatalf("test failed: status=%d body=%s", response.Code, response.Body.String())
	}
	if !app.cfg.Accounts[0].Verified || app.cfg.Accounts[0].BlockedReason != "" {
		t.Fatalf("matching test did not clear state: %#v", app.cfg.Accounts[0])
	}

	app.cfg.Accounts[0].Verified = false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/responses" {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer other.Close()
	request.Candidate = &accountInput{BaseURL: other.URL + "/v1", APIKey: "other-key"}
	response = adminJSON(app, http.MethodPost, "/admin/test", request, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || app.cfg.Accounts[0].Verified {
		t.Fatalf("unsaved candidate changed saved verification: status=%d account=%#v", response.Code, app.cfg.Accounts[0])
	}
}

func TestAccountResetClearsBlockAndRefreshesBalance(t *testing.T) {
	var balanceCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
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
	app := newTestApplication(t, strategyPriority, time.Now, account)
	staleRequestAccount := app.cfg.Accounts[0]
	response := adminJSON(app, http.MethodPost, "/admin/accounts/reset", map[string]string{"id": "a"}, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK || app.cfg.Accounts[0].BlockedReason != "" || balanceCalls.Load() != 1 ||
		app.runtime["a"].Balance.Status != "ok" || app.cfg.Accounts[0].Revision != staleRequestAccount.Revision+1 {
		t.Fatalf("reset failed: status=%d account=%#v runtime=%#v calls=%d body=%s",
			response.Code, app.cfg.Accounts[0], app.runtime["a"], balanceCalls.Load(), response.Body.String())
	}
	app.blockAccount(staleRequestAccount, "quota")
	if app.cfg.Accounts[0].BlockedReason != "" {
		t.Fatalf("pre-reset request reblocked account: %#v", app.cfg.Accounts[0])
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
			Version: configVersion, Accounts: accounts, Strategy: strategy, TestModel: "test-model",
			AllowInsecureHTTP: true, GatewayToken: "gateway-token",
		},
		configPath: filepath.Join(t.TempDir(), "config.dat"),
		csrfToken:  "csrf-token", client: &http.Client{}, now: now,
		runtime: make(map[string]*accountRuntime),
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
