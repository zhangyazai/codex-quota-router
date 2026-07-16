package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

const (
	codexUsageEndpoint         = "https://chatgpt.com/backend-api/wham/usage"
	codexUsageCacheTTL         = time.Minute
	codexUsageRequestTimeout   = 10 * time.Second
	codexUsageMaxResponseBytes = int64(1 << 20)

	quotaResetUnitHour  = "hour"
	quotaResetUnitDay   = "day"
	quotaResetUnitWeek  = "week"
	quotaResetUnitMonth = "month"
)

var (
	errCodexUsageCacheUnavailable = errors.New("codex usage cache is unavailable")
	errCodexUsageCredentials      = errors.New("codex usage credentials are required")
	errCodexUsageResponseTooLarge = errors.New("codex usage response is too large")
)

type codexUsageHTTPError struct {
	StatusCode int
}

func (err *codexUsageHTTPError) Error() string {
	return fmt.Sprintf("codex usage request returned HTTP %d", err.StatusCode)
}

type codexUsageWindow struct {
	Present           bool      `json:"present"`
	UsedPercent       float64   `json:"usedPercent"`
	WindowSeconds     int64     `json:"windowSeconds,omitempty"`
	ResetAfterSeconds int64     `json:"resetAfterSeconds,omitempty"`
	ResetAt           time.Time `json:"resetAt,omitempty"`
}

type codexUsageCredits struct {
	Present         bool    `json:"present"`
	HasCredits      bool    `json:"hasCredits"`
	HasCreditsKnown bool    `json:"hasCreditsKnown"`
	Unlimited       bool    `json:"unlimited"`
	UnlimitedKnown  bool    `json:"unlimitedKnown"`
	Balance         float64 `json:"balance"`
	BalanceKnown    bool    `json:"balanceKnown"`
}

type codexUsageRateLimit struct {
	Allowed         bool             `json:"allowed"`
	AllowedKnown    bool             `json:"allowedKnown"`
	LimitReached    bool             `json:"limitReached"`
	PrimaryWindow   codexUsageWindow `json:"primaryWindow"`
	SecondaryWindow codexUsageWindow `json:"secondaryWindow"`
}

type codexUsageSnapshot struct {
	PlanType     string              `json:"planType,omitempty"`
	RateLimit    codexUsageRateLimit `json:"rateLimit"`
	Credits      codexUsageCredits   `json:"credits"`
	CheckedAt    time.Time           `json:"checkedAt"`
	ExpiresAt    time.Time           `json:"expiresAt"`
	LimitResetAt time.Time           `json:"limitResetAt,omitempty"`
}

func (snapshot codexUsageSnapshot) Fresh(now time.Time) bool {
	return !snapshot.ExpiresAt.IsZero() && now.Before(snapshot.ExpiresAt)
}

func (snapshot codexUsageSnapshot) Exhausted() bool {
	return snapshot.RateLimit.LimitReached ||
		(snapshot.RateLimit.AllowedKnown && !snapshot.RateLimit.Allowed)
}

func (snapshot codexUsageSnapshot) PlanWindowExhausted() bool {
	primary := snapshot.RateLimit.PrimaryWindow
	secondary := snapshot.RateLimit.SecondaryWindow
	return (primary.Present && primary.UsedPercent >= 100) ||
		(secondary.Present && secondary.UsedPercent >= 100)
}

func (snapshot codexUsageSnapshot) BlocksRouting(disableCredits bool) bool {
	return snapshot.Exhausted() || (disableCredits && snapshot.PlanWindowExhausted())
}

func (snapshot codexUsageSnapshot) NextResetAt(now time.Time) time.Time {
	return earliestFutureTime(
		now,
		snapshot.RateLimit.PrimaryWindow.effectiveResetAt(snapshot.CheckedAt, now),
		snapshot.RateLimit.SecondaryWindow.effectiveResetAt(snapshot.CheckedAt, now),
	)
}

