package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	responsesStateTTL             = 24 * time.Hour
	responsesStateMaxEntries      = 2048
	responsesStateMaxBytes        = 128 << 20
	responsesTranscriptMaxBytes   = 32 << 20
	responsesCaptureMaxBytes      = 32 << 20
	responsesStreamPreludeMaxSize = 1 << 20
	responsesIDMaxLength          = 512
)

var (
	errResponsesContextTooLarge = errors.New("responses context is too large to replay")
	errResponsesContextBusy     = errors.New("responses context memory is busy")
	errResponsesAccountChanged  = errors.New("responses account configuration changed")
)

type responsesStateKey struct {
	gatewayTokenID string
	responseID     string
}

type responsesStateEntry struct {
	accountID         string
	accountRevision   int
	upstreamReusable  bool
	input             []json.RawMessage
	size              int64
	expiresAt         time.Time
	sequence          uint64
}

type responsesStateCache struct {
	mu       sync.Mutex
	entries  map[responsesStateKey]responsesStateEntry
	bytes    int64
	sequence uint64
}

type responsesRequestState struct {
	input                    []json.RawMessage
	preferredAccountID       string
	preferredAccountRevision int
	previousReusable         bool
	knownPrevious            bool
	forceReplay              bool
	replayPayload            map[string]any
	storeEnabled             bool
	cacheable                bool
}

type responsesResponseCapture struct {
	body     io.ReadCloser
	buffer   bytes.Buffer
	overflow bool
}

type responsesResponseResult struct {
	terminalFailure bool
	classification string
	retryAfter      *time.Duration
}

func isResponsesRequest(request *http.Request) bool {
	return request != nil && request.URL != nil && request.Method == http.MethodPost &&
		request.URL.Path == codexOAuthResponsesPath
}

func (a *application) getResponsesStateCache() *responsesStateCache {
	a.mu.Lock()
	if a.responsesState == nil {
		a.responsesState = &responsesStateCache{entries: make(map[responsesStateKey]responsesStateEntry)}
	}
	cache := a.responsesState
	a.mu.Unlock()
	return cache
}

func (a *application) responsesNow() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

func (a *application) responsesAccountRevisionCurrent(accountID string, revision int) bool {
	account, found := a.savedAccount(accountID)
	return found && account.Revision == revision
}

func (cache *responsesStateCache) lookup(gatewayTokenID, responseID string, now time.Time) (responsesStateEntry, bool) {
	if cache == nil || gatewayTokenID == "" || !validResponsesID(responseID) {
		return responsesStateEntry{}, false
	}
	key := responsesStateKey{gatewayTokenID: gatewayTokenID, responseID: responseID}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.pruneExpiredLocked(now)
	entry, ok := cache.entries[key]
	if !ok {
		return responsesStateEntry{}, false
	}
	cache.sequence++
	entry.sequence = cache.sequence
	entry.expiresAt = now.Add(responsesStateTTL)
	cache.entries[key] = entry
	return entry, true
}

func (cache *responsesStateCache) store(
	gatewayTokenID string,
	responseID string,
	accountID string,
	accountRevision int,
	upstreamReusable bool,
	input []json.RawMessage,
	now time.Time,
) bool {
	if cache == nil || gatewayTokenID == "" || accountID == "" || !validResponsesID(responseID) {
		return false
	}
	size := rawMessagesSize(input)
	if size > responsesTranscriptMaxBytes {
		return false
	}
	key := responsesStateKey{gatewayTokenID: gatewayTokenID, responseID: responseID}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.pruneExpiredLocked(now)
	if previous, ok := cache.entries[key]; ok &&
		(previous.accountID != accountID || previous.accountRevision != accountRevision ||
			previous.upstreamReusable != upstreamReusable ||
			!rawMessagesEqual(previous.input, input)) {
		cache.bytes -= previous.size
		delete(cache.entries, key)
		return false
	}
	cache.sequence++
	entry := responsesStateEntry{
		accountID:        accountID,
		accountRevision:  accountRevision,
		upstreamReusable: upstreamReusable,
		input:            cloneRawMessages(input),
		size:             size,
		expiresAt:        now.Add(responsesStateTTL),
		sequence:         cache.sequence,
	}
	if previous, ok := cache.entries[key]; ok {
		cache.bytes -= previous.size
	}
	cache.entries[key] = entry
	cache.bytes += entry.size
	for len(cache.entries) > responsesStateMaxEntries || cache.bytes > responsesStateMaxBytes {
		cache.evictOldestLocked()
	}
	_, ok := cache.entries[key]
	return ok
}

