package main

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestRequestStatisticsPersistAndClearPerAccount(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, configFilename)
	accounts := []accountConfig{
		testAccount("a", "https://a.invalid/v1", "key-a"),
		testAccount("b", "https://b.invalid/v1", "key-b"),
	}
	cfg := storedConfig{
		Version: configVersion, Accounts: accounts, Strategy: strategyPriority,
		GatewayTokens: []gatewayTokenConfig{{ID: "token", Name: "Token", Token: "gateway-token"}},
	}
	if err := saveConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	statsPath := requestStatsPath(configPath)
	if err := saveRequestStats(statsPath, storedRequestStats{
		Version: requestStatsVersion,
		Accounts: map[string]accountRequestStats{
			"a": {SuccessfulRequests: 2, FailedRequests: 1},
			"b": {SuccessfulRequests: 4, FailedRequests: 3},
		},
	}); err != nil {
		t.Fatal(err)
	}

	app, err := newApplication(configPath, &http.Client{}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	status := app.status()
	if status.Accounts[0].SuccessfulRequests != 2 || status.Accounts[0].FailedRequests != 1 ||
		status.Accounts[1].SuccessfulRequests != 4 || status.Accounts[1].FailedRequests != 3 {
		t.Fatalf("loaded request statistics=%#v", status.Accounts)
	}

	app.recordAccountRequest(accounts[0], true)
	if err := app.flushRequestStats(); err != nil {
		t.Fatal(err)
	}
	stored, err := loadRequestStats(statsPath)
	if err != nil {
		t.Fatal(err)
	}
	if stored["a"].SuccessfulRequests != 3 || stored["a"].FailedRequests != 1 {
		t.Fatalf("persisted account a statistics=%#v", stored["a"])
	}

	response := adminJSON(app, http.MethodPost, "/admin/accounts/statistics/clear", map[string]string{"id": "a"}, "http://127.0.0.1:4000")
	if response.Code != http.StatusOK {
		t.Fatalf("clear response=%d body=%s", response.Code, response.Body.String())
	}
	status = app.status()
	if status.Accounts[0].SuccessfulRequests != 0 || status.Accounts[0].FailedRequests != 0 ||
		status.Accounts[1].SuccessfulRequests != 4 || status.Accounts[1].FailedRequests != 3 {
		t.Fatalf("cleared request statistics=%#v", status.Accounts)
	}
	stored, err = loadRequestStats(statsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := stored["a"]; exists {
		t.Fatalf("cleared account a still persisted: %#v", stored["a"])
	}
	if stored["b"].SuccessfulRequests != 4 || stored["b"].FailedRequests != 3 {
		t.Fatalf("account b statistics changed: %#v", stored["b"])
	}
}
