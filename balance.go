package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	balanceTTL              = 5 * time.Minute
	balanceAutoTTL          = time.Minute
	balanceRefreshTime      = 20 * time.Second
	balanceRoutingTime      = 1500 * time.Millisecond
	balanceWorkers          = 8
	balanceProbeParallelism = 3
	newAPIUnlimitedLimit    = 100000000

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
	balanceStageAccountSubscription   = "account_subscription"
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

var errNewAPIAuthentication = errors.New("New API authentication failed")

type balanceSnapshot struct {
	Status        string
	Amount        float64
	Unit          string
	DisplayLabel  string
	Unlimited     bool
	Scope         string
	LimitedBy     string
	Subscription  *subscriptionQuota
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
	Subscription  *subscriptionQuota `json:"subscription,omitempty"`
	UpdatedAt     string  `json:"updatedAt,omitempty"`
	RefreshStatus string  `json:"refreshStatus,omitempty"`
	CheckedAt     string  `json:"checkedAt,omitempty"`
	ErrorStage    string  `json:"errorStage,omitempty"`
	ErrorCode     string  `json:"errorCode,omitempty"`
	Retryable     bool    `json:"retryable,omitempty"`
	Fresh         bool    `json:"fresh"`
}

type subscriptionQuota struct {
	Total             float64 `json:"total"`
	Remaining         float64 `json:"remaining"`
	Unit              string  `json:"unit,omitempty"`
	DisplayLabel      string  `json:"displayLabel,omitempty"`
	Unlimited         bool    `json:"unlimited"`
	BillingPreference string  `json:"billingPreference,omitempty"`
	PriorityResetAt   string  `json:"priorityResetAt,omitempty"`
	ResetAt           string  `json:"resetAt,omitempty"`
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
	quota        float64
	unlimited    bool
	subscription newAPISubscriptionBalance
	failure      *balanceFailure
}

type newAPISubscriptionBalance struct {
	quota               float64
	total               float64
	unlimited           bool
	hasSubscriptions    bool
	allowWalletOverflow bool
	preference          string
	priorityResetAt     time.Time
	resetAt             time.Time
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
		Unlimited: balance.Unlimited, Scope: scope, LimitedBy: balance.LimitedBy, Subscription: balance.Subscription,
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
	if balance.Subscription != nil {
		for _, value := range []string{balance.Subscription.ResetAt, balance.Subscription.PriorityResetAt} {
			if resetAt, ok := parseSubscriptionResetAt(value); ok && !resetAt.After(now) {
				return true
			}
		}
	}
	return !balanceFreshFor(balance, now, maxAge)
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
	account := a.cfg.Accounts[index]
	runtime := a.runtimeForLocked(account)
	if !balance.CheckedAt.IsZero() && !runtime.Balance.CheckedAt.IsZero() &&
		balance.CheckedAt.Before(runtime.Balance.CheckedAt) {
		return runtime.Balance, false
	}
	runtime.Balance = mergeBalanceAttempt(runtime.Balance, balance)
	now := a.now()
	if account.BlockedReason == "quota" && runtime.CooldownReason == "quota" &&
		!runtime.CooldownUntil.IsZero() && !now.After(runtime.CooldownUntil) && balanceConfirmsRecovery(balance) {
		runtime.CooldownUntil = now
		a.signalProbeChangedLocked()
	}
	return runtime.Balance, true
}

