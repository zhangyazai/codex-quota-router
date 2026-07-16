package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxErrorBody            = 1 << 20
	maxProxyBody            = 128 << 20
	maxRetryAfter           = 31 * 24 * time.Hour
	errorInspectionTime     = 250 * time.Millisecond
	upstreamHeaderTime      = time.Minute
	upstreamBodyIdleTime    = 2 * time.Minute
	proxyBodyReadTime       = 2 * time.Minute
	safeUpstreamAttempts    = 5
	safeUpstreamRetryBudget = 3 * time.Second
	codexGlobalConcurrency  = 4
	codexAccountConcurrency = 2
	codexGlobalRate         = 2.0
	codexGlobalBurst        = 4
	codexAccountRate        = 1.0
	codexAccountBurst       = 2
)

var (
	errEmptyUpstreamStream = errors.New("upstream stream closed before first payload")
	errUpstreamIdleTimeout = errors.New("upstream response idle timeout")
)

type codexTrafficLimitError struct {
	retryAfter time.Duration
}

func (err *codexTrafficLimitError) Error() string {
	return "Codex OAuth traffic limit reached"
}

type codexProviderGateError struct{}

func (err *codexProviderGateError) Error() string {
	return "Codex OAuth provider is cooling down"
}

type codexTrafficBucket struct {
	tokens      float64
	lastRefill  time.Time
	initialized bool
	inFlight    int
}

type codexTrafficGate struct {
	mu sync.Mutex

	global   codexTrafficBucket
	accounts map[string]*codexTrafficBucket

	providerUntil         time.Time
	providerLastRateLimit time.Time
	providerStrikes       int
	jitter                func(time.Duration) time.Duration
}

func (gate *codexTrafficGate) acquire(now time.Time, account accountConfig) (func(), error) {
	key := strings.TrimSpace(account.CodexAccountID)
	if key == "" {
		key = account.ID
	}

	gate.mu.Lock()
	if gate.providerUntil.After(now) {
		gate.mu.Unlock()
		return nil, &codexProviderGateError{}
	}
	if gate.accounts == nil {
		gate.accounts = make(map[string]*codexTrafficBucket)
	}
	accountBucket := gate.accounts[key]
	if accountBucket == nil {
		accountBucket = &codexTrafficBucket{}
		gate.accounts[key] = accountBucket
	}
	refillCodexTrafficBucket(&gate.global, now, codexGlobalRate, codexGlobalBurst)
	refillCodexTrafficBucket(accountBucket, now, codexAccountRate, codexAccountBurst)

	retryAfter := time.Duration(0)
	if gate.global.inFlight >= codexGlobalConcurrency || accountBucket.inFlight >= codexAccountConcurrency {
		retryAfter = time.Second
	}
	if delay := codexTokenRetryAfter(gate.global.tokens, codexGlobalRate); delay > retryAfter {
		retryAfter = delay
	}
	if delay := codexTokenRetryAfter(accountBucket.tokens, codexAccountRate); delay > retryAfter {
		retryAfter = delay
	}
	if retryAfter > 0 {
		gate.mu.Unlock()
		return nil, &codexTrafficLimitError{retryAfter: retryAfter}
	}

	gate.global.tokens--
	gate.global.inFlight++
	accountBucket.tokens--
	accountBucket.inFlight++
	gate.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() { gate.release(key) })
	}, nil
}

func (gate *codexTrafficGate) release(key string) {
	gate.mu.Lock()
	if gate.global.inFlight > 0 {
		gate.global.inFlight--
	}
	if accountBucket := gate.accounts[key]; accountBucket != nil && accountBucket.inFlight > 0 {
		accountBucket.inFlight--
	}
	gate.mu.Unlock()
}

func (gate *codexTrafficGate) providerAllows(account accountConfig, now time.Time) bool {
	if normalizeAccountAuthType(account.AuthType) != accountAuthCodexOAuth {
		return true
	}
	gate.mu.Lock()
	allowed := !gate.providerUntil.After(now)
	gate.mu.Unlock()
	return allowed
}

func (gate *codexTrafficGate) providerRecoveryAt(now time.Time) time.Time {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.providerUntil.After(now) {
		return gate.providerUntil
	}
	return time.Time{}
}

func (gate *codexTrafficGate) tripProviderRateLimit(now time.Time, retryAfter *time.Duration) time.Time {
	gate.mu.Lock()
	defer gate.mu.Unlock()

	if gate.providerLastRateLimit.IsZero() || now.Sub(gate.providerLastRateLimit) > upstreamFailureReset {
		gate.providerStrikes = 0
	}
	gate.providerStrikes++
	gate.providerLastRateLimit = now

	base := upstreamFailureCooldown(gate.providerStrikes)
	jitter := time.Duration(0)
	if gate.jitter != nil {
		jitter = gate.jitter(base)
	} else if limit := base / 4; limit > 0 {
		jitter = time.Duration(rand.Int63n(int64(limit) + 1))
	}
	if jitter < 0 {
		jitter = 0
	}
	if limit := base / 4; jitter > limit {
		jitter = limit
	}
	delay := base + jitter
	if retryAfter != nil && *retryAfter > delay {
		delay = *retryAfter
	}
	if delay > maxRetryAfter {
		delay = maxRetryAfter
	}
	until := now.Add(delay)
	if until.After(gate.providerUntil) {
		gate.providerUntil = until
	}
	return gate.providerUntil
}