func (cache *responsesStateCache) pruneExpiredLocked(now time.Time) {
	for key, entry := range cache.entries {
		if !now.Before(entry.expiresAt) {
			delete(cache.entries, key)
			cache.bytes -= entry.size
		}
	}
}

func (cache *responsesStateCache) evictOldestLocked() {
	var oldestKey responsesStateKey
	var oldest uint64
	found := false
	for key, entry := range cache.entries {
		if !found || entry.sequence < oldest {
			oldestKey = key
			oldest = entry.sequence
			found = true
		}
	}
	if !found {
		return
	}
	cache.bytes -= cache.entries[oldestKey].size
	delete(cache.entries, oldestKey)
}

func (a *application) prepareResponsesRequest(
	request *http.Request,
	gatewayTokenID string,
	body []byte,
	reserve func(int64) bool,
) ([]byte, *responsesRequestState, error) {
	if !isResponsesRequest(request) || len(body) == 0 {
		return body, nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil || payload == nil {
		return body, nil, nil
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return body, nil, nil
	}
	currentInput, ok := normalizeResponsesInput(payload["input"])
	if !ok {
		return body, nil, nil
	}
	storeEnabled := true
	if rawStore, exists := payload["store"]; exists {
		if store, ok := rawStore.(bool); ok {
			storeEnabled = store
		}
	}
	state := &responsesRequestState{input: currentInput, storeEnabled: storeEnabled, cacheable: true}
	previousResponseID, _ := payload["previous_response_id"].(string)
	if previousResponseID != "" {
		if !validResponsesID(previousResponseID) {
			return body, nil, nil
		}
		entry, found := a.getResponsesStateCache().lookup(gatewayTokenID, previousResponseID, a.responsesNow())
		if !found {
			return body, nil, nil
		}
		if !a.responsesAccountRevisionCurrent(entry.accountID, entry.accountRevision) {
			return nil, nil, errResponsesAccountChanged
		}
		state.knownPrevious = true
		state.preferredAccountID = entry.accountID
		state.preferredAccountRevision = entry.accountRevision
		state.previousReusable = entry.upstreamReusable
		state.forceReplay = rawMessagesHavePrefix(currentInput, entry.input)
		if !state.forceReplay {
			if reserve != nil && !reserve(entry.size) {
				return nil, nil, errResponsesContextBusy
			}
			state.input = append(cloneRawMessages(entry.input), currentInput...)
		}
		state.replayPayload = payload
	}
	if !state.knownPrevious && rawMessagesSize(state.input) > responsesTranscriptMaxBytes {
		return body, nil, nil
	}
	payload["input"] = currentInput
	ensureResponsesReasoningInclude(payload)
	prepared, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, err
	}
	if len(prepared) > maxProxyBody {
		return nil, nil, errResponsesContextTooLarge
	}
	return prepared, state, nil
}

func (state *responsesRequestState) shouldReplay(account accountConfig) bool {
	if state == nil || !state.knownPrevious {
		return false
	}
	if state.forceReplay || normalizeAccountAuthType(account.AuthType) == accountAuthCodexOAuth {
		return true
	}
	return account.ID != state.preferredAccountID || account.Revision != state.preferredAccountRevision ||
		!state.previousReusable
}

func (state *responsesRequestState) replayBody() ([]byte, error) {
	if state == nil || !state.knownPrevious || state.replayPayload == nil {
		return nil, errors.New("responses replay state is unavailable")
	}
	if rawMessagesSize(state.input) > responsesTranscriptMaxBytes {
		return nil, errResponsesContextTooLarge
	}
	state.replayPayload["input"] = state.input
	delete(state.replayPayload, "previous_response_id")
	prepared, err := json.Marshal(state.replayPayload)
	if err != nil {
		return nil, err
	}
	if len(prepared) > codexOAuthRequestLimit {
		return nil, errResponsesContextTooLarge
	}
	return prepared, nil
}

func normalizeResponsesInput(value any) ([]json.RawMessage, bool) {
	if text, ok := value.(string); ok {
		value = []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": text,
			}},
		}}
	}
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	result := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, false
		}
		result = append(result, encoded)
	}
	return result, true
}

func ensureResponsesReasoningInclude(payload map[string]any) {
	const encryptedReasoning = "reasoning.encrypted_content"
	include, exists := payload["include"]
	if !exists {
		payload["include"] = []any{encryptedReasoning}
		return
	}
	items, ok := include.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		if text, ok := item.(string); ok && text == encryptedReasoning {
			return
		}
	}
	payload["include"] = append(items, encryptedReasoning)
}

