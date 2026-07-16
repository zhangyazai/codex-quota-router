package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	requestStatsVersion   = 1
	requestStatsFilename  = "request-stats.dat"
	requestStatsSaveDelay = time.Second
)

type accountRequestStats struct {
	SuccessfulRequests uint64 `json:"successfulRequests"`
	FailedRequests     uint64 `json:"failedRequests"`
}

type storedRequestStats struct {
	Version  int                            `json:"version"`
	Accounts map[string]accountRequestStats `json:"accounts"`
}

func requestStatsPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), requestStatsFilename)
}

func loadRequestStats(path string) (map[string]accountRequestStats, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]accountRequestStats), nil
	}
	if err != nil {
		return nil, err
	}
	plain, err := unprotectConfig(data)
	if err != nil {
		return nil, err
	}
	var stored storedRequestStats
	if err := json.Unmarshal(plain, &stored); err != nil {
		return nil, err
	}
	if stored.Version != requestStatsVersion {
		return nil, fmt.Errorf("请求统计版本 %d 不受支持", stored.Version)
	}
	if stored.Accounts == nil {
		stored.Accounts = make(map[string]accountRequestStats)
	}
	return stored.Accounts, nil
}

func saveRequestStats(path string, stats storedRequestStats) error {
	return saveProtectedJSON(path, stats)
}

func (a *application) requestStatsForLocked(id string) accountRequestStats {
	if a.requestStats == nil {
		a.requestStats = make(map[string]accountRequestStats)
	}
	return a.requestStats[id]
}

func (a *application) requestStatsSnapshotLocked() storedRequestStats {
	accounts := make(map[string]accountRequestStats)
	for _, account := range a.cfg.Accounts {
		stats := a.requestStats[account.ID]
		if stats.SuccessfulRequests != 0 || stats.FailedRequests != 0 {
			accounts[account.ID] = stats
		}
	}
	return storedRequestStats{Version: requestStatsVersion, Accounts: accounts}
}

func (a *application) pruneRequestStatsLocked() {
	current := make(map[string]bool, len(a.cfg.Accounts))
	for _, account := range a.cfg.Accounts {
		current[account.ID] = true
	}
	changed := false
	for id := range a.requestStats {
		if !current[id] {
			delete(a.requestStats, id)
			changed = true
		}
	}
	if changed {
		a.markRequestStatsDirtyLocked()
	}
}

func (a *application) markRequestStatsDirtyLocked() {
	if a.requestStatsPath == "" {
		return
	}
	a.requestStatsDirty = true
	if a.requestStatsTimer == nil {
		a.requestStatsTimer = time.AfterFunc(requestStatsSaveDelay, func() {
			_ = a.flushRequestStats()
		})
	}
}

func (a *application) flushRequestStats() error {
	if a.requestStatsPath == "" {
		return nil
	}
	a.requestStatsWriteMu.Lock()
	defer a.requestStatsWriteMu.Unlock()

	a.mu.Lock()
	if a.requestStatsTimer != nil {
		a.requestStatsTimer.Stop()
		a.requestStatsTimer = nil
	}
	if !a.requestStatsDirty {
		a.mu.Unlock()
		return nil
	}
	snapshot := a.requestStatsSnapshotLocked()
	a.requestStatsDirty = false
	a.mu.Unlock()

	persist := a.persistRequestStats
	if persist == nil {
		persist = saveRequestStats
	}
	if err := persist(a.requestStatsPath, snapshot); err != nil {
		a.mu.Lock()
		a.markRequestStatsDirtyLocked()
		a.mu.Unlock()
		return err
	}
	return nil
}