func refillCodexTrafficBucket(bucket *codexTrafficBucket, now time.Time, rate float64, burst int) {
	if !bucket.initialized {
		bucket.tokens = float64(burst)
		bucket.lastRefill = now
		bucket.initialized = true
		return
	}
	if !now.After(bucket.lastRefill) {
		return
	}
	bucket.tokens += now.Sub(bucket.lastRefill).Seconds() * rate
	if bucket.tokens > float64(burst) {
		bucket.tokens = float64(burst)
	}
	bucket.lastRefill = now
}

func codexTokenRetryAfter(tokens, rate float64) time.Duration {
	if tokens >= 1 || rate <= 0 {
		return 0
	}
	delay := time.Duration((1 - tokens) / rate * float64(time.Second))
	if delay < time.Nanosecond {
		return time.Nanosecond
	}
	return delay
}

type codexTrafficBody struct {
	body    io.ReadCloser
	release func()
	once    sync.Once
	err     error
}

func (body *codexTrafficBody) Read(buffer []byte) (int, error) {
	return body.body.Read(buffer)
}

func (body *codexTrafficBody) Close() error {
	body.once.Do(func() {
		body.err = body.body.Close()
		body.release()
	})
	return body.err
}

type byteBudget struct {
	mu      sync.Mutex
	limit   int64
	used    int64
	changed chan struct{}
}

func newByteBudget(limit int64) *byteBudget {
	return &byteBudget{limit: limit, changed: make(chan struct{})}
}

func (budget *byteBudget) acquire(ctx context.Context, size int64) bool {
	if size <= 0 {
		return true
	}
	if size > budget.limit {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		budget.mu.Lock()
		if size <= budget.limit-budget.used {
			budget.used += size
			budget.mu.Unlock()
			return true
		}
		changed := budget.changed
		budget.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		}
	}
}

func (budget *byteBudget) release(size int64) {
	if size <= 0 {
		return
	}
	budget.mu.Lock()
	budget.used -= size
	close(budget.changed)
	budget.changed = make(chan struct{})
	budget.mu.Unlock()
}