func rawMessagesHavePrefix(items, prefix []json.RawMessage) bool {
	if len(items) < len(prefix) {
		return false
	}
	for index := range prefix {
		if !bytes.Equal(bytes.TrimSpace(items[index]), bytes.TrimSpace(prefix[index])) {
			return false
		}
	}
	return true
}

func rawMessagesEqual(left, right []json.RawMessage) bool {
	return len(left) == len(right) && rawMessagesHavePrefix(left, right)
}

func cloneRawMessages(items []json.RawMessage) []json.RawMessage {
	cloned := make([]json.RawMessage, len(items))
	for index := range items {
		cloned[index] = bytes.Clone(items[index])
	}
	return cloned
}

func rawMessagesSize(items []json.RawMessage) int64 {
	var size int64
	for _, item := range items {
		size += int64(len(item) + 1)
	}
	return size
}

func validResponsesID(responseID string) bool {
	return responseID != "" && len(responseID) <= responsesIDMaxLength && responseID == strings.TrimSpace(responseID)
}

func newResponsesResponseCapture(body io.ReadCloser) *responsesResponseCapture {
	return &responsesResponseCapture{body: body}
}

func (capture *responsesResponseCapture) Read(buffer []byte) (int, error) {
	count, err := capture.body.Read(buffer)
	if count > 0 && !capture.overflow {
		if capture.buffer.Len()+count > responsesCaptureMaxBytes {
			capture.buffer = bytes.Buffer{}
			capture.overflow = true
		} else {
			_, _ = capture.buffer.Write(buffer[:count])
		}
	}
	return count, err
}

func (capture *responsesResponseCapture) Close() error {
	return capture.body.Close()
}

func (capture *responsesResponseCapture) bytes() ([]byte, bool) {
	if capture == nil || capture.overflow {
		return nil, false
	}
	return capture.buffer.Bytes(), true
}

func (a *application) storeResponsesResponse(
	gatewayTokenID string,
	requestState *responsesRequestState,
	account accountConfig,
	contentType string,
	body []byte,
) responsesResponseResult {
	if len(body) == 0 {
		return responsesResponseResult{}
	}
	now := a.responsesNow()
	responseBody := body
	if strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
		completed, status, retryAfter, err := aggregateCodexSSE(body, now)
		if err != nil {
			return responsesResponseResult{terminalFailure: true}
		}
		if status != http.StatusOK {
			classification, classifiedRetryAfter := classifyResponsesTerminalError(completed, status, now)
			if retryAfter == nil {
				retryAfter = classifiedRetryAfter
			}
			return responsesResponseResult{
				terminalFailure: true,
				classification: classification,
				retryAfter:      retryAfter,
			}
		}
		responseBody = completed
	}
	responseID, output, ok := parseCompletedResponsesObject(responseBody)
	if !ok {
		return responsesResponseResult{}
	}
	if requestState == nil || !requestState.cacheable {
		return responsesResponseResult{}
	}
	transcript := append(cloneRawMessages(requestState.input), output...)
	upstreamReusable := requestState.storeEnabled && normalizeAccountAuthType(account.AuthType) != accountAuthCodexOAuth
	a.getResponsesStateCache().store(
		gatewayTokenID, responseID, account.ID, account.Revision, upstreamReusable, transcript, now,
	)
	return responsesResponseResult{}
}

func classifyResponsesTerminalError(body []byte, status int, now time.Time) (string, *time.Duration) {
	if structuredQuotaError(body) {
		retryAfter, _ := codexUsageLimitRetryAfter(body, now)
		return "quota", retryAfter
	}
	if structuredAccountRestrictionError(body) {
		return "account_restricted", nil
	}
	if structuredRequestCompatibilityError(body) {
		return "request_incompatible", nil
	}
	mappedStatus, retryAfter := responsesStreamErrorStatus(body, now)
	switch {
	case mappedStatus == http.StatusUnauthorized || status == http.StatusUnauthorized:
		return "unauthorized", retryAfter
	case mappedStatus == http.StatusTooManyRequests || status == http.StatusTooManyRequests:
		return "rate_limit", retryAfter
	default:
		return "", retryAfter
	}
}

