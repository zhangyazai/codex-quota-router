package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"sort"
	"time"
)

const (
	leastUsedBatchSize   = 30
	accountCooldown      = time.Minute
	upstreamFailureReset = 5 * time.Minute
	accountHealthTTL     = 5 * time.Minute
	maxSoftProbeDelay    = 30 * time.Second
	softProbeRecheck     = time.Second

	accountHealthUnverified    = "unverified"
	accountHealthRecentSuccess = "recent_success"
	accountHealthRecentFailure = "recent_failure"

	strategyQuotaReset = "quota_reset"
)

type accountRuntime struct {
	Revision           int
	CooldownUntil      time.Time
	CooldownReason     string
	UpstreamFailures   int
	RecoveryFailures   int
	LastFailureAt      time.Time
	LastRecoveryAt     time.Time
	LastUnauthorizedAt time.Time
	FailureBarrierAt   time.Time
	HealthState        string
	HealthCheckedAt    time.Time
	AssignedRequests   uint64
	LastUsedAt         time.Time
	Balance            balanceSnapshot
	NewAPISession      string
	NewAPIUserID       int
	NewAPIAuthHash     [sha256.Size]byte
	CodexUsage         codexUsageSnapshot
	ProbeInFlight      bool
}

type accountCandidate struct {
	account accountConfig
	index   int
	runtime *accountRuntime
	probe   bool
}