func (budget *byteBudget) tryAcquire(size int64) bool {
	if size <= 0 {
		return true
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if size > budget.limit-budget.used {
		return false
	}
	budget.used += size
	return true
}

func (a *application) requestBodyBudget() *byteBudget {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.proxyBodyBudget == nil {
		a.proxyBodyBudget = newByteBudget(maxProxyBody)
	}
	return a.proxyBodyBudget
}

func (a *application) nextCodexProviderRecoveryAt(
	supports func(accountConfig) bool,
	now time.Time,
) time.Time {
	providerRecoveryAt := a.codexTraffic.providerRecoveryAt(now)
	if providerRecoveryAt.IsZero() {
		return time.Time{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var recoveryAt time.Time
	for _, account := range a.cfg.Accounts {
		if !account.Enabled || !accountConfigured(account) ||
			hardBlockedReason(account.BlockedReason) || normalizeAccountAuthType(account.AuthType) != accountAuthCodexOAuth ||
			(supports != nil && !supports(account)) {
			continue
		}
		runtime := a.runtimeForLocked(account)
		if runtime.ProbeInFlight {
			continue
		}
		candidateAt := providerRecoveryAt
		if probeAt := accountNextProbeAt(account, runtime); probeAt.After(candidateAt) {
			candidateAt = probeAt
		}
		if recoveryAt.IsZero() || candidateAt.Before(recoveryAt) {
			recoveryAt = candidateAt
		}
	}
	return recoveryAt
}

func (a *application) handleProxy(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	a.mu.Lock()
	a.requestSequence++
	requestID := a.requestSequence
	a.mu.Unlock()
	routeKind := proxyRouteKind(r.URL.Path)
	a.logEvent(r.Context(), slog.LevelInfo, "proxy_request_started",
		"request_id", requestID, "method", r.Method, "route", routeKind,
		"content_length", r.ContentLength, "query_present", r.URL.RawQuery != "")
	gatewayTokenID, ok := a.gatewayTokenID(r)
	if !ok {
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
	reservation := r.ContentLength
	if reservation < 0 {
		reservation = maxProxyBody
	}
	readDeadline := time.Now().Add(proxyBodyReadTime)
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(readDeadline)
	readContext, cancelRead := context.WithDeadline(r.Context(), readDeadline)
	bodyBudget := a.requestBodyBudget()
	if !bodyBudget.acquire(readContext, reservation) {
		cancelRead()
		_ = controller.SetReadDeadline(time.Time{})
		if r.Context().Err() == nil {
			writeProxyError(w, http.StatusServiceUnavailable, "proxy_busy", "代理请求等待资源超时")
		}
		return
	}
	var body []byte
	bodyReleased := false
	releaseBody := func() {
		if bodyReleased {
			return
		}
		body = nil
		bodyBudget.release(reservation)
		bodyReleased = true
	}
	defer releaseBody()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxProxyBody))
	cancelRead()
	_ = controller.SetReadDeadline(time.Time{})
	if unused := reservation - int64(len(body)); unused > 0 {
		bodyBudget.release(unused)
		reservation -= unused
	}
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
	contextReservation := int64(0)
	preparedBody, responsesRequest, prepareErr := a.prepareResponsesRequest(r, gatewayTokenID, body, func(size int64) bool {
		if !bodyBudget.tryAcquire(size) {
			return false
		}
		contextReservation += size
		reservation += size
		return true
	})
	if prepareErr != nil {
		if errors.Is(prepareErr, errResponsesAccountChanged) {
			writeProxyError(w, http.StatusConflict, "session_account_changed", "Responses 会话所属账号配置已变化，不能跨账号继续")
		} else if errors.Is(prepareErr, errResponsesContextTooLarge) {
			writeProxyError(w, http.StatusRequestEntityTooLarge, "response_context_too_large", "Responses 上下文超过可无感切换的大小限制")
		} else if errors.Is(prepareErr, errResponsesContextBusy) {
			writeProxyError(w, http.StatusServiceUnavailable, "proxy_busy", "Responses 上下文内存暂时不可用")
		} else {
			writeProxyError(w, http.StatusBadRequest, "invalid_response_context", "无法准备 Responses 上下文")
		}
		return
	}
	preparedReservation := int64(len(preparedBody)) + contextReservation
	if preparedReservation > maxProxyBody {
		writeProxyError(w, http.StatusRequestEntityTooLarge, "response_context_too_large", "Responses 上下文超过可无感切换的大小限制")
		return
	}
	if extra := preparedReservation - reservation; extra > 0 {
		budgetContext, cancelBudget := context.WithTimeout(r.Context(), proxyBodyReadTime)
		acquired := bodyBudget.acquire(budgetContext, extra)
		cancelBudget()
		if !acquired {
			if r.Context().Err() == nil {
				writeProxyError(w, http.StatusServiceUnavailable, "proxy_busy", "代理请求等待上下文资源超时")
			}
			return
		}
		reservation += extra
	} else if extra < 0 {
		bodyBudget.release(-extra)
		reservation += extra
	}
	body = preparedBody
	preferredAccountID := ""
	preferredAccountRevision := 0
	if responsesRequest != nil {
		preferredAccountID = responsesRequest.preferredAccountID
		preferredAccountRevision = responsesRequest.preferredAccountRevision
	}
	bodyIsReplay := false
	attempted := make(map[string]bool)
	var previous *http.Response
	upstreamUnavailable := false
	previousAccountRef := "none"
	previousAttempt := 0
	supports := func(account accountConfig) bool {
		return accountSupportsProxyRequest(account, r)
	}
	eligible := func(account accountConfig) bool {
		return supports(account) && a.codexTraffic.providerAllows(account, a.now())
	}
	for {
		account, ok, probe := a.selectProxyAccountFor(
			r.Context(), attempted, eligible, preferredAccountID, preferredAccountRevision,
		)
		if !ok {
			if r.Context().Err() != nil {
				if previous != nil {
					previous.Body.Close()
				}
				return
			}
			if previous != nil {
				defer previous.Body.Close()
				a.logEvent(r.Context(), slog.LevelWarn, "proxy_no_candidate",
					"request_id", requestID, "attempts", len(attempted), "last_status", previous.StatusCode)
				_ = a.forwardResponse(w, previous, requestID, previousAccountRef, previousAttempt, started)
				return
			}
			if upstreamUnavailable {
				writeProxyError(w, http.StatusBadGateway, "upstream_unavailable", "无法连接到上游账号")
				return
			}
			a.logEvent(r.Context(), slog.LevelWarn, "proxy_request_rejected",
				"request_id", requestID, "status", http.StatusServiceUnavailable, "reason", "no_available_account",
				"attempts", len(attempted), "duration_ms", time.Since(started).Milliseconds())
			now := a.now()
			recoveryAt := a.nextProxyRecoveryAt(attempted, eligible)
			providerRecoveryAt := a.nextCodexProviderRecoveryAt(supports, now)
			if recoveryAt.IsZero() || (!providerRecoveryAt.IsZero() && providerRecoveryAt.Before(recoveryAt)) {
				recoveryAt = providerRecoveryAt
			}
			writeProxyUnavailable(w, recoveryAt, now)
			return
		}
		if responsesRequest != nil && responsesRequest.knownPrevious &&
			!a.responsesAccountRevisionCurrent(responsesRequest.preferredAccountID, responsesRequest.preferredAccountRevision) {
			if probe {
				a.finishAccountProbe(account, false)
			}
			writeProxyError(w, http.StatusConflict, "session_account_changed", "Responses 会话所属账号配置已变化，不能跨账号继续")
			return
		}
		attempt := len(attempted) + 1
		accountRef := logReference(account.ID)
		attempted[account.ID] = true
		finishProbe := func() {
			if probe {
				a.finishAccountProbe(account, false)
				probe = false
			}
		}
		if previous != nil {
			previous.Body.Close()
			previous = nil
		}
		if !bodyIsReplay && responsesRequest.shouldReplay(account) {
			replayBody, replayErr := responsesRequest.replayBody()
			if replayErr != nil {
				finishProbe()
				if errors.Is(replayErr, errResponsesContextTooLarge) {
					writeProxyError(w, http.StatusRequestEntityTooLarge, "response_context_too_large", "Responses 上下文超过可无感切换的大小限制")
				} else {
					writeProxyError(w, http.StatusBadRequest, "invalid_response_context", "无法回放 Responses 上下文")
				}
				return
			}
			replayReservation := int64(len(replayBody)) + contextReservation
			if replayReservation > maxProxyBody {
				finishProbe()
				writeProxyError(w, http.StatusRequestEntityTooLarge, "response_context_too_large", "Responses 上下文超过可无感切换的大小限制")
				return
			}
			if extra := replayReservation - reservation; extra > 0 {
				budgetContext, cancelBudget := context.WithTimeout(r.Context(), proxyBodyReadTime)
				acquired := bodyBudget.acquire(budgetContext, extra)
				cancelBudget()
				if !acquired {
					finishProbe()
					if r.Context().Err() == nil {
						writeProxyError(w, http.StatusServiceUnavailable, "proxy_busy", "代理请求等待上下文资源超时")
					}
					return
				}
				reservation += extra
			} else if extra < 0 {
				bodyBudget.release(-extra)
				reservation += extra
			}
			body = replayBody
			bodyIsReplay = true
		}
		attemptStarted := time.Now()
		failureStartedAt := a.now()
		response, sendErr := a.sendUpstream(r.Context(), r, body, account)
		if sendErr != nil {
			var providerGate *codexProviderGateError
			if errors.As(sendErr, &providerGate) {
				finishProbe()
				continue
			}
			var trafficLimit *codexTrafficLimitError
			if errors.As(sendErr, &trafficLimit) {
				finishProbe()
				setCodexRetryAfter(w.Header(), trafficLimit.retryAfter)
				writeProxyError(w, http.StatusTooManyRequests, "codex_router_rate_limited", "Codex OAuth 请求过于频繁，请稍后重试")
				return
			}
			providerErrorCode := ""
			var providerError *codexProviderError
			if errors.As(sendErr, &providerError) {
				providerErrorCode = providerError.Code
			}
			replayable := safeToReplayUpstreamError(r.Method, sendErr)
			upstreamUnavailable = upstreamUnavailable || replayable
			if r.Context().Err() == nil {
				a.recordAccountRequest(account, false)
				a.markAccountHealthFailure(account)
			}
			a.logEvent(r.Context(), slog.LevelError, "proxy_upstream_failed",
				"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
				"status", http.StatusBadGateway, "request_bytes", len(body), "error_kind", logErrorKind(sendErr),
				"provider_error_code", providerErrorCode,
				"latency_ms", time.Since(attemptStarted).Milliseconds(), "duration_ms", time.Since(started).Milliseconds())
			if r.Context().Err() == nil {
				a.cooldownAccountForFailure(account, failureStartedAt)
				if replayable {
					finishProbe()
					continue
				}
			}
			finishProbe()
			if r.Context().Err() != nil {
				return
			}
			writeProxyError(w, http.StatusBadGateway, "upstream_result_unknown", "请求可能已被上游处理，但结果未返回")
			return
		}
		stream := strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
		var responsesStreamErr error
		if response.StatusCode >= 200 && response.StatusCode < 300 && stream && isResponsesRequest(r) {
			responsesStreamErr = inspectResponsesStreamPrelude(response, upstreamBodyIdleTime, a.responsesNow())
			stream = strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
		}
		responseLevel := slog.LevelInfo
		if response.StatusCode >= 400 {
			responseLevel = slog.LevelWarn
		}
		a.logEvent(r.Context(), responseLevel, "proxy_upstream_response",
			"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
			"status", response.StatusCode, "stream", stream, "request_bytes", len(body),
			"latency_ms", time.Since(attemptStarted).Milliseconds())
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if stream && r.Method != http.MethodHead && response.StatusCode != http.StatusNoContent &&
				response.StatusCode != http.StatusResetContent {
				streamErr := responsesStreamErr
				if streamErr == nil {
					streamErr = awaitStreamStart(response, upstreamBodyIdleTime)
				}
				if streamErr != nil {
					_ = response.Body.Close()
					if r.Context().Err() != nil {
						finishProbe()
						return
					}
					a.recordAccountRequest(account, false)
					a.markAccountHealthFailure(account)
					a.cooldownAccountForFailure(account, failureStartedAt)
					finishProbe()
					if r.Method == http.MethodGet || r.Method == http.MethodHead {
						upstreamUnavailable = true
						continue
					}
					writeProxyError(w, http.StatusBadGateway, "upstream_result_unknown", "请求可能已被上游处理，但结果未返回")
					return
				}
			}
			if probe && a.probeReleasesOnHeaders(account) {
				a.finishAccountProbe(account, true)
				probe = false
			}
			var responseCapture *responsesResponseCapture
			if isResponsesRequest(r) && bodyBudget.tryAcquire(responsesCaptureMaxBytes) {
				defer bodyBudget.release(responsesCaptureMaxBytes)
				responseCapture = newResponsesResponseCapture(response.Body)
				response.Body = responseCapture
			}
			contentType := response.Header.Get("Content-Type")
			defer response.Body.Close()
			forwardErr := a.forwardResponse(w, response, requestID, accountRef, attempt, started)
			if forwardErr == nil {
				responseResult := responsesResponseResult{}
				if captured, ok := responseCapture.bytes(); ok {
					responseResult = a.storeResponsesResponse(gatewayTokenID, responsesRequest, account, contentType, captured)
				}
				if responseResult.terminalFailure {
					a.applyResponsesTerminalFailure(account, responseResult, failureStartedAt)
					finishProbe()
					return
				}
				a.recordAccountRequest(account, true)
				a.markAccountVerified(account, failureStartedAt)
			} else if r.Context().Err() == nil {
				a.recordAccountRequest(account, false)
				a.markAccountHealthFailure(account)
				a.cooldownAccountForFailure(account, failureStartedAt)
			}
			finishProbe()
			return
		}
		classification, inspectErr := inspectErrorResponse(response)
		if inspectErr != nil {
			if r.Context().Err() == nil {
				a.markAccountHealthFailure(account)
			}
			a.logEvent(r.Context(), slog.LevelWarn, "proxy_response_inspection_failed",
				"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
				"error_kind", logErrorKind(inspectErr))
		}
		if classification == "" && (response.StatusCode == http.StatusRequestTimeout || response.StatusCode >= 500) {
			a.recordAccountRequest(account, false)
			a.markAccountHealthFailure(account)
			a.cooldownAccountForFailure(account, failureStartedAt)
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				previous = response
				previousAccountRef = accountRef
				previousAttempt = attempt
				finishProbe()
				continue
			}
			defer response.Body.Close()
			finishProbe()
			_ = a.forwardResponse(w, response, requestID, accountRef, attempt, started)
			return
		}
		a.recordAccountRequest(account, false)
		if classification != "request_incompatible" && (response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound ||
			response.StatusCode == http.StatusMethodNotAllowed || response.StatusCode == http.StatusGone) {
			a.markAccountHealthFailure(account)
		}
		retry := false
		if inspectErr == nil {
			switch classification {
			case "quota":
				a.markAccountHealthFailure(account)
				action := a.blockAccountFor(account, "quota", retryAfterHint(response.Header.Get("Retry-After"), a.now()), failureStartedAt)
				a.logEvent(r.Context(), slog.LevelWarn, "proxy_account_retry",
					"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
					"reason", "quota", "action", action)
				retry = true
			case "account_restricted":
				a.markAccountHealthFailure(account)
				a.blockAccount(account, "restricted")
				retry = true
			case "rate_limit":
				a.markAccountHealthFailure(account)
				if normalizeAccountAuthType(account.AuthType) == accountAuthCodexOAuth {
					now := a.now()
					providerUntil := a.codexTraffic.tripProviderRateLimit(
						now, retryAfterHint(response.Header.Get("Retry-After"), now),
					)
					if response.Header == nil {
						response.Header = make(http.Header)
					}
					setCodexRetryAfter(response.Header, providerUntil.Sub(now))
				}
				action := a.cooldownAccountForRecovery(account, "rate_limit", retryAfterHint(response.Header.Get("Retry-After"), a.now()), failureStartedAt)
				a.logEvent(r.Context(), slog.LevelWarn, "proxy_account_retry",
					"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
					"reason", "rate_limit", "action", action)
				retry = true
			case "request_incompatible":
				a.clearUpstreamFailures(account, true)
				a.clearUnauthorizedFailure(account)
				retry = true
			}
		}
		if !retry && response.StatusCode == http.StatusUnauthorized {
			a.markAccountHealthFailure(account)
			action := a.handleAccountUnauthorized(account, failureStartedAt)
			a.logEvent(r.Context(), slog.LevelWarn, "proxy_account_retry",
				"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
				"reason", "unauthorized", "action", action)
			retry = true
		}
		if !retry {
			a.clearUpstreamFailures(account, response.StatusCode != http.StatusUnauthorized)
			if response.StatusCode != http.StatusUnauthorized {
				a.clearUnauthorizedFailure(account)
			}
			defer response.Body.Close()
			finishProbe()
			if forwardErr := a.forwardResponse(w, response, requestID, accountRef, attempt, started); forwardErr != nil && r.Context().Err() == nil {
				a.markAccountHealthFailure(account)
				a.cooldownAccountForFailure(account, failureStartedAt)
			}
			return
		}
		previous = response
		previousAccountRef = accountRef
		previousAttempt = attempt
		finishProbe()
	}
}