func (snapshot codexUsageSnapshot) CooldownUntil(now time.Time) time.Time {
	if snapshot.LimitResetAt.After(now) {
		return snapshot.LimitResetAt
	}

	var exhaustedResets []time.Time
	primary := snapshot.RateLimit.PrimaryWindow
	if primary.Present && primary.UsedPercent >= 100 {
		exhaustedResets = append(exhaustedResets, primary.effectiveResetAt(snapshot.CheckedAt, now))
	}
	secondary := snapshot.RateLimit.SecondaryWindow
	if secondary.Present && secondary.UsedPercent >= 100 {
		exhaustedResets = append(exhaustedResets, secondary.effectiveResetAt(snapshot.CheckedAt, now))
	}
	if resetAt := latestFutureTime(now, exhaustedResets...); !resetAt.IsZero() {
		return resetAt
	}
	if snapshot.Exhausted() {
		return snapshot.NextResetAt(now)
	}
	return time.Time{}
}

// EffectiveResetAt is the comparable reset time used by quota-reset routing.
// An exhausted account returns the time at which it can be retried; otherwise
// it returns the nearest quota window reset so expiring quota can be used first.
func (snapshot codexUsageSnapshot) EffectiveResetAt(now time.Time) time.Time {
	return snapshot.EffectiveResetAtFor(false, now)
}

func (snapshot codexUsageSnapshot) EffectiveResetAtFor(disableCredits bool, now time.Time) time.Time {
	if snapshot.BlocksRouting(disableCredits) {
		return snapshot.CooldownUntil(now)
	}
	return snapshot.NextResetAt(now)
}

func (window codexUsageWindow) effectiveResetAt(checkedAt, now time.Time) time.Time {
	if window.ResetAt.After(now) {
		return window.ResetAt
	}
	if window.ResetAfterSeconds <= 0 || checkedAt.IsZero() {
		return time.Time{}
	}
	duration, ok := codexSecondsDuration(window.ResetAfterSeconds)
	if !ok {
		return time.Time{}
	}
	resetAt := checkedAt.Add(duration)
	if resetAt.After(now) {
		return resetAt
	}
	return time.Time{}
}

type codexUsageCacheEntry struct {
	snapshot       codexUsageSnapshot
	credentialHash [sha256.Size]byte
}

type codexUsageRefresh struct {
	done           chan struct{}
	credentialHash [sha256.Size]byte
	snapshot       codexUsageSnapshot
	err            error
}

type codexUsageCache struct {
	mu       sync.Mutex
	client   *http.Client
	now      func() time.Time
	entries  map[string]codexUsageCacheEntry
	inflight map[string]*codexUsageRefresh
	versions map[string]uint64
}