type accountStatus struct {
	ID                 string              `json:"id"`
	Name               string              `json:"name"`
	State              string              `json:"state"`
	HealthState        string              `json:"healthState"`
	Verified           bool                `json:"verified"`
	BlockedReason      string              `json:"blockedReason,omitempty"`
	CoolingDown        bool                `json:"coolingDown"`
	CooldownUntil      string              `json:"cooldownUntil,omitempty"`
	NextProbeAt        string              `json:"nextProbeAt,omitempty"`
	CooldownReason     string              `json:"cooldownReason,omitempty"`
	UpstreamFailures   int                 `json:"upstreamFailures,omitempty"`
	AssignedRequests   uint64              `json:"assignedRequests"`
	SuccessfulRequests uint64              `json:"successfulRequests"`
	FailedRequests     uint64              `json:"failedRequests"`
	LastUsedAt         string              `json:"lastUsedAt,omitempty"`
	Balance            publicBalance       `json:"balance"`
	AuthType           string              `json:"authType"`
	QuotaResetAt       string              `json:"quotaResetAt,omitempty"`
	CodexUsage         *codexUsageSnapshot `json:"codexUsage,omitempty"`
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
		nextProbeAt := ""
		if probeAt := accountNextProbeAt(account, runtime); !probeAt.IsZero() {
			if probeAt.Before(now) {
				probeAt = now
			}
			nextProbeAt = probeAt.UTC().Format(time.RFC3339)
		}
		lastUsedAt := ""
		if !runtime.LastUsedAt.IsZero() {
			lastUsedAt = runtime.LastUsedAt.UTC().Format(time.RFC3339)
		}
		quotaResetAt := accountRecoveryResetAt(account, runtime, now)
		quotaResetText := ""
		if !quotaResetAt.IsZero() {
			quotaResetText = quotaResetAt.UTC().Format(time.RFC3339)
		}
		var usage *codexUsageSnapshot
		if normalizeAccountAuthType(account.AuthType) == accountAuthCodexOAuth {
			snapshot := runtime.CodexUsage
			if !snapshot.CheckedAt.IsZero() {
				if snapshot.PlanType == "" {
					snapshot.PlanType = account.CodexPlanType
				}
				usage = &snapshot
			}
		}
		requestStats := a.requestStatsForLocked(account.ID)
		accounts = append(accounts, accountStatus{
			ID:                 account.ID,
			Name:               account.Name,
			State:              state,
			HealthState:        accountHealthState(runtime, now),
			Verified:           account.Verified,
			BlockedReason:      account.BlockedReason,
			CoolingDown:        cooldownUntil != "",
			CooldownUntil:      cooldownUntil,
			NextProbeAt:        nextProbeAt,
			CooldownReason:     runtime.CooldownReason,
			UpstreamFailures:   runtime.UpstreamFailures,
			AssignedRequests:   runtime.AssignedRequests,
			SuccessfulRequests: requestStats.SuccessfulRequests,
			FailedRequests:     requestStats.FailedRequests,
			LastUsedAt:         lastUsedAt,
			Balance:            publicBalanceAt(runtime.Balance, now),
			AuthType:           normalizeAccountAuthType(account.AuthType),
			QuotaResetAt:       quotaResetText,
			CodexUsage:         usage,
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
	case hardBlockedReason(account.BlockedReason):
		return "blocked"
	case now.Before(runtime.CooldownUntil) && runtime.CooldownReason == "upstream_failures" && runtime.UpstreamFailures >= 3:
		return "temporarily_disabled"
	case now.Before(runtime.CooldownUntil):
		return "cooldown"
	case !codexCreditsRoutingAllowed(account, runtime, now):
		if runtime != nil && runtime.CodexUsage.Fresh(now) && runtime.CodexUsage.BlocksRouting(true) {
			return "quota_exhausted"
		}
		return "unavailable"
	case recoverableBlockedReason(account.BlockedReason):
		return "cooldown"
	default:
		return "available"
	}
}

func codexCreditsRoutingAllowed(account accountConfig, runtime *accountRuntime, now time.Time) bool {
	if normalizeAccountAuthType(account.AuthType) != accountAuthCodexOAuth || !account.DisableCodexCredits {
		return true
	}
	return runtime != nil && runtime.CodexUsage.Fresh(now) && !runtime.CodexUsage.BlocksRouting(true)
}

func accountNextProbeAt(account accountConfig, runtime *accountRuntime) time.Time {
	if runtime == nil || !account.Enabled || !accountConfigured(account) || hardBlockedReason(account.BlockedReason) {
		return time.Time{}
	}
	if runtime.CooldownReason == "upstream_failures" && runtime.UpstreamFailures > 0 {
		return softProbeAt(runtime)
	}
	if recoverableBlockedReason(account.BlockedReason) || runtime.CooldownReason != "" {
		return runtime.CooldownUntil
	}
	return time.Time{}
}

func automaticSubscriptionQuota(account accountConfig, runtime *accountRuntime, now time.Time) *subscriptionQuota {
	if normalizeAccountAuthType(account.AuthType) != accountAuthAPIKey || runtime == nil {
		return nil
	}
	mode := normalizeNewAPIAuthMode(account.NewAPIAuthMode)
	if mode != newAPIAuthPassword && mode != newAPIAuthAccessToken {
		return nil
	}
	balance := runtime.Balance
	if balance.Status != "ok" || !balanceFresh(balance, now) {
		return nil
	}
	switch balance.RefreshStatus {
	case "", balanceRefreshOK, balanceRefreshPartial:
	default:
		return nil
	}
	if balance.Subscription == nil {
		return nil
	}
	return balance.Subscription
}

func automaticSubscriptionPriorityQuota(account accountConfig, runtime *accountRuntime, now time.Time) *subscriptionQuota {
	subscription := automaticSubscriptionQuota(account, runtime, now)
	if subscription == nil || subscription.Unlimited || subscription.Remaining <= 0 {
		return nil
	}
	switch subscription.BillingPreference {
	case "", "subscription_first", "subscription_only":
		return subscription
	default:
		return nil
	}
}

func parseSubscriptionResetAt(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	resetAt, err := time.Parse(time.RFC3339, value)
	return resetAt, err == nil
}

func futureSubscriptionResetAt(value string, now time.Time) time.Time {
	resetAt, ok := parseSubscriptionResetAt(value)
	if !ok || !resetAt.After(now) {
		return time.Time{}
	}
	return resetAt
}

func manualQuotaResetAt(account accountConfig, now time.Time) time.Time {
	resetAt, ok := quotaResetEffectiveAt(
		account.QuotaResetPeriod,
		account.QuotaResetTimezone,
		account.QuotaResetEvery,
		account.QuotaResetUnit,
		account.QuotaResetAnchorAt,
		now,
	)
	if !ok {
		return time.Time{}
	}
	return resetAt
}

func accountPriorityResetAt(account accountConfig, runtime *accountRuntime, now time.Time) time.Time {
	if normalizeAccountAuthType(account.AuthType) == accountAuthCodexOAuth {
		if runtime == nil {
			return time.Time{}
		}
		return runtime.CodexUsage.EffectiveResetAtFor(account.DisableCodexCredits, now)
	}
	if normalizeAccountAuthType(account.AuthType) != accountAuthAPIKey {
		return time.Time{}
	}
	switch normalizeNewAPIAuthMode(account.NewAPIAuthMode) {
	case newAPIAuthPassword, newAPIAuthAccessToken:
		subscription := automaticSubscriptionPriorityQuota(account, runtime, now)
		if subscription == nil {
			return time.Time{}
		}
		return futureSubscriptionResetAt(subscription.PriorityResetAt, now)
	case newAPIAuthAPIKey:
		return manualQuotaResetAt(account, now)
	default:
		return time.Time{}
	}
}

func accountRecoveryResetAt(account accountConfig, runtime *accountRuntime, now time.Time) time.Time {
	if normalizeAccountAuthType(account.AuthType) == accountAuthCodexOAuth {
		if runtime == nil {
			return time.Time{}
		}
		return runtime.CodexUsage.EffectiveResetAtFor(account.DisableCodexCredits, now)
	}
	if normalizeAccountAuthType(account.AuthType) != accountAuthAPIKey {
		return time.Time{}
	}
	switch normalizeNewAPIAuthMode(account.NewAPIAuthMode) {
	case newAPIAuthPassword, newAPIAuthAccessToken:
		subscription := automaticSubscriptionQuota(account, runtime, now)
		if subscription == nil || subscription.Unlimited || subscription.BillingPreference == "wallet_only" {
			return time.Time{}
		}
		return futureSubscriptionResetAt(subscription.ResetAt, now)
	case newAPIAuthAPIKey:
		return manualQuotaResetAt(account, now)
	default:
		return time.Time{}
	}
}

func newAPISubscriptionRefreshDue(account accountConfig, runtime *accountRuntime, now time.Time) bool {
	if normalizeAccountAuthType(account.AuthType) != accountAuthAPIKey {
		return false
	}
	mode := normalizeNewAPIAuthMode(account.NewAPIAuthMode)
	if mode != newAPIAuthPassword && mode != newAPIAuthAccessToken {
		return false
	}
	if runtime == nil {
		return true
	}
	return balanceRefreshDue(runtime.Balance, now, balanceTTL)
}

func recoverableBlockedReason(reason string) bool {
	return reason == "quota" || reason == "unauthorized"
}

func hardBlockedReason(reason string) bool {
	return reason != "" && !recoverableBlockedReason(reason)
}

func accountHealthState(runtime *accountRuntime, now time.Time) string {
	if runtime.HealthCheckedAt.IsZero() || now.After(runtime.HealthCheckedAt.Add(accountHealthTTL)) {
		return accountHealthUnverified
	}
	if runtime.HealthState == accountHealthRecentSuccess || runtime.HealthState == accountHealthRecentFailure {
		return runtime.HealthState
	}
	return accountHealthUnverified
}

func (a *application) effectiveStrategyLocked(now time.Time) (string, string) {
	if !isBalanceStrategy(a.cfg.Strategy) {
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
	return a.cfg.Strategy, ""
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
	account, ok, _ := a.selectAccountWithMode(ctx, attempted, false, nil, "", 0)
	return account, ok
}

func (a *application) selectProxyAccount(ctx context.Context, attempted map[string]bool) (accountConfig, bool, bool) {
	return a.selectAccountWithMode(ctx, attempted, true, nil, "", 0)
}

func (a *application) selectProxyAccountFor(
	ctx context.Context,
	attempted map[string]bool,
	eligible func(accountConfig) bool,
	preferredAccountID string,
	preferredAccountRevision int,
) (accountConfig, bool, bool) {
	return a.selectAccountWithMode(ctx, attempted, true, eligible, preferredAccountID, preferredAccountRevision)
}

func (a *application) nextProxyRecoveryAt(attempted map[string]bool, eligible func(accountConfig) bool) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	var next time.Time
	for _, account := range a.cfg.Accounts {
		if attempted[account.ID] || !account.Enabled || !accountConfigured(account) ||
			hardBlockedReason(account.BlockedReason) || (eligible != nil && !eligible(account)) {
			continue
		}
		probeAt := accountNextProbeAt(account, a.runtimeForLocked(account))
		if !probeAt.After(now) {
			continue
		}
		if next.IsZero() || probeAt.Before(next) {
			next = probeAt
		}
	}
	return next
}

func (a *application) selectAccountWithMode(
	ctx context.Context,
	attempted map[string]bool,
	allowSoftProbe bool,
	eligible func(accountConfig) bool,
	preferredAccountID string,
	preferredAccountRevision int,
) (accountConfig, bool, bool) {
	a.mu.Lock()
	strategy := a.cfg.Strategy
	now := a.now()
	protectedUsage := make(map[string]bool)
	for _, account := range a.cfg.Accounts {
		if attempted[account.ID] || !account.Enabled || !accountConfigured(account) ||
			hardBlockedReason(account.BlockedReason) || (eligible != nil && !eligible(account)) ||
			normalizeAccountAuthType(account.AuthType) != accountAuthCodexOAuth || !account.DisableCodexCredits {
			continue
		}
		runtime := a.runtimeForLocked(account)
		if !now.Before(runtime.CooldownUntil) && !runtime.CodexUsage.Fresh(now) {
			protectedUsage[account.ID] = true
		}
	}
	var stale map[string]bool
	if isBalanceStrategy(strategy) {
		stale = make(map[string]bool)
		for _, account := range a.cfg.Accounts {
			runtime := a.runtimeForLocked(account)
			if accountState(account, runtime, now) == "available" && balanceRefreshDue(runtime.Balance, now, balanceTTL) {
				stale[account.ID] = true
			}
		}
	} else if strategy == strategyQuotaReset {
		stale = make(map[string]bool)
		for _, account := range a.cfg.Accounts {
			runtime := a.runtimeForLocked(account)
			if accountState(account, runtime, now) == "available" &&
				newAPISubscriptionRefreshDue(account, runtime, now) {
				stale[account.ID] = true
			}
		}
	}
	a.mu.Unlock()
	if len(protectedUsage) != 0 {
		usageCtx, cancel := context.WithTimeout(ctx, codexUsageRequestTimeout)
		a.refreshCodexUsageAccounts(usageCtx, protectedUsage)
		cancel()
	}
	if strategy == strategyQuotaReset {
		a.scheduleCodexUsageRefresh()
	}
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
	for {
		a.mu.Lock()
		now := a.now()
		regular := make([]accountCandidate, 0, len(a.cfg.Accounts))
		probes := make([]accountCandidate, 0, len(a.cfg.Accounts))
		emergency := make([]accountCandidate, 0, len(a.cfg.Accounts))
		softRemaining := false
		var wakeAt time.Time
		for index, account := range a.cfg.Accounts {
			if attempted[account.ID] || !account.Enabled || !accountConfigured(account) ||
				hardBlockedReason(account.BlockedReason) || (eligible != nil && !eligible(account)) {
				continue
			}
			runtime := a.runtimeForLocked(account)
			if !codexCreditsRoutingAllowed(account, runtime, now) {
				continue
			}
			state := accountState(account, runtime, now)
			softFailure := recoverableBlockedReason(account.BlockedReason) || runtime.CooldownReason != ""
			if !allowSoftProbe || !softFailure {
				if state == "available" {
					regular = append(regular, accountCandidate{account: account, index: index, runtime: runtime})
				}
				continue
			}
			waitForProbe := runtime.CooldownReason == "upstream_failures" && runtime.UpstreamFailures > 0
			if runtime.ProbeInFlight {
				softRemaining = true
				continue
			}
			softRemaining = softRemaining || waitForProbe
			candidate := accountCandidate{account: account, index: index, runtime: runtime, probe: true}
			probeAt := runtime.CooldownUntil
			if waitForProbe {
				probeAt = softProbeAt(runtime)
			}
			if !now.Before(probeAt) && (state == "available" || !waitForProbe) {
				probes = append(probes, candidate)
				continue
			}
			if waitForProbe && !now.Before(probeAt) {
				emergency = append(emergency, candidate)
			} else if waitForProbe && (wakeAt.IsZero() || probeAt.Before(wakeAt)) {
				wakeAt = probeAt
			}
		}

		candidates := regular
		forcedProbe := false
		if len(regular) != 0 {
			candidates = append(candidates, probes...)
			sort.SliceStable(candidates, func(left, right int) bool {
				return candidates[left].index < candidates[right].index
			})
		} else if len(probes) != 0 {
			candidates = probes
			forcedProbe = true
		} else if len(emergency) != 0 {
			candidates = emergency
			forcedProbe = true
		}
		if len(candidates) == 0 {
			a.leastUsedBatchAccount = ""
			a.leastUsedBatchLeft = 0
			if !allowSoftProbe || !softRemaining {
				a.mu.Unlock()
				return accountConfig{}, false, false
			}
			if a.probeChanged == nil {
				a.probeChanged = make(chan struct{})
			}
			changed := a.probeChanged
			a.mu.Unlock()
			if wakeAt.IsZero() {
				select {
				case <-ctx.Done():
					return accountConfig{}, false, false
				case <-changed:
				case <-time.After(softProbeRecheck):
				}
			} else {
				delay := wakeAt.Sub(now)
				if delay > softProbeRecheck {
					delay = softProbeRecheck
				}
				if delay < 0 {
					delay = 0
				}
				select {
				case <-ctx.Done():
					return accountConfig{}, false, false
				case <-changed:
				case <-time.After(delay):
				}
			}
			continue
		}

		if forcedProbe {
			sort.SliceStable(candidates, func(left, right int) bool {
				if candidates[left].runtime.UpstreamFailures != candidates[right].runtime.UpstreamFailures {
					return candidates[left].runtime.UpstreamFailures < candidates[right].runtime.UpstreamFailures
				}
				return candidates[left].runtime.LastFailureAt.Before(candidates[right].runtime.LastFailureAt)
			})
		}
		effective, _ := a.effectiveStrategyLocked(now)
		if forcedProbe || effective != strategyLeastUsed {
			a.leastUsedBatchAccount = ""
			a.leastUsedBatchLeft = 0
		}
		selected := 0
		preferredSelected := false
		if !forcedProbe && preferredAccountID != "" {
			for index := range candidates {
				if !candidates[index].probe && candidates[index].account.ID == preferredAccountID &&
					candidates[index].account.Revision == preferredAccountRevision {
					selected = index
					preferredSelected = true
					break
				}
			}
		}
		if !forcedProbe && !preferredSelected {
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
				batchFound := false
				if a.leastUsedBatchLeft > 0 {
					for index := range candidates {
						if candidates[index].account.ID == a.leastUsedBatchAccount {
							selected = index
							batchFound = true
							break
						}
					}
				}
				if !batchFound {
					for index := 1; index < len(candidates); index++ {
						if candidates[index].runtime.AssignedRequests < candidates[selected].runtime.AssignedRequests {
							selected = index
						}
					}
					a.leastUsedBatchAccount = candidates[selected].account.ID
					a.leastUsedBatchLeft = leastUsedBatchSize
				}
			case strategyQuotaReset:
				for index := 1; index < len(candidates); index++ {
					leftReset := accountPriorityResetAt(candidates[index].account, candidates[index].runtime, now)
					rightReset := accountPriorityResetAt(candidates[selected].account, candidates[selected].runtime, now)
					if (!leftReset.IsZero() && rightReset.IsZero()) ||
						(!leftReset.IsZero() && !rightReset.IsZero() && leftReset.Before(rightReset)) {
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
			case strategyLowestBalance:
				for index := 1; index < len(candidates); index++ {
					left := candidates[index]
					right := candidates[selected]
					switch {
					case !left.runtime.Balance.Unlimited && right.runtime.Balance.Unlimited:
						selected = index
					case !left.runtime.Balance.Unlimited && !right.runtime.Balance.Unlimited &&
						left.runtime.Balance.Amount < right.runtime.Balance.Amount:
						selected = index
					}
				}
			}
		}
		chosen := candidates[selected]
		if !forcedProbe && !preferredSelected && effective == strategyLeastUsed &&
			chosen.account.ID == a.leastUsedBatchAccount && a.leastUsedBatchLeft > 0 {
			a.leastUsedBatchLeft--
		}
		if chosen.probe {
			chosen.runtime.ProbeInFlight = true
		}
		chosen.runtime.AssignedRequests++
		chosen.runtime.LastUsedAt = now
		a.lastRoutedAccountID = chosen.account.ID
		a.lastRoutedAccountName = chosen.account.Name
		a.mu.Unlock()
		return chosen.account, true, chosen.probe
	}
}

func (a *application) scheduleCodexUsageRefresh() {
	if a.codexUsage == nil || !a.codexUsageRefreshDue() {
		return
	}
	a.mu.Lock()
	if a.codexUsageRefreshGate == nil {
		a.codexUsageRefreshGate = make(chan struct{}, 1)
	}
	gate := a.codexUsageRefreshGate
	a.mu.Unlock()
	select {
	case gate <- struct{}{}:
	default:
		return
	}
	go func() {
		defer func() { <-gate }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		a.refreshCodexUsageAccounts(ctx, nil)
	}()
}

func (a *application) codexUsageRefreshDue() bool {
	now := a.now()
	a.mu.Lock()
	accounts := append([]accountConfig(nil), a.cfg.Accounts...)
	a.mu.Unlock()
	for _, account := range accounts {
		if !account.Enabled || normalizeAccountAuthType(account.AuthType) != accountAuthCodexOAuth ||
			!codexAuthenticated(account) {
			continue
		}
		snapshot, ok := a.codexUsage.Cached(account.ID)
		if !ok || !snapshot.Fresh(now) {
			return true
		}
	}
	return false
}

func softProbeAt(runtime *accountRuntime) time.Time {
	delay := upstreamFailureCooldown(runtime.UpstreamFailures)
	if delay > maxSoftProbeDelay {
		delay = maxSoftProbeDelay
	}
	probeAt := runtime.LastFailureAt.Add(delay)
	if !runtime.CooldownUntil.IsZero() && runtime.CooldownUntil.Before(probeAt) {
		return runtime.CooldownUntil
	}
	return probeAt
}

func (a *application) probeReleasesOnHeaders(account accountConfig) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	runtime := a.runtime[account.ID]
	return runtime != nil && runtime.Revision == account.Revision && runtime.CooldownReason == "upstream_failures"
}

func (a *application) finishAccountProbe(account accountConfig, accepted bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if runtime := a.runtime[account.ID]; runtime != nil && runtime.Revision == account.Revision {
		if accepted && runtime.CooldownReason == "upstream_failures" {
			runtime.CooldownUntil = time.Time{}
			runtime.CooldownReason = ""
		}
		runtime.ProbeInFlight = false
	}
	a.signalProbeChangedLocked()
}

func (a *application) signalProbeChangedLocked() {
	if a.probeChanged == nil {
		a.probeChanged = make(chan struct{})
		return
	}
	close(a.probeChanged)
	a.probeChanged = make(chan struct{})
}

func (a *application) markAccountVerified(expected accountConfig, startedAt ...time.Time) {
	a.setAccountVerified(expected, startedAt...)
}

func (a *application) setAccountVerified(expected accountConfig, startedAt ...time.Time) {
	a.mu.Lock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		a.mu.Unlock()
		return
	}
	current := a.cfg.Accounts[index]
	runtime := a.runtimeForLocked(current)
	if len(startedAt) != 0 && !startedAt[0].IsZero() && !runtime.FailureBarrierAt.IsZero() &&
		startedAt[0].Before(runtime.FailureBarrierAt) {
		a.mu.Unlock()
		return
	}
	clearBlock := len(startedAt) != 0 && recoverableBlockedReason(current.BlockedReason)
	if current.Verified && !clearBlock {
		markRuntimeSuccessLocked(runtime, a.now())
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	a.configMu.Lock()
	a.mu.Lock()
	index = a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		a.mu.Unlock()
		a.configMu.Unlock()
		return
	}
	current = a.cfg.Accounts[index]
	runtime = a.runtimeForLocked(current)
	if len(startedAt) != 0 && !startedAt[0].IsZero() && !runtime.FailureBarrierAt.IsZero() &&
		startedAt[0].Before(runtime.FailureBarrierAt) {
		a.mu.Unlock()
		a.configMu.Unlock()
		return
	}
	clearBlock = len(startedAt) != 0 && recoverableBlockedReason(current.BlockedReason)
	if current.Verified && !clearBlock {
		markRuntimeSuccessLocked(runtime, a.now())
		a.mu.Unlock()
		a.configMu.Unlock()
		return
	}
	now := a.now()
	markRuntimeSuccessLocked(runtime, now)
	candidate := cloneConfig(a.cfg)
	candidate.Accounts[index].Verified = true
	if clearBlock {
		candidate.Accounts[index].BlockedReason = ""
		candidate.LastSwitchReason = "account_recovered"
		candidate.LastSwitchAt = now.UTC().Format(time.RFC3339)
	}
	a.mu.Unlock()
	if err := a.writeConfig(candidate); err != nil {
		a.configMu.Unlock()
		a.logEvent(context.Background(), slog.LevelError, "account_state_persist_failed",
			"account_ref", logReference(expected.ID), "state", "verified", "error_kind", logErrorKind(err))
		return
	}
	a.mu.Lock()
	a.cfg = candidate
	a.mu.Unlock()
	a.configMu.Unlock()
}