func safeToReplayUpstreamError(method string, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var webSocketRequestSent *codexWebSocketRequestSentError
	if errors.As(err, &webSocketRequestSent) {
		return false
	}
	if method == http.MethodGet || method == http.MethodHead {
		return true
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return true
	}
	var operationError *net.OpError
	return errors.As(err, &operationError) && operationError.Op == "dial"
}

func (a *application) sendUpstream(ctx context.Context, original *http.Request, body []byte, account accountConfig) (*http.Response, error) {
	var release func()
	if normalizeAccountAuthType(account.AuthType) == accountAuthCodexOAuth {
		var err error
		release, err = a.codexTraffic.acquire(a.now(), account)
		if err != nil {
			return nil, err
		}
		defer func() {
			if release != nil {
				release()
			}
		}()
	}

	started := time.Now()
	for attempt := 1; ; attempt++ {
		response, err := a.sendUpstreamOnce(ctx, original, body, account)
		if err == nil || attempt >= safeUpstreamAttempts || !safeToReplayUpstreamError(original.Method, err) {
			if err == nil && response != nil && response.Body != nil && release != nil {
				response.Body = &codexTrafficBody{body: response.Body, release: release}
				release = nil
			}
			return response, err
		}
		delay := safeUpstreamRetryDelay(attempt)
		if time.Since(started)+delay >= safeUpstreamRetryBudget {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (a *application) sendUpstreamOnce(ctx context.Context, original *http.Request, body []byte, account accountConfig) (*http.Response, error) {
	if normalizeAccountAuthType(account.AuthType) == accountAuthCodexOAuth {
		updatedAccount, credentials, err := a.ensureCodexCredentials(ctx, account)
		if err != nil {
			var oauthError *codexOAuthUpstreamError
			if errors.Is(err, errCodexOAuthRefreshTokenMissing) ||
				(errors.As(err, &oauthError) && oauthError.StatusCode >= 400 && oauthError.StatusCode < 500) {
				return localProxyResponse(
					http.StatusUnauthorized,
					"codex_oauth_expired",
					"Codex OAuth 登录已失效，请在管理页面重新登录",
				), nil
			}
			return nil, err
		}
		response, err := sendCodexUpstream(
			ctx, a.client, original, body, updatedAccount.ID, updatedAccount.Revision,
			credentials.AccessToken, credentials.AccountID,
		)
		var providerError *codexProviderError
		if errors.As(err, &providerError) && providerError.HTTPStatus() < 500 {
			return localProxyResponse(
				providerError.HTTPStatus(),
				providerError.Code,
				"该 Codex OAuth 账号仅支持 Responses HTTP/SSE 请求",
			), nil
		}
		return response, err
	}
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
	if isResponsesRequest(original) {
		request.Header.Set("Accept-Encoding", "identity")
	}
	request.Header.Del("Authorization")
	request.Header.Del("X-API-Key")
	request.Header.Del("Cookie")
	request.Header.Del("Forwarded")
	request.Header.Del("X-Forwarded-For")
	request.Header.Del("X-Forwarded-Host")
	request.Header.Del("X-Forwarded-Proto")
	request.Header.Del("X-Real-IP")
	request.Header.Set("Authorization", "Bearer "+account.APIKey)
	return a.client.Do(request)
}

func localProxyResponse(status int, code, message string) *http.Response {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{"type": code, "code": code, "message": message},
	})
	return &http.Response{
		StatusCode: status,
		Status:     strconv.Itoa(status) + " " + http.StatusText(status),
		Header: http.Header{
			"Content-Type":   []string{"application/json; charset=utf-8"},
			"Content-Length": []string{strconv.Itoa(len(body))},
			"Cache-Control":  []string{"no-store"},
		},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func accountSupportsProxyRequest(account accountConfig, request *http.Request) bool {
	if normalizeAccountAuthType(account.AuthType) != accountAuthCodexOAuth {
		return true
	}
	if request == nil || request.URL == nil || request.Method != http.MethodPost || request.URL.RawQuery != "" {
		return false
	}
	if request.Header.Get("Upgrade") != "" ||
		strings.Contains(strings.ToLower(request.Header.Get("Connection")), "upgrade") {
		return false
	}
	return request.URL.Path == codexOAuthResponsesPath || request.URL.Path == codexOAuthCompactPath
}

func safeUpstreamRetryDelay(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 50 * time.Millisecond
	case 2:
		return 100 * time.Millisecond
	case 3:
		return 200 * time.Millisecond
	default:
		return 400 * time.Millisecond
	}
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

type readCloser struct {
	io.Reader
	io.Closer
}

func awaitStreamStart(response *http.Response, timeout time.Duration) error {
	if response == nil || response.Body == nil {
		return errEmptyUpstreamStream
	}
	originalBody := response.Body
	buffered := bufio.NewReader(originalBody)
	timerDone := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		_ = originalBody.Close()
		close(timerDone)
	})
	_, err := buffered.Peek(1)
	timedOut := !timer.Stop()
	if timedOut {
		<-timerDone
		return errUpstreamIdleTimeout
	}
	if errors.Is(err, io.EOF) {
		return errEmptyUpstreamStream
	}
	if err != nil {
		return err
	}
	response.Body = &readCloser{Reader: buffered, Closer: originalBody}
	return nil
}

func inspectErrorResponse(response *http.Response) (string, error) {
	return inspectErrorResponseWithTimeout(response, errorInspectionTime)
}

func inspectErrorResponseWithTimeout(response *http.Response, timeout time.Duration) (string, error) {
	if response.StatusCode == http.StatusPaymentRequired {
		_ = response.Body.Close()
		setBufferedResponseBody(response, nil)
		return "quota", nil
	}
	originalBody := response.Body
	timerDone := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		_ = originalBody.Close()
		close(timerDone)
	})
	prefix, err := io.ReadAll(io.LimitReader(originalBody, maxErrorBody+1))
	timedOut := !timer.Stop()
	if timedOut {
		<-timerDone
	}
	if timedOut {
		setBufferedResponseBody(response, prefix)
	} else {
		response.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(prefix), originalBody), Closer: originalBody}
	}
	if structuredQuotaError(prefix) {
		return "quota", nil
	}
	if structuredAccountRestrictionError(prefix) {
		return "account_restricted", nil
	}
	if structuredRequestCompatibilityError(prefix) {
		return "request_incompatible", nil
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return "rate_limit", nil
	}
	if err != nil && !timedOut {
		return "", err
	}
	return "", nil
}