func (a *application) applyResponsesTerminalFailure(
	account accountConfig,
	result responsesResponseResult,
	failureStartedAt time.Time,
) {
	a.recordAccountRequest(account, false)
	switch result.classification {
	case "quota":
		a.markAccountHealthFailure(account)
		a.blockAccountFor(account, "quota", result.retryAfter, failureStartedAt)
	case "account_restricted":
		a.markAccountHealthFailure(account)
		a.blockAccount(account, "restricted")
	case "rate_limit":
		a.markAccountHealthFailure(account)
		a.cooldownAccountForRecovery(account, "rate_limit", result.retryAfter, failureStartedAt)
	case "unauthorized":
		a.markAccountHealthFailure(account)
		a.handleAccountUnauthorized(account, failureStartedAt)
	case "request_incompatible":
		a.clearUpstreamFailures(account, true)
		a.clearUnauthorizedFailure(account)
	default:
		a.markAccountHealthFailure(account)
		a.cooldownAccountForFailure(account, failureStartedAt)
	}
}

func parseCompletedResponsesObject(body []byte) (string, []json.RawMessage, bool) {
	var response map[string]json.RawMessage
	if json.Unmarshal(body, &response) != nil {
		return "", nil, false
	}
	var responseID string
	if json.Unmarshal(response["id"], &responseID) != nil || !validResponsesID(responseID) {
		return "", nil, false
	}
	if rawStatus, exists := response["status"]; exists {
		var status string
		if json.Unmarshal(rawStatus, &status) != nil || status != "completed" {
			return "", nil, false
		}
	}
	rawOutput := bytes.TrimSpace(response["output"])
	if len(rawOutput) == 0 || rawOutput[0] != '[' {
		return "", nil, false
	}
	var output []json.RawMessage
	if json.Unmarshal(rawOutput, &output) != nil {
		return "", nil, false
	}
	canonical, ok := canonicalizeRawMessages(output)
	if !ok {
		return "", nil, false
	}
	return responseID, canonical, true
}

func canonicalizeRawMessages(items []json.RawMessage) ([]json.RawMessage, bool) {
	canonical := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		decoder := json.NewDecoder(bytes.NewReader(item))
		decoder.UseNumber()
		var value any
		if decoder.Decode(&value) != nil {
			return nil, false
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			return nil, false
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, false
		}
		canonical = append(canonical, encoded)
	}
	return canonical, true
}

func inspectResponsesStreamPrelude(response *http.Response, timeout time.Duration, now time.Time) error {
	if response == nil || response.Body == nil {
		return errEmptyUpstreamStream
	}
	originalBody := response.Body
	reader := bufio.NewReader(originalBody)
	var prefix bytes.Buffer
	var lineBuffer bytes.Buffer
	var eventData bytes.Buffer
	timerDone := make(chan struct{})
	timer := time.AfterFunc(timeout, func() {
		_ = originalBody.Close()
		close(timerDone)
	})
	stopTimer := func() bool {
		timedOut := !timer.Stop()
		if timedOut {
			<-timerDone
		}
		return timedOut
	}
	restore := func() {
		response.Body = &readCloser{
			Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), reader),
			Closer: originalBody,
		}
	}
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			_, _ = prefix.Write(fragment)
			if prefix.Len() > responsesStreamPreludeMaxSize {
				stopTimer()
				restore()
				return nil
			}
			_, _ = lineBuffer.Write(fragment)
		}
		if errors.Is(readErr, bufio.ErrBufferFull) {
			continue
		}
		line := lineBuffer.Bytes()
		if len(line) > 0 {
			trimmedLine := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte{'\n'}), []byte{'\r'})
			if len(trimmedLine) == 0 {
				action, status, errorBody, retryAfter := classifyResponsesPreludeEvent(eventData.Bytes(), now)
				eventData.Reset()
				switch action {
				case responsesPreludeContinue:
				case responsesPreludeError:
					stopTimer()
					_ = originalBody.Close()
					setResponsesPreludeError(response, status, errorBody, retryAfter)
					return nil
				default:
					stopTimer()
					restore()
					return nil
				}
			} else if bytes.HasPrefix(trimmedLine, []byte("data:")) {
				value := trimmedLine[len("data:"):]
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
				}
				if eventData.Len() > 0 {
					eventData.WriteByte('\n')
				}
				_, _ = eventData.Write(value)
			}
		}
		lineBuffer.Reset()
		if readErr != nil {
			timedOut := stopTimer()
			if eventData.Len() > 0 {
				action, status, errorBody, retryAfter := classifyResponsesPreludeEvent(eventData.Bytes(), now)
				if action == responsesPreludeError {
					_ = originalBody.Close()
					setResponsesPreludeError(response, status, errorBody, retryAfter)
					return nil
				}
				if action == responsesPreludeOutput {
					restore()
					return nil
				}
			}
			restore()
			if timedOut {
				return errUpstreamIdleTimeout
			}
			if errors.Is(readErr, io.EOF) && prefix.Len() == 0 {
				return errEmptyUpstreamStream
			}
			return readErr
		}
	}
}