func markRuntimeSuccessLocked(runtime *accountRuntime, now time.Time) {
	clearUpstreamFailureState(runtime)
	clearRecoveryState(runtime)
	runtime.FailureBarrierAt = now
	runtime.LastUnauthorizedAt = time.Time{}
	runtime.CooldownUntil = time.Time{}
	runtime.CooldownReason = ""
	runtime.HealthState = accountHealthRecentSuccess
	runtime.HealthCheckedAt = now
}

func (a *application) markAccountHealthFailure(expected accountConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return
	}
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	runtime.HealthState = accountHealthRecentFailure
	runtime.HealthCheckedAt = a.now()
}

func (a *application) recordAccountRequest(expected accountConfig, successful bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return
	}
	stats := a.requestStatsForLocked(expected.ID)
	if successful {
		stats.SuccessfulRequests++
	} else {
		stats.FailedRequests++
	}
	a.requestStats[expected.ID] = stats
	a.markRequestStatsDirtyLocked()
}

func (a *application) handleAccountUnauthorized(expected accountConfig, startedAt ...time.Time) string {
	return a.blockAccountFor(expected, "unauthorized", nil, startedAt...)
}

func (a *application) blockAccount(expected accountConfig, reason string) string {
	return a.blockAccountFor(expected, reason, nil)
}