func retryAfterDuration(value string, now time.Time) time.Duration {
	if duration := retryAfterHint(value, now); duration != nil {
		return *duration
	}
	return accountCooldown
}

func retryAfterHint(value string, now time.Time) *time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= int64(maxRetryAfter/time.Second) {
		duration := maxRetryAfter
		return &duration
	} else if err == nil && seconds > 0 {
		duration := time.Duration(seconds) * time.Second
		return &duration
	} else if retryAt, err := http.ParseTime(value); err == nil && retryAt.After(now) {
		duration := retryAt.Sub(now)
		if duration > maxRetryAfter {
			duration = maxRetryAfter
		}
		return &duration
	}
	return nil
}

func setBufferedResponseBody(response *http.Response, body []byte) {
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	response.Header.Set("Content-Length", strconv.FormatInt(response.ContentLength, 10))
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
		"usage_limit_reached": true,
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

func structuredAccountRestrictionError(body []byte) bool {
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
	restrictedCodes := map[string]bool{
		"account_deactivated": true, "account_suspended": true, "account_disabled": true,
		"account_restricted": true, "user_deactivated": true, "user_suspended": true,
		"organization_deactivated": true, "organization_suspended": true, "access_terminated": true,
	}
	for _, identifier := range identifiers {
		normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(identifier)))
		if restrictedCodes[normalized] {
			return true
		}
	}
	restrictedPhrases := []string{
		"account has been deactivated", "account has been suspended", "account is disabled",
		"账号已停用", "账号已封禁", "账号被限制", "账户已停用", "账户已封禁", "账户被限制",
	}
	for _, message := range messages {
		message = strings.ToLower(message)
		for _, phrase := range restrictedPhrases {
			if strings.Contains(message, phrase) {
				return true
			}
		}
	}
	return false
}