type responsesPreludeAction int

const (
	responsesPreludeOutput responsesPreludeAction = iota
	responsesPreludeContinue
	responsesPreludeError
)

func classifyResponsesPreludeEvent(
	data []byte,
	now time.Time,
) (responsesPreludeAction, int, []byte, *time.Duration) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return responsesPreludeContinue, 0, nil, nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		return responsesPreludeOutput, 0, nil, nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(data, &envelope) != nil {
		return responsesPreludeOutput, 0, nil, nil
	}
	var eventType string
	if json.Unmarshal(envelope["type"], &eventType) != nil {
		return responsesPreludeOutput, 0, nil, nil
	}
	switch eventType {
	case "response.created", "response.in_progress", "response.queued":
		if responsesEnvelopeHasOutput(envelope["response"]) {
			return responsesPreludeOutput, 0, nil, nil
		}
		return responsesPreludeContinue, 0, nil, nil
	case "error":
		body := codexErrorBody(envelope["error"])
		status, retryAfter := responsesStreamErrorStatus(body, now)
		return responsesPreludeError, status, body, retryAfter
	case "response.failed":
		var failed map[string]json.RawMessage
		if json.Unmarshal(envelope["response"], &failed) != nil {
			return responsesPreludeOutput, 0, nil, nil
		}
		if responsesEnvelopeHasOutput(envelope["response"]) {
			return responsesPreludeOutput, 0, nil, nil
		}
		body := codexErrorBody(failed["error"])
		status, retryAfter := responsesStreamErrorStatus(body, now)
		return responsesPreludeError, status, body, retryAfter
	default:
		return responsesPreludeOutput, 0, nil, nil
	}
}

func responsesEnvelopeHasOutput(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(raw, &response) != nil {
		return true
	}
	rawOutput, exists := response["output"]
	if !exists {
		return false
	}
	var output []json.RawMessage
	return json.Unmarshal(rawOutput, &output) != nil || len(output) != 0
}

func responsesStreamErrorStatus(body []byte, now time.Time) (int, *time.Duration) {
	if retryAfter, usageLimit := codexUsageLimitRetryAfter(body, now); usageLimit {
		return http.StatusTooManyRequests, retryAfter
	}
	if structuredQuotaError(body) {
		return http.StatusTooManyRequests, nil
	}
	if structuredAccountRestrictionError(body) {
		return http.StatusForbidden, nil
	}
	if structuredRequestCompatibilityError(body) {
		return http.StatusBadRequest, nil
	}
	var payload any
	if json.Unmarshal(body, &payload) == nil {
		identifiers := make([]string, 0, 4)
		messages := make([]string, 0, 2)
		collectErrorFields(payload, &identifiers, &messages)
		for _, identifier := range identifiers {
			normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(identifier)))
			switch normalized {
			case "authentication_error", "invalid_api_key", "invalid_token", "token_expired", "unauthorized":
				return http.StatusUnauthorized, nil
			case "rate_limit_error", "rate_limit_exceeded", "too_many_requests":
				return http.StatusTooManyRequests, nil
			}
		}
		for _, message := range messages {
			message = strings.ToLower(message)
			if strings.Contains(message, "rate limit") || strings.Contains(message, "too many requests") {
				return http.StatusTooManyRequests, nil
			}
			if strings.Contains(message, "authentication") ||
				(strings.Contains(message, "api key") && (strings.Contains(message, "invalid") || strings.Contains(message, "incorrect"))) {
				return http.StatusUnauthorized, nil
			}
		}
	}
	return http.StatusBadGateway, nil
}

func setResponsesPreludeError(response *http.Response, status int, body []byte, retryAfter *time.Duration) {
	response.StatusCode = status
	response.Status = strconv.Itoa(status) + " " + http.StatusText(status)
	response.TransferEncoding = nil
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	response.Header.Set("Content-Type", "application/json; charset=utf-8")
	response.Header.Del("Content-Encoding")
	if retryAfter != nil {
		setCodexRetryAfter(response.Header, *retryAfter)
	}
	setBufferedResponseBody(response, body)
}