func (a *application) blockAccountFor(expected accountConfig, reason string, retryAfter *time.Duration, startedAt ...time.Time) string {
	a.configMu.Lock()
	a.mu.Lock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		a.mu.Unlock()
		a.configMu.Unlock()
		return "stale"
	}
	if recoverableBlockedReason(reason) {
		current := a.cfg.Accounts[index]
		if hardBlockedReason(current.BlockedReason) {
			a.mu.Unlock()
			a.configMu.Unlock()
			return "blocked"
		}
		runtime := a.runtimeForLocked(current)
		now := a.now()
		if len(startedAt) != 0 && !startedAt[0].IsZero() && !runtime.FailureBarrierAt.IsZero() &&
			!startedAt[0].After(runtime.FailureBarrierAt) {
			a.mu.Unlock()
			a.configMu.Unlock()
			return "ignored"
		}
		if runtime.CooldownReason == reason && now.Before(runtime.CooldownUntil) && !runtime.ProbeInFlight {
			a.mu.Unlock()
			a.configMu.Unlock()
			return "cooldown"
		}
		configChanged := current.BlockedReason != reason
		candidate := a.cfg
		if configChanged {
			candidate = cloneConfig(a.cfg)
			candidate.Accounts[index].BlockedReason = reason
			candidate.LastSwitchReason = "account_" + reason
			candidate.LastSwitchAt = now.UTC().Format(time.RFC3339)
			a.cfg = candidate
		}
		cooldownHint := retryAfter
		if cooldownHint == nil && reason == "quota" {
			if resetAt := accountRecoveryResetAt(current, runtime, now); resetAt.After(now) {
				duration := resetAt.Sub(now)
				cooldownHint = &duration
			}
		}
		applyRecoveryCooldownLocked(runtime, reason, cooldownHint, now)
		if reason == "quota" && normalizeAccountAuthType(current.AuthType) == accountAuthCodexOAuth {
			runtime.CodexUsage.RateLimit.LimitReached = true
			runtime.CodexUsage.RateLimit.AllowedKnown = true
			runtime.CodexUsage.RateLimit.Allowed = false
			runtime.CodexUsage.CheckedAt = now
			runtime.CodexUsage.ExpiresAt = now.Add(codexUsageCacheTTL)
			runtime.CodexUsage.LimitResetAt = runtime.CooldownUntil
			if runtime.CodexUsage.PlanType == "" {
				runtime.CodexUsage.PlanType = current.CodexPlanType
			}
			if a.codexUsage != nil {
				a.codexUsage.MarkLimitReached(
					current.ID,
					[]byte(`{"error":{"code":"usage_limit_reached"}}`),
				)
			}
		}
		a.mu.Unlock()
		var err error
		if configChanged {
			err = a.writeConfig(candidate)
		}
		a.configMu.Unlock()
		if err != nil {
			a.logEvent(context.Background(), slog.LevelError, "account_state_persist_failed",
				"account_ref", logReference(expected.ID), "state", "blocked",
				"reason", reason, "error_kind", logErrorKind(err))
		}
		return "cooldown"
	}
	candidate := a.blockAccountLocked(index, reason)
	a.mu.Unlock()
	err := a.writeConfig(candidate)
	a.configMu.Unlock()
	if err != nil {
		a.logEvent(context.Background(), slog.LevelError, "account_state_persist_failed",
			"account_ref", logReference(expected.ID), "state", "blocked",
			"reason", reason, "error_kind", logErrorKind(err))
	}
	return "blocked"
}