func structuredRequestCompatibilityError(body []byte) bool {
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
	compatibilityCodes := map[string]bool{
		"model_access_denied": true, "model_not_found": true, "model_not_supported": true,
		"model_not_available": true, "unsupported_model": true, "invalid_model": true,
		"endpoint_not_supported": true, "unsupported_endpoint": true,
	}
	for _, identifier := range identifiers {
		normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(identifier)))
		if compatibilityCodes[normalized] {
			return true
		}
	}
	for _, message := range messages {
		message = strings.ToLower(message)
		if strings.Contains(message, "model") &&
			(strings.Contains(message, "is only supported on") || strings.Contains(message, "not supported on") ||
				strings.Contains(message, "not supported for this endpoint") || strings.Contains(message, "not supported by this endpoint") ||
				strings.Contains(message, "does not exist or you do not have access") || strings.Contains(message, "model not found")) {
			return true
		}
		if strings.Contains(message, "模型") &&
			(strings.Contains(message, "仅支持") || strings.Contains(message, "不支持当前接口") ||
				strings.Contains(message, "模型不存在") || strings.Contains(message, "无权访问该模型")) {
			return true
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
) error {
	written, terminal, errorKind, err := forwardResponseWithIdleTimeoutResult(w, response, upstreamBodyIdleTime)
	level := slog.LevelInfo
	if terminal != "eof" {
		level = slog.LevelWarn
	}
	a.logEvent(context.Background(), level, "proxy_response_finished",
		"request_id", requestID, "attempt", attempt, "account_ref", accountRef,
		"status", response.StatusCode, "stream", strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream"),
		"bytes", written, "terminal", terminal, "error_kind", errorKind,
		"duration_ms", time.Since(started).Milliseconds())
	return err
}

func forwardResponseWithIdleTimeout(w http.ResponseWriter, response *http.Response, timeout time.Duration) error {
	_, _, _, err := forwardResponseWithIdleTimeoutResult(w, response, timeout)
	return err
}

func forwardResponseWithIdleTimeoutResult(
	w http.ResponseWriter,
	response *http.Response,
	timeout time.Duration,
) (written int64, terminal string, errorKind string, err error) {
	return forwardResponseWithTimeoutsResult(w, response, timeout, timeout)
}

func forwardResponseWithTimeoutsResult(
	w http.ResponseWriter,
	response *http.Response,
	readIdleTimeout time.Duration,
	writeIdleTimeout time.Duration,
) (written int64, terminal string, errorKind string, err error) {
	terminal = "eof"
	errorKind = "none"
	controller := http.NewResponseController(w)
	writeDeadlineSupported := writeIdleTimeout > 0
	setWriteDeadline := func(deadline time.Time) error {
		if !writeDeadlineSupported {
			return nil
		}
		deadlineErr := controller.SetWriteDeadline(deadline)
		if errors.Is(deadlineErr, http.ErrNotSupported) {
			writeDeadlineSupported = false
			return nil
		}
		return deadlineErr
	}
	if writeIdleTimeout > 0 {
		defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
		if deadlineErr := setWriteDeadline(time.Now().Add(writeIdleTimeout)); deadlineErr != nil {
			terminal = "downstream_write_error"
			errorKind = logErrorKind(deadlineErr)
			return
		}
	}
	copyHeaders(w.Header(), response.Header)
	removeHopHeaders(w.Header())
	w.WriteHeader(response.StatusCode)
	buffer := make([]byte, 32<<10)
	for {
		timerDone := make(chan struct{})
		timer := time.AfterFunc(readIdleTimeout, func() {
			_ = response.Body.Close()
			close(timerDone)
		})
		count, readErr := response.Body.Read(buffer)
		timedOut := !timer.Stop()
		if timedOut {
			<-timerDone
		}
		if count > 0 {
			if deadlineErr := setWriteDeadline(time.Now().Add(writeIdleTimeout)); deadlineErr != nil {
				terminal = "downstream_write_error"
				errorKind = logErrorKind(deadlineErr)
				return
			}
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
		if timedOut {
			terminal = "upstream_idle_timeout"
			errorKind = "timeout"
			err = errUpstreamIdleTimeout
			return
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				terminal = "upstream_read_error"
				errorKind = logErrorKind(readErr)
				err = readErr
				return
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
	return
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

func writeProxyUnavailable(w http.ResponseWriter, retryAt, now time.Time) {
	if !retryAt.After(now) {
		writeProxyError(w, http.StatusServiceUnavailable, "upstream_not_available", "没有可路由账号")
		return
	}
	delay := retryAt.Sub(now)
	if delay > maxRetryAfter {
		delay = maxRetryAfter
		retryAt = now.Add(delay)
	}
	seconds := int64(delay / time.Second)
	if delay%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error": map[string]any{
			"message":           "没有可路由账号，仍有账号处于受控冷却",
			"type":              "gateway_error",
			"code":              "upstream_not_available",
			"retryAfterSeconds": seconds,
			"retryAt":           retryAt.UTC().Format(time.RFC3339),
		},
	})
}