func balanceConfirmsRecovery(balance balanceSnapshot) bool {
	if balance.Status != "ok" || (balance.RefreshStatus != "" &&
		balance.RefreshStatus != balanceRefreshOK && balance.RefreshStatus != balanceRefreshPartial) {
		return false
	}
	scope := balance.Scope
	if scope == "" {
		scope = balanceScopeActual
	}
	if scope != balanceScopeActual && scope != balanceScopeAccountOnly {
		return false
	}
	return balance.Unlimited || balance.Amount > 0
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
		if normalizeAccountAuthType(account.AuthType) == accountAuthCodexOAuth {
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
	accountStage := stage == balanceStageAccountLogin || stage == balanceStageAccountQuota ||
		stage == balanceStageAccountSubscription
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
	quota, unlimited, subscription, err := a.probeNewAPIAccountBalance(ctx, account)
	if err != nil {
		failure := failureFromError(balanceStageAccountQuota, err)
		return accountQuotaResult{failure: &failure}
	}
	if quota < 0 {
		quota = 0
	}
	return accountQuotaResult{quota: quota, unlimited: unlimited, subscription: subscription}
}

func applyAccountBalanceLimit(balance *balanceSnapshot, accountAmount float64, accountUnlimited bool) {
	balance.Scope = balanceScopeActual
	switch {
	case balance.Unlimited && accountUnlimited:
		balance.LimitedBy = ""
	case balance.Unlimited:
		balance.Amount = accountAmount
		balance.Unlimited = false
		balance.LimitedBy = "account"
	case accountUnlimited:
		balance.LimitedBy = "token"
	case accountAmount < balance.Amount:
		balance.Amount = accountAmount
		balance.LimitedBy = "account"
	default:
		balance.LimitedBy = "token"
	}
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
	switch effectiveAccountProvider(account) {
	case accountProviderSub2API, accountProviderCodexOAuth:
		return balanceAttemptFromFailure(checkedAt, balanceFailure{
			Status: balanceRefreshUnsupported, Stage: balanceStageTokenUsage, Code: balanceErrorUnsupported,
		})
	case accountProviderOpenAIResponses:
		return a.probeDashboardBalance(ctx, account, checkedAt)
	}
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
					Unlimited: accountQuota.unlimited, Scope: balanceScopeAccountOnly,
					Subscription: convertedNewAPISubscriptionQuota(accountQuota.subscription, metadata.data),
					UpdatedAt: checkedAt, CheckedAt: checkedAt,
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
		applyAccountBalanceLimit(&result, accountAmount, accountQuota.unlimited)
	} else if accountChannel != nil {
		applyAccountBalanceLimit(&result, accountQuota.quota, accountQuota.unlimited)
	}

	if metadata.failure != nil {
		if token.fromDashboard && mode == newAPIAuthAPIKey && metadata.statusCode == http.StatusNotFound {
			if result.hasHardLimit && result.hardLimit >= newAPIUnlimitedLimit {
				result.Unlimited = true
			}
			result.Scope = balanceScopeTokenOnly
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
	if accountChannel != nil {
		result.Subscription = convertedNewAPISubscriptionQuota(accountQuota.subscription, metadata.data)
	}

	if mode == newAPIAuthAPIKey && !token.fromDashboard && result.Unlimited {
		dashboard := a.probeDashboardBalance(ctx, account, checkedAt)
		if dashboard.Status == "ok" && dashboard.hasHardLimit && dashboard.hardLimit < newAPIUnlimitedLimit {
			amount, unit, label, converted := convertUSDToDisplay(dashboard.Amount, metadata.data)
			if converted && result.Unit == unit {
				dashboard.Amount = amount
				dashboard.Unit = unit
				dashboard.DisplayLabel = label
				dashboard.Scope = balanceScopeTokenOnly
				dashboard.LimitedBy = ""
				return dashboard
			}
		}
	}
	result.Status = "ok"
	result.UpdatedAt = checkedAt
	result.RefreshStatus = balanceRefreshOK
	return result
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

func convertedNewAPISubscriptionQuota(subscription newAPISubscriptionBalance, statusData map[string]any) *subscriptionQuota {
	if !subscription.hasSubscriptions {
		return nil
	}
	total, unit, label, totalOK := convertNewAPIQuota(subscription.total, statusData)
	remaining, remainingUnit, _, remainingOK := convertNewAPIQuota(subscription.quota, statusData)
	if !totalOK || !remainingOK || unit != remainingUnit {
		return nil
	}
	resetAt := ""
	if !subscription.resetAt.IsZero() {
		resetAt = subscription.resetAt.UTC().Format(time.RFC3339)
	}
	priorityResetAt := ""
	if !subscription.priorityResetAt.IsZero() {
		priorityResetAt = subscription.priorityResetAt.UTC().Format(time.RFC3339)
	}
	return &subscriptionQuota{
		Total: total, Remaining: remaining, Unit: unit, DisplayLabel: label,
		Unlimited: subscription.unlimited, BillingPreference: subscription.preference,
		PriorityResetAt: priorityResetAt, ResetAt: resetAt,
	}
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

func (a *application) probeNewAPIAccountBalance(ctx context.Context, account accountConfig) (float64, bool, newAPISubscriptionBalance, error) {
	switch normalizeNewAPIAuthMode(account.NewAPIAuthMode) {
	case newAPIAuthAccessToken:
		return a.getNewAPIAccountBalance(ctx, account, "", account.NewAPIUserID)
	case newAPIAuthPassword:
		if cookie, userID := a.cachedNewAPISession(account); cookie != "" && userID > 0 {
			quota, unlimited, subscription, err := a.getNewAPIAccountBalance(ctx, account, cookie, userID)
			if err == nil {
				return quota, unlimited, subscription, nil
			}
			if !errors.Is(err, errNewAPIAuthentication) {
				return 0, false, newAPISubscriptionBalance{}, err
			}
			a.clearNewAPISession(account)
		}
		cookie, userID, err := a.loginNewAPI(ctx, account)
		if err != nil {
			return 0, false, newAPISubscriptionBalance{}, err
		}
		a.cacheNewAPISession(account, cookie, userID)
		return a.getNewAPIAccountBalance(ctx, account, cookie, userID)
	default:
		return 0, false, newAPISubscriptionBalance{}, errors.New("New API account authentication is not configured")
	}
}

func (a *application) getNewAPIAccountBalance(ctx context.Context, account accountConfig, cookie string, userID int) (float64, bool, newAPISubscriptionBalance, error) {
	walletQuota, err := a.getNewAPIUserQuota(ctx, account, cookie, userID)
	if err != nil {
		return 0, false, newAPISubscriptionBalance{}, err
	}
	if walletQuota < 0 {
		walletQuota = 0
	}
	subscription, err := a.getNewAPISubscriptionBalance(ctx, account, cookie, userID)
	if err != nil {
		var probeErr *balanceProbeError
		if errors.As(err, &probeErr) && probeErr.failure.Status == balanceRefreshUnsupported {
			return walletQuota, false, newAPISubscriptionBalance{}, nil
		}
		return 0, false, newAPISubscriptionBalance{}, err
	}
	quota, unlimited, err := availableNewAPIAccountQuota(walletQuota, subscription)
	return quota, unlimited, subscription, err
}

func availableNewAPIAccountQuota(walletQuota float64, subscription newAPISubscriptionBalance) (float64, bool, error) {
	combine := func() (float64, bool, error) {
		if subscription.unlimited {
			return 0, true, nil
		}
		total := walletQuota + subscription.quota
		if math.IsNaN(total) || math.IsInf(total, 0) {
			return 0, false, errors.New("New API account balance is invalid")
		}
		return total, false, nil
	}

	switch subscription.preference {
	case "wallet_only":
		return walletQuota, false, nil
	case "subscription_only":
		return subscription.quota, subscription.unlimited, nil
	case "wallet_first":
		return combine()
	default:
		if !subscription.hasSubscriptions {
			return walletQuota, false, nil
		}
		if subscription.allowWalletOverflow {
			return combine()
		}
		return subscription.quota, subscription.unlimited, nil
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
	headers := newAPIUserHeaders(account, cookie, userID)
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

func (a *application) getNewAPISubscriptionBalance(ctx context.Context, account accountConfig, cookie string, userID int) (newAPISubscriptionBalance, error) {
	target, err := balanceAPIURL(account.BaseURL, "/api/subscription/self")
	if err != nil {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountSubscription, Code: balanceErrorInvalidResponse}
		return newAPISubscriptionBalance{}, &balanceProbeError{failure: failure, cause: err}
	}
	status, body, _, err := a.requestUpstreamJSON(ctx, http.MethodGet, target, nil, newAPIUserHeaders(account, cookie, userID))
	if err != nil {
		failure := balanceFailureFor(balanceStageAccountSubscription, status, err)
		return newAPISubscriptionBalance{}, &balanceProbeError{failure: failure, cause: err}
	}
	if status < 200 || status >= 300 {
		failure := balanceFailureFor(balanceStageAccountSubscription, status, nil)
		var cause error
		if failure.Status == balanceRefreshAuthError {
			if cookie == "" {
				failure.Code = accessTokenAuthErrorCode(body)
			}
			cause = errNewAPIAuthentication
		}
		return newAPISubscriptionBalance{}, &balanceProbeError{failure: failure, cause: cause}
	}
	payload, ok := decodeJSONObject(body)
	if !ok {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountSubscription, Code: balanceErrorInvalidResponse, Retryable: true}
		return newAPISubscriptionBalance{}, &balanceProbeError{failure: failure}
	}
	if success, exists := boolValue(payload["success"]); exists && !success {
		code := balanceErrorAccountAuth
		if cookie == "" {
			code = accessTokenAuthErrorCode(body)
		}
		failure := balanceFailure{Status: balanceRefreshAuthError, Stage: balanceStageAccountSubscription, Code: code}
		return newAPISubscriptionBalance{}, &balanceProbeError{failure: failure, cause: errNewAPIAuthentication}
	}

	data := nestedObject(payload, "data")
	preference, _ := data["billing_preference"].(string)
	items, ok := data["subscriptions"].([]any)
	if !ok {
		failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountSubscription, Code: balanceErrorInvalidResponse, Retryable: true}
		return newAPISubscriptionBalance{}, &balanceProbeError{failure: failure}
	}
	result := newAPISubscriptionBalance{
		hasSubscriptions: len(items) > 0, allowWalletOverflow: true,
		preference: strings.ToLower(strings.TrimSpace(preference)),
	}
	for _, item := range items {
		summary, ok := item.(map[string]any)
		if !ok {
			failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountSubscription, Code: balanceErrorInvalidResponse, Retryable: true}
			return newAPISubscriptionBalance{}, &balanceProbeError{failure: failure}
		}
		subscription := nestedObject(summary, "subscription")
		amountTotal, hasTotal := numberValue(subscription["amount_total"])
		amountUsed, hasUsed := numberValue(subscription["amount_used"])
		if !hasTotal || !hasUsed {
			failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountSubscription, Code: balanceErrorInvalidResponse, Retryable: true}
			return newAPISubscriptionBalance{}, &balanceProbeError{failure: failure}
		}
		if amountTotal > 0 {
			total := result.total + amountTotal
			if math.IsNaN(total) || math.IsInf(total, 0) {
				failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountSubscription, Code: balanceErrorInvalidResponse, Retryable: true}
				return newAPISubscriptionBalance{}, &balanceProbeError{failure: failure}
			}
			result.total = total
			remaining := math.Max(amountTotal-amountUsed, 0)
			if remaining > 0 {
				quota := result.quota + remaining
				if math.IsNaN(quota) || math.IsInf(quota, 0) {
					failure := balanceFailure{Status: balanceRefreshError, Stage: balanceStageAccountSubscription, Code: balanceErrorInvalidResponse, Retryable: true}
					return newAPISubscriptionBalance{}, &balanceProbeError{failure: failure}
				}
				result.quota = quota
			}
			if nextReset, ok := numberValue(subscription["next_reset_time"]); ok && nextReset > 0 && nextReset <= float64(1<<63-1) {
				resetAt := time.Unix(int64(nextReset), 0).UTC()
				if result.resetAt.IsZero() || resetAt.Before(result.resetAt) {
					result.resetAt = resetAt
				}
				if remaining > 0 && (result.priorityResetAt.IsZero() || resetAt.Before(result.priorityResetAt)) {
					result.priorityResetAt = resetAt
				}
			}
		} else {
			result.unlimited = true
		}
		allowOverflow, _ := boolValue(subscription["allow_wallet_overflow"])
		if !allowOverflow {
			result.allowWalletOverflow = false
		}
	}
	return result, nil
}

func newAPIUserHeaders(account accountConfig, cookie string, userID int) http.Header {
	headers := make(http.Header)
	headers.Set("New-Api-User", strconv.Itoa(userID))
	if cookie != "" {
		headers.Set("Cookie", cookie)
	} else {
		headers.Set("Authorization", "Bearer "+account.NewAPISecret)
	}
	return headers
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