func (a *application) blockAccountLocked(index int, reason string) storedConfig {
	now := a.now()
	candidate := cloneConfig(a.cfg)
	candidate.Accounts[index].BlockedReason = reason
	candidate.LastSwitchReason = "account_" + reason
	candidate.LastSwitchAt = now.UTC().Format(time.RFC3339)
	a.cfg = candidate
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	clearUpstreamFailureState(runtime)
	clearRecoveryState(runtime)
	runtime.FailureBarrierAt = now
	runtime.LastUnauthorizedAt = time.Time{}
	runtime.CooldownUntil = time.Time{}
	runtime.CooldownReason = ""
	runtime.ProbeInFlight = false
	return candidate
}

func (a *application) cooldownAccount(expected accountConfig) string {
	return a.cooldownAccountFor(expected, "temporary", accountCooldown)
}

func (a *application) cooldownAccountFor(expected accountConfig, reason string, duration time.Duration) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return "stale"
	}
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	now := a.now()
	clearUpstreamFailureState(runtime)
	runtime.FailureBarrierAt = now
	runtime.CooldownUntil = now.Add(duration)
	runtime.CooldownReason = reason
	return "cooldown"
}

func (a *application) cooldownAccountForRecovery(expected accountConfig, reason string, retryAfter *time.Duration, startedAt ...time.Time) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return "stale"
	}
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	now := a.now()
	if len(startedAt) != 0 && !startedAt[0].IsZero() && !runtime.FailureBarrierAt.IsZero() &&
		!startedAt[0].After(runtime.FailureBarrierAt) {
		return "ignored"
	}
	if runtime.CooldownReason == reason && now.Before(runtime.CooldownUntil) && !runtime.ProbeInFlight {
		return "cooldown"
	}
	applyRecoveryCooldownLocked(runtime, reason, retryAfter, now)
	return "cooldown"
}