func newCodexUsageCache(baseClient *http.Client, now func() time.Time) *codexUsageCache {
	transport := http.DefaultTransport
	if baseClient != nil && baseClient.Transport != nil {
		transport = baseClient.Transport
	}
	if now == nil {
		now = time.Now
	}
	return &codexUsageCache{
		client: &http.Client{
			Transport: transport,
			Timeout:   codexUsageRequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now:      now,
		entries:  make(map[string]codexUsageCacheEntry),
		inflight: make(map[string]*codexUsageRefresh),
		versions: make(map[string]uint64),
	}
}

// Cached is network-free and is the only method routing code should call.
func (cache *codexUsageCache) Cached(cacheKey string) (codexUsageSnapshot, bool) {
	if cache == nil {
		return codexUsageSnapshot{}, false
	}
	cacheKey = strings.TrimSpace(cacheKey)
	if cacheKey == "" {
		return codexUsageSnapshot{}, false
	}
	cache.mu.Lock()
	entry, ok := cache.entries[cacheKey]
	cache.mu.Unlock()
	return entry.snapshot, ok
}

// Refresh returns a fresh cached value when possible and coalesces concurrent
// refreshes for the same account. Call it from status/background refresh paths,
// not from every proxied request.
func (cache *codexUsageCache) Refresh(
	ctx context.Context,
	cacheKey, accessToken, chatGPTAccountID string,
) (codexUsageSnapshot, error) {
	if cache == nil || cache.client == nil {
		return codexUsageSnapshot{}, errCodexUsageCacheUnavailable
	}
	cacheKey = strings.TrimSpace(cacheKey)
	accessToken = trimBearerPrefix(accessToken)
	chatGPTAccountID = strings.TrimSpace(chatGPTAccountID)
	if cacheKey == "" || accessToken == "" || chatGPTAccountID == "" {
		return codexUsageSnapshot{}, errCodexUsageCredentials
	}
	if ctx == nil {
		ctx = context.Background()
	}
	credentialHash := codexUsageCredentialHash(accessToken, chatGPTAccountID)

	for {
		now := cache.currentTime()
		cache.mu.Lock()
		entry, hasEntry := cache.entries[cacheKey]
		if hasEntry && entry.credentialHash == credentialHash && entry.snapshot.Fresh(now) {
			cache.mu.Unlock()
			return entry.snapshot, nil
		}
		if running := cache.inflight[cacheKey]; running != nil {
			done := running.done
			sameCredentials := running.credentialHash == credentialHash
			cache.mu.Unlock()
			select {
			case <-ctx.Done():
				return codexUsageSnapshot{}, ctx.Err()
			case <-done:
				if sameCredentials {
					return running.snapshot, running.err
				}
				continue
			}
		}

		version := cache.versions[cacheKey]
		refresh := &codexUsageRefresh{
			done:           make(chan struct{}),
			credentialHash: credentialHash,
		}
		cache.inflight[cacheKey] = refresh
		stale := entry.snapshot
		hasMatchingStale := hasEntry && entry.credentialHash == credentialHash
		cache.mu.Unlock()

		snapshot, err := cache.fetch(ctx, accessToken, chatGPTAccountID)
		if err != nil && hasMatchingStale {
			snapshot = stale
		}

		cache.mu.Lock()
		if cache.versions[cacheKey] != version {
			if current, ok := cache.entries[cacheKey]; ok {
				snapshot = current.snapshot
				err = nil
			}
		} else if err == nil {
			cache.entries[cacheKey] = codexUsageCacheEntry{
				snapshot:       snapshot,
				credentialHash: credentialHash,
			}
		}
		refresh.snapshot = snapshot
		refresh.err = err
		delete(cache.inflight, cacheKey)
		close(refresh.done)
		cache.mu.Unlock()
		return snapshot, err
	}
}

func (cache *codexUsageCache) Invalidate(cacheKey string) {
	if cache == nil {
		return
	}
	cacheKey = strings.TrimSpace(cacheKey)
	if cacheKey == "" {
		return
	}
	cache.mu.Lock()
	delete(cache.entries, cacheKey)
	cache.versions[cacheKey]++
	cache.mu.Unlock()
}

// MarkLimitReached updates cached routing state from a Codex
// usage_limit_reached error without making another network request.
func (cache *codexUsageCache) MarkLimitReached(cacheKey string, body []byte) (codexUsageSnapshot, bool) {
	if cache == nil {
		return codexUsageSnapshot{}, false
	}
	cacheKey = strings.TrimSpace(cacheKey)
	if cacheKey == "" {
		return codexUsageSnapshot{}, false
	}
	now := cache.currentTime()
	resetAt, matched := parseCodexUsageLimitReached(body, now)
	if !matched {
		return codexUsageSnapshot{}, false
	}

	cache.mu.Lock()
	entry := cache.entries[cacheKey]
	snapshot := entry.snapshot
	snapshot.RateLimit.Allowed = false
	snapshot.RateLimit.AllowedKnown = true
	snapshot.RateLimit.LimitReached = true
	if resetAt.After(now) {
		snapshot.LimitResetAt = resetAt
	}
	if snapshot.CheckedAt.IsZero() {
		snapshot.CheckedAt = now
	}
	snapshot.ExpiresAt = now.Add(codexUsageCacheTTL)
	entry.snapshot = snapshot
	cache.entries[cacheKey] = entry
	cache.versions[cacheKey]++
	cache.mu.Unlock()
	return snapshot, true
}

func (cache *codexUsageCache) fetch(
	ctx context.Context,
	accessToken, chatGPTAccountID string,
) (codexUsageSnapshot, error) {
	requestContext, cancel := context.WithTimeout(ctx, codexUsageRequestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, codexUsageEndpoint, nil)
	if err != nil {
		return codexUsageSnapshot{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("ChatGPT-Account-ID", chatGPTAccountID)

	response, err := cache.client.Do(request)
	if err != nil {
		return codexUsageSnapshot{}, err
	}
	defer response.Body.Close()
	if response.ContentLength > codexUsageMaxResponseBytes {
		return codexUsageSnapshot{}, errCodexUsageResponseTooLarge
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return codexUsageSnapshot{}, &codexUsageHTTPError{StatusCode: response.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, codexUsageMaxResponseBytes+1))
	if err != nil {
		return codexUsageSnapshot{}, err
	}
	if int64(len(body)) > codexUsageMaxResponseBytes {
		return codexUsageSnapshot{}, errCodexUsageResponseTooLarge
	}
	return parseCodexUsageSnapshot(body, cache.currentTime())
}

func (cache *codexUsageCache) currentTime() time.Time {
	if cache == nil || cache.now == nil {
		return time.Now().UTC()
	}
	return cache.now().UTC()
}

func parseCodexUsageSnapshot(body []byte, checkedAt time.Time) (codexUsageSnapshot, error) {
	if len(body) == 0 || int64(len(body)) > codexUsageMaxResponseBytes || !json.Valid(body) {
		return codexUsageSnapshot{}, errors.New("invalid codex usage response")
	}
	payload, ok := decodeJSONObject(body)
	if !ok {
		return codexUsageSnapshot{}, errors.New("invalid codex usage response")
	}
	rateLimit, hasRateLimit := payload["rate_limit"].(map[string]any)
	_, hasCredits := payload["credits"]
	planType := stringValue(payload["plan_type"])
	if planType == "" {
		planType = stringValue(payload["planType"])
	}
	if planType == "" && !hasRateLimit && !hasCredits {
		return codexUsageSnapshot{}, errors.New("codex usage response has no usage fields")
	}

	checkedAt = checkedAt.UTC()
	snapshot := codexUsageSnapshot{
		PlanType:  planType,
		CheckedAt: checkedAt,
		ExpiresAt: checkedAt.Add(codexUsageCacheTTL),
	}
	if hasRateLimit {
		if allowed, known := boolValue(rateLimit["allowed"]); known {
			snapshot.RateLimit.Allowed = allowed
			snapshot.RateLimit.AllowedKnown = true
		}
		if reached, known := boolValue(rateLimit["limit_reached"]); known {
			snapshot.RateLimit.LimitReached = reached
		}
		snapshot.RateLimit.PrimaryWindow = parseCodexUsageWindow(rateLimit["primary_window"])
		snapshot.RateLimit.SecondaryWindow = parseCodexUsageWindow(rateLimit["secondary_window"])
		snapshot.LimitResetAt = resetAtFromObject(rateLimit, checkedAt)
	}
	snapshot.Credits = parseCodexUsageCredits(payload["credits"])
	if snapshot.RateLimit.LimitReached && !snapshot.RateLimit.AllowedKnown {
		snapshot.RateLimit.AllowedKnown = true
		snapshot.RateLimit.Allowed = false
	}
	return snapshot, nil
}

func parseCodexUsageWindow(value any) codexUsageWindow {
	payload, ok := value.(map[string]any)
	if !ok {
		return codexUsageWindow{}
	}
	window := codexUsageWindow{Present: true}
	if used, ok := firstNumber(payload, "used_percent", "usedPercent"); ok && used >= 0 {
		window.UsedPercent = used
	}
	window.WindowSeconds, _ = durationSecondsFromObject(
		payload,
		"window",
		"window_seconds",
		"limit_window_seconds",
	)
	window.ResetAfterSeconds, _ = durationSecondsFromObject(
		payload,
		"reset_after",
		"reset_after_seconds",
		"resets_in_seconds",
	)
	window.ResetAt, _ = timeFromObject(payload, "reset_at", "resets_at")
	return window
}

func parseCodexUsageCredits(value any) codexUsageCredits {
	credits := codexUsageCredits{}
	switch payload := value.(type) {
	case map[string]any:
		credits.Present = true
		if hasCredits, known := boolValue(payload["has_credits"]); known {
			credits.HasCredits = hasCredits
			credits.HasCreditsKnown = true
		}
		if unlimited, known := boolValue(payload["unlimited"]); known {
			credits.Unlimited = unlimited
			credits.UnlimitedKnown = true
		}
		if balance, known := firstNumber(payload, "balance", "remaining", "amount", "credits"); known {
			credits.Balance = balance
			credits.BalanceKnown = true
		}
	default:
		if balance, known := numberValue(value); known {
			credits.Present = true
			credits.Balance = balance
			credits.BalanceKnown = true
		} else if hasCredits, known := boolValue(value); known {
			credits.Present = true
			credits.HasCredits = hasCredits
			credits.HasCreditsKnown = true
		}
	}
	return credits
}

func parseCodexUsageLimitReached(body []byte, now time.Time) (time.Time, bool) {
	if len(body) == 0 || int64(len(body)) > codexUsageMaxResponseBytes || !json.Valid(body) {
		return time.Time{}, false
	}
	payload, ok := decodeJSONObject(body)
	if !ok {
		return time.Time{}, false
	}
	objects := codexUsageErrorObjects(payload)
	matched := false
	for _, object := range objects {
		if strings.EqualFold(stringValue(object["type"]), "usage_limit_reached") {
			matched = true
			break
		}
		if strings.EqualFold(stringValue(object["code"]), "usage_limit_reached") {
			matched = true
			break
		}
	}
	if !matched {
		return time.Time{}, false
	}
	now = now.UTC()
	for _, object := range objects {
		if resetAt := resetAtFromObject(object, now); resetAt.After(now) {
			return resetAt, true
		}
	}
	return time.Time{}, true
}

func codexUsageErrorObjects(payload map[string]any) []map[string]any {
	objects := []map[string]any{payload}
	appendNested := func(parent map[string]any, key string) map[string]any {
		if nested, ok := parent[key].(map[string]any); ok {
			objects = append(objects, nested)
			return nested
		}
		return nil
	}
	appendNested(payload, "error")
	if response := appendNested(payload, "response"); response != nil {
		appendNested(response, "error")
	}
	if body := appendNested(payload, "body"); body != nil {
		appendNested(body, "error")
	}
	return objects
}

func resetAtFromObject(payload map[string]any, observedAt time.Time) time.Time {
	if resetAt, ok := timeFromObject(payload, "reset_at", "resets_at"); ok && resetAt.After(observedAt) {
		return resetAt
	}
	seconds, ok := durationSecondsFromObject(
		payload,
		"reset_after",
		"reset_after_seconds",
		"resets_in_seconds",
	)
	if !ok {
		return time.Time{}
	}
	duration, ok := codexSecondsDuration(seconds)
	if !ok {
		return time.Time{}
	}
	return observedAt.Add(duration)
}

func durationSecondsFromObject(payload map[string]any, keys ...string) (int64, bool) {
	const maxDurationSeconds = float64((1<<63 - 1) / int64(time.Second))
	for _, key := range keys {
		value, exists := payload[key]
		if !exists {
			continue
		}
		if number, ok := numberValue(value); ok && number > 0 && number <= maxDurationSeconds {
			return int64(math.Ceil(number)), true
		}
		text, ok := value.(string)
		if !ok {
			continue
		}
		duration, err := time.ParseDuration(strings.TrimSpace(text))
		if err == nil && duration > 0 {
			return int64(math.Ceil(duration.Seconds())), true
		}
	}
	return 0, false
}

func timeFromObject(payload map[string]any, keys ...string) (time.Time, bool) {
	for _, key := range keys {
		if parsed, ok := codexUsageTime(payload[key]); ok {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func codexUsageTime(value any) (time.Time, bool) {
	if number, ok := numberValue(value); ok && number > 0 {
		seconds := number
		switch {
		case number >= 1e18:
			seconds = number / 1e9
		case number >= 1e15:
			seconds = number / 1e6
		case number >= 1e12:
			seconds = number / 1e3
		}
		whole, fraction := math.Modf(seconds)
		if whole <= float64(1<<63-1) {
			return time.Unix(int64(whole), int64(fraction*float64(time.Second))).UTC(), true
		}
	}
	text, ok := value.(string)
	if !ok {
		return time.Time{}, false
	}
	text = strings.TrimSpace(text)
	if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return parsed.UTC(), true
	}
	if parsed, err := http.ParseTime(text); err == nil {
		return parsed.UTC(), true
	}
	return time.Time{}, false
}

func codexSecondsDuration(seconds int64) (time.Duration, bool) {
	if seconds <= 0 || seconds > (1<<63-1)/int64(time.Second) {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func codexUsageCredentialHash(accessToken, accountID string) [sha256.Size]byte {
	return sha256.Sum256([]byte(accountID + "\x00" + accessToken))
}

func trimBearerPrefix(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= len("Bearer ") && strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		return strings.TrimSpace(value[len("Bearer "):])
	}
	return value
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func earliestFutureTime(now time.Time, values ...time.Time) time.Time {
	var selected time.Time
	for _, value := range values {
		if !value.After(now) || (!selected.IsZero() && !value.Before(selected)) {
			continue
		}
		selected = value
	}
	return selected
}

func latestFutureTime(now time.Time, values ...time.Time) time.Time {
	var selected time.Time
	for _, value := range values {
		if !value.After(now) || (!selected.IsZero() && !value.After(selected)) {
			continue
		}
		selected = value
	}
	return selected
}

func quotaResetInterval(every int, unit string) (time.Duration, error) {
	if every <= 0 {
		return 0, errors.New("quota reset interval must be positive")
	}
	var base time.Duration
	switch strings.ToLower(strings.TrimSpace(unit)) {
	case quotaResetUnitHour:
		base = time.Hour
	case quotaResetUnitDay:
		base = 24 * time.Hour
	case quotaResetUnitWeek:
		base = 7 * 24 * time.Hour
	default:
		return 0, errors.New("quota reset unit must be hour, day, or week")
	}
	const maxDuration = time.Duration(1<<63 - 1)
	if time.Duration(every) > maxDuration/base {
		return 0, errors.New("quota reset interval is too large")
	}
	return time.Duration(every) * base, nil
}

func parseQuotaResetAnchorAt(anchorAt string) (time.Time, error) {
	anchorAt = strings.TrimSpace(anchorAt)
	if anchorAt == "" {
		return time.Time{}, errors.New("quota reset anchor is required")
	}
	anchor, err := time.Parse(time.RFC3339Nano, anchorAt)
	if err != nil {
		return time.Time{}, errors.New("quota reset anchor must use RFC3339")
	}
	return anchor, nil
}

func nextQuotaResetAt(every int, unit, anchorAt string, now time.Time) (time.Time, error) {
	return nextQuotaResetAtInTimezone(every, unit, anchorAt, "", now)
}

func nextQuotaResetAtInTimezone(every int, unit, anchorAt, timezone string, now time.Time) (time.Time, error) {
	anchor, err := parseQuotaResetAnchorAt(anchorAt)
	if err != nil {
		return time.Time{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	if strings.EqualFold(strings.TrimSpace(unit), quotaResetUnitMonth) {
		location, err := quotaResetLocation(timezone)
		if err != nil {
			return time.Time{}, err
		}
		return nextMonthlyQuotaResetAt(every, anchor.In(location), now.In(location))
	}
	interval, err := quotaResetInterval(every, unit)
	if err != nil {
		return time.Time{}, err
	}
	if now.Before(anchor) {
		return anchor, nil
	}
	steps := now.Sub(anchor)/interval + 1
	const maxDuration = time.Duration(1<<63 - 1)
	if steps > maxDuration/interval {
		return time.Time{}, errors.New("quota reset time is too far in the future")
	}
	return anchor.Add(steps * interval), nil
}

func nextMonthlyQuotaResetAt(every int, anchor, now time.Time) (time.Time, error) {
	if every <= 0 {
		return time.Time{}, errors.New("quota reset interval must be positive")
	}
	if now.Before(anchor) {
		return anchor, nil
	}
	months := (now.Year()-anchor.Year())*12 + int(now.Month()-anchor.Month())
	steps := months / every
	resetAt := addQuotaResetMonths(anchor, steps*every)
	if !resetAt.After(now) {
		resetAt = addQuotaResetMonths(anchor, (steps+1)*every)
	}
	return resetAt, nil
}

func addQuotaResetMonths(anchor time.Time, months int) time.Time {
	first := time.Date(anchor.Year(), anchor.Month()+time.Month(months), 1,
		anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), anchor.Location())
	day := anchor.Day()
	if lastDay := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, first.Location()).Day(); day > lastDay {
		day = lastDay
	}
	return time.Date(first.Year(), first.Month(), day,
		anchor.Hour(), anchor.Minute(), anchor.Second(), anchor.Nanosecond(), anchor.Location())
}

func quotaResetLocation(timezone string) (*time.Location, error) {
	timezone = strings.TrimSpace(timezone)
	if timezone == "" {
		timezone = defaultQuotaResetTimezone
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, errors.New("quota reset timezone is invalid")
	}
	return location, nil
}

func nextCalendarQuotaResetAt(period, timezone string, now time.Time) (time.Time, error) {
	location, err := quotaResetLocation(timezone)
	if err != nil {
		return time.Time{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	now = now.In(location)
	resetAt := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	switch period {
	case quotaResetPeriodDaily:
		if !resetAt.After(now) {
			resetAt = resetAt.AddDate(0, 0, 1)
		}
	case quotaResetPeriodWeekly:
		resetAt = resetAt.AddDate(0, 0, -((int(now.Weekday())+6)%7))
		if !resetAt.After(now) {
			resetAt = resetAt.AddDate(0, 0, 7)
		}
	case quotaResetPeriodMonthly:
		resetAt = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, location)
		if !resetAt.After(now) {
			resetAt = resetAt.AddDate(0, 1, 0)
		}
	default:
		return time.Time{}, errors.New("quota reset period is invalid")
	}
	return resetAt, nil
}

func quotaResetEffectiveAt(period, timezone string, every int, unit, anchorAt string, now time.Time) (time.Time, bool) {
	period = strings.TrimSpace(strings.ToLower(period))
	if period == "" {
		if every > 0 {
			period = quotaResetPeriodCustom
		} else {
			period = quotaResetPeriodNever
		}
	}
	var resetAt time.Time
	var err error
	switch period {
	case quotaResetPeriodNever:
		return time.Time{}, false
	case quotaResetPeriodDaily, quotaResetPeriodWeekly, quotaResetPeriodMonthly:
		resetAt, err = nextCalendarQuotaResetAt(period, timezone, now)
	case quotaResetPeriodCustom:
		resetAt, err = nextQuotaResetAtInTimezone(every, unit, anchorAt, timezone, now)
	default:
		return time.Time{}, false
	}
	return resetAt, err == nil
}