func applyRecoveryCooldownLocked(runtime *accountRuntime, reason string, retryAfter *time.Duration, now time.Time) {
	if runtime.CooldownReason != reason || runtime.LastRecoveryAt.IsZero() ||
		now.Sub(runtime.LastRecoveryAt) > upstreamFailureReset {
		runtime.RecoveryFailures = 0
	}
	runtime.RecoveryFailures++
	duration := upstreamFailureCooldown(runtime.RecoveryFailures)
	if retryAfter != nil {
		duration = *retryAfter
	}
	if duration < 0 {
		duration = 0
	}
	clearUpstreamFailureState(runtime)
	runtime.FailureBarrierAt = now
	runtime.LastRecoveryAt = now
	runtime.CooldownReason = reason
	runtime.CooldownUntil = now.Add(duration)
}

func (a *application) clearUpstreamFailures(expected accountConfig, recordBarrier bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return
	}
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	clearUpstreamFailureState(runtime)
	if recordBarrier {
		runtime.FailureBarrierAt = a.now()
	}
}

func (a *application) clearUnauthorizedFailure(expected accountConfig) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return
	}
	a.runtimeForLocked(a.cfg.Accounts[index]).LastUnauthorizedAt = time.Time{}
}

func clearUpstreamFailureState(runtime *accountRuntime) {
	runtime.UpstreamFailures = 0
	runtime.LastFailureAt = time.Time{}
	if runtime.CooldownReason == "upstream_failures" {
		runtime.CooldownUntil = time.Time{}
		runtime.CooldownReason = ""
	}
}

func clearRecoveryState(runtime *accountRuntime) {
	runtime.RecoveryFailures = 0
	runtime.LastRecoveryAt = time.Time{}
}

func (a *application) cooldownAccountForFailure(expected accountConfig, startedAt ...time.Time) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 || !sameAccountRevision(a.cfg.Accounts[index], expected) {
		return "stale"
	}
	now := a.now()
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	if runtime.CooldownReason == "upstream_failures" && now.Before(runtime.CooldownUntil) && !runtime.ProbeInFlight {
		return upstreamFailureAction(runtime.UpstreamFailures)
	}
	if len(startedAt) != 0 && !startedAt[0].IsZero() && !runtime.FailureBarrierAt.IsZero() &&
		!startedAt[0].After(runtime.FailureBarrierAt) {
		return "ignored"
	}
	if runtime.LastFailureAt.IsZero() || (!runtime.CooldownUntil.IsZero() && now.Sub(runtime.CooldownUntil) > upstreamFailureReset) {
		runtime.UpstreamFailures = 0
	}
	runtime.UpstreamFailures++
	runtime.LastFailureAt = now
	runtime.FailureBarrierAt = now
	runtime.CooldownReason = "upstream_failures"
	runtime.CooldownUntil = now.Add(upstreamFailureCooldown(runtime.UpstreamFailures))
	return upstreamFailureAction(runtime.UpstreamFailures)
}

func upstreamFailureAction(failures int) string {
	if failures >= 3 {
		return "temporarily_disabled"
	}
	return "cooldown"
}

func upstreamFailureCooldown(failures int) time.Duration {
	switch failures {
	case 1:
		return 10 * time.Second
	case 2:
		return 30 * time.Second
	case 3:
		return 5 * time.Minute
	case 4:
		return 15 * time.Minute
	default:
		return 30 * time.Minute
	}
}
