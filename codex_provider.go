package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

const (
	codexOAuthBaseURL            = "https://chatgpt.com/backend-api/codex"
	codexOAuthResponsesPath      = "/v1/responses"
	codexOAuthCompactPath        = "/v1/responses/compact"
	codexOAuthRequestLimit       = 32 << 20
	codexOAuthResponseLimit      = 64 << 20
	codexOAuthErrorResponseLimit = 1 << 20
	codexOAuthTokenLimit         = 32 << 10
	codexOAuthAccountIDLimit     = 4 << 10
	codexOAuthRequestTimeout     = 5 * time.Minute
	codexOAuthMaxRetryAfter      = 31 * 24 * time.Hour
	codexOAuthUserAgent          = "codex-tui/0.135.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.135.0)"
	codexOAuthOriginator         = "codex-tui"
	codexOAuthResponsesBeta      = "responses=experimental"
)

var (
	errCodexOAuthResponseTooLarge = errors.New("Codex OAuth response exceeds the configured size limit")
	errCodexOAuthStreamIncomplete = errors.New("Codex OAuth stream ended before response.completed")
	sharedCodexCloudflareCookieJar = mustCodexCloudflareCookieJar()
)

// codexProviderError describes an error detected before an upstream HTTP
// response can be returned. It deliberately contains no credential values.
type codexProviderError struct {
	Status int
	Code   string
	Err    error
}

func (err *codexProviderError) Error() string {
	if err == nil {
		return "Codex OAuth provider error"
	}
	if err.Err != nil {
		return err.Code + ": " + err.Err.Error()
	}
	return err.Code
}

func (err *codexProviderError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

func (err *codexProviderError) HTTPStatus() int {
	if err == nil || err.Status == 0 {
		return http.StatusBadGateway
	}
	return err.Status
}

type codexProvider struct {
	client        *http.Client
	now           func() time.Time
	websocketDial codexWebSocketDialFunc
	websockets    *codexWebSocketPool
}

type codexCloudflareCookieJar struct {
	inner *cookiejar.Jar
}

func mustCodexCloudflareCookieJar() *codexCloudflareCookieJar {
	inner, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		panic(fmt.Errorf("create Codex Cloudflare cookie jar: %w", err))
	}
	return &codexCloudflareCookieJar{inner: inner}
}

func (jar *codexCloudflareCookieJar) SetCookies(target *url.URL, cookies []*http.Cookie) {
	if jar == nil || jar.inner == nil || !codexCloudflareCookieURL(target) {
		return
	}
	filtered := make([]*http.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		if cookie != nil && codexCloudflareCookieName(cookie.Name) {
			filtered = append(filtered, cookie)
		}
	}
	jar.inner.SetCookies(target, filtered)
}

func (jar *codexCloudflareCookieJar) Cookies(target *url.URL) []*http.Cookie {
	if jar == nil || jar.inner == nil || !codexCloudflareCookieURL(target) {
		return nil
	}
	cookies := jar.inner.Cookies(target)
	filtered := cookies[:0]
	for _, cookie := range cookies {
		if cookie != nil && codexCloudflareCookieName(cookie.Name) {
			filtered = append(filtered, cookie)
		}
	}
	return filtered
}

func codexCloudflareCookieURL(target *url.URL) bool {
	return target != nil && target.Scheme == "https" && strings.EqualFold(target.Hostname(), "chatgpt.com")
}

func codexCloudflareCookieName(name string) bool {
	switch name {
	case "__cf_bm", "__cflb", "__cfruid", "__cfseq", "__cfwaitingroom",
		"_cfuvid", "cf_clearance", "cf_ob_info", "cf_use_ob":
		return true
	default:
		return strings.HasPrefix(name, "cf_chl_")
	}
}

// sendCodexUpstream is the integration point for OAuth-backed Codex accounts.
// It accepts only the two explicitly supported Responses routes and constructs
// the fixed ChatGPT Codex upstream URL internally.
func sendCodexUpstream(
	ctx context.Context,
	client *http.Client,
	original *http.Request,
	body []byte,
	localAccountID string,
	accountRevision int,
	accessToken string,
	chatGPTAccountID string,
) (*http.Response, error) {
	provider := newCodexProvider(client)
	return provider.send(ctx, original, body, localAccountID, accountRevision, accessToken, chatGPTAccountID)
}

func newCodexProvider(client *http.Client) *codexProvider {
	if client == nil {
		client = defaultCodexHTTPClient()
	}
	clientCopy := *client
	var baseTransport *http.Transport
	if clientCopy.Transport == nil {
		baseTransport, _ = http.DefaultTransport.(*http.Transport)
	} else {
		baseTransport, _ = clientCopy.Transport.(*http.Transport)
	}
	clientCopy.Jar = sharedCodexCloudflareCookieJar
	clientCopy.Transport = codexOAuthRoundTripper(clientCopy.Transport)
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &codexProvider{
		client: &clientCopy, now: time.Now,
		websocketDial: newCodexWebSocketDialer(baseTransport),
		websockets:    sharedCodexWebSocketPool,
	}
}

func codexRequestSessionID(header http.Header) (string, error) {
	for _, name := range []string{"Session_id", "session_id", "Session-Id"} {
		if value := strings.TrimSpace(header.Get(name)); validCodexSessionID(value) {
			return value, nil
		}
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:]), nil
}

func validCodexSessionID(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] <= 0x20 || value[index] == 0x7f {
			return false
		}
	}
	return true
}

func defaultCodexHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                  http.ProxyFromEnvironment,
			DialContext:            dialer.DialContext,
			ForceAttemptHTTP2:      true,
			MaxIdleConns:           512,
			MaxIdleConnsPerHost:    128,
			MaxConnsPerHost:        256,
			IdleConnTimeout:        90 * time.Second,
			TLSHandshakeTimeout:    10 * time.Second,
			ExpectContinueTimeout:  time.Second,
			ResponseHeaderTimeout:  30 * time.Second,
			MaxResponseHeaderBytes: 1 << 20,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (provider *codexProvider) send(
	ctx context.Context,
	original *http.Request,
	body []byte,
	localAccountID string,
	accountRevision int,
	accessToken string,
	chatGPTAccountID string,
) (*http.Response, error) {
	if original == nil || original.URL == nil {
		return nil, newCodexProviderError(http.StatusBadRequest, "invalid_codex_request", errors.New("request is nil"))
	}
	if original.Method != http.MethodPost {
		return nil, newCodexProviderError(http.StatusMethodNotAllowed, "unsupported_codex_method", errors.New("only POST is supported"))
	}
	if original.URL.RawQuery != "" || original.URL.Fragment != "" {
		return nil, newCodexProviderError(http.StatusBadRequest, "unsupported_codex_query", errors.New("query parameters and fragments are not supported"))
	}
	if len(body) > codexOAuthRequestLimit {
		return nil, newCodexProviderError(http.StatusRequestEntityTooLarge, "codex_request_too_large", errors.New("request body exceeds 32 MiB"))
	}

	target, compact, err := codexUpstreamTarget(original.URL.Path)
	if err != nil {
		return nil, err
	}
	accessToken, err = validateCodexHeaderCredential(accessToken, codexOAuthTokenLimit, "access token")
	if err != nil {
		return nil, newCodexProviderError(http.StatusUnauthorized, "invalid_codex_access_token", err)
	}
	chatGPTAccountID, err = validateCodexHeaderCredential(chatGPTAccountID, codexOAuthAccountIDLimit, "account ID")
	if err != nil {
		return nil, newCodexProviderError(http.StatusUnauthorized, "invalid_codex_account_id", err)
	}

	upstreamBody, downstreamStream, err := sanitizeCodexResponsesRequest(body, compact)
	if err != nil {
		return nil, err
	}
	if compact {
		downstreamStream = false
	}
	sessionID, err := codexRequestSessionID(original.Header)
	if err != nil {
		return nil, newCodexProviderError(http.StatusInternalServerError, "codex_session_id_generation_failed", err)
	}

	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, codexOAuthRequestTimeout)
	var response *http.Response
	if !compact {
		webSocketResponse, requestSent, webSocketErr := provider.sendCodexWebSocket(
			requestCtx, original, upstreamBody, localAccountID, accountRevision,
			accessToken, chatGPTAccountID, sessionID,
		)
		switch {
		case webSocketErr == nil:
			response = webSocketResponse
		case requestSent:
			response = newCodexWebSocketErrorResponse(
				original, &codexWebSocketRequestSentError{Err: webSocketErr},
			)
		}
	}
	if response == nil {
		request, requestErr := http.NewRequestWithContext(requestCtx, http.MethodPost, target, bytes.NewReader(upstreamBody))
		if requestErr != nil {
			cancel()
			return nil, newCodexProviderError(http.StatusBadGateway, "codex_request_creation_failed", requestErr)
		}
		request.Header.Set("Authorization", "Bearer "+accessToken)
		request.Header.Set("ChatGPT-Account-ID", chatGPTAccountID)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("User-Agent", codexOAuthUserAgent)
		request.Header.Set("Originator", codexOAuthOriginator)
		request.Header.Set("session-id", sessionID)
		request.Header.Set("x-client-request-id", sessionID)
		if compact {
			request.Header.Set("Accept", "application/json")
		} else {
			request.Header.Set("Accept", "text/event-stream")
			request.Header.Set("OpenAI-Beta", codexOAuthResponsesBeta)
		}

		response, err = provider.client.Do(request)
		if err != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			cancel()
			status := http.StatusBadGateway
			if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			return nil, newCodexProviderError(status, "codex_upstream_unavailable", err)
		}
	}
	if response.Body == nil {
		cancel()
		return nil, newCodexProviderError(http.StatusBadGateway, "invalid_codex_response", errors.New("upstream response body is nil"))
	}
	response.Header = codexResponseHeaders(response.Header)

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		buffered, readErr := readAndCloseCodexBody(response.Body, codexOAuthErrorResponseLimit)
		cancel()
		if readErr != nil {
			return nil, newCodexProviderError(http.StatusBadGateway, "invalid_codex_error_response", readErr)
		}
		buffered = redactCodexResponseSecrets(buffered, accessToken, chatGPTAccountID)
		contentType := response.Header.Get("Content-Type")
		if contentType == "" && json.Valid(buffered) {
			contentType = "application/json"
		}
		response = bufferedCodexResponse(response, response.StatusCode, contentType, buffered)
		applyCodexUsageLimit(response, buffered, provider.now())
		return response, nil
	}

	if compact {
		buffered, readErr := readAndCloseCodexBody(response.Body, codexOAuthResponseLimit)
		cancel()
		if readErr != nil {
			return nil, newCodexProviderError(http.StatusBadGateway, "invalid_codex_compact_response", readErr)
		}
		if !validCodexJSONObject(buffered) {
			return nil, newCodexProviderError(http.StatusBadGateway, "invalid_codex_compact_response", errors.New("upstream response is not a JSON object"))
		}
		response = bufferedCodexResponse(response, response.StatusCode, "application/json", buffered)
		applyCodexUsageLimit(response, buffered, provider.now())
		return response, nil
	}

	if codexContentType(response.Header.Get("Content-Type")) != "text/event-stream" {
		_ = response.Body.Close()
		cancel()
		return nil, newCodexProviderError(http.StatusBadGateway, "invalid_codex_stream", errors.New("upstream did not return text/event-stream"))
	}
	if response.ContentLength > codexOAuthResponseLimit {
		_ = response.Body.Close()
		cancel()
		return nil, newCodexProviderError(http.StatusBadGateway, "codex_response_too_large", errCodexOAuthResponseTooLarge)
	}
	if downstreamStream {
		response.Body = &codexLimitedReadCloser{
			body:      response.Body,
			remaining: codexOAuthResponseLimit,
			cancel:    cancel,
		}
		return response, nil
	}

	streamBody, readErr := readAndCloseCodexBody(response.Body, codexOAuthResponseLimit)
	cancel()
	if readErr != nil {
		return nil, newCodexProviderError(http.StatusBadGateway, "invalid_codex_stream", readErr)
	}
	aggregatedBody, status, retryAfter, aggregateErr := aggregateCodexSSE(streamBody, provider.now())
	if aggregateErr != nil {
		return nil, newCodexProviderError(http.StatusBadGateway, "invalid_codex_stream", aggregateErr)
	}
	aggregatedBody = redactCodexResponseSecrets(aggregatedBody, accessToken, chatGPTAccountID)
	contentType := "application/json"
	response = bufferedCodexResponse(response, status, contentType, aggregatedBody)
	if retryAfter != nil {
		setCodexRetryAfter(response.Header, *retryAfter)
	}
	return response, nil
}

func newCodexProviderError(status int, code string, err error) *codexProviderError {
	return &codexProviderError{Status: status, Code: code, Err: err}
}

func codexUpstreamTarget(path string) (target string, compact bool, err error) {
	switch path {
	case codexOAuthResponsesPath:
		return codexOAuthBaseURL + "/responses", false, nil
	case codexOAuthCompactPath:
		return codexOAuthBaseURL + "/responses/compact", true, nil
	default:
		return "", false, newCodexProviderError(http.StatusNotFound, "unsupported_codex_route", errors.New("only /v1/responses and /v1/responses/compact are supported"))
	}
}

func validateCodexHeaderCredential(value string, limit int, label string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "bearer ") {
		value = strings.TrimSpace(value[len("bearer "):])
	}
	if value == "" {
		return "", fmt.Errorf("%s is empty", label)
	}
	if len(value) > limit {
		return "", fmt.Errorf("%s is too large", label)
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] == 0x7f {
			return "", fmt.Errorf("%s contains a control character", label)
		}
	}
	return value, nil
}

func sanitizeCodexResponsesRequest(body []byte, compact bool) ([]byte, bool, error) {
	if len(body) == 0 {
		return nil, false, newCodexProviderError(http.StatusBadRequest, "invalid_codex_json", errors.New("request body is empty"))
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, false, newCodexProviderError(http.StatusBadRequest, "invalid_codex_json", err)
	}
	if payload == nil {
		return nil, false, newCodexProviderError(http.StatusBadRequest, "invalid_codex_json", errors.New("request body must be a JSON object"))
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("request body contains multiple JSON values")
		}
		return nil, false, newCodexProviderError(http.StatusBadRequest, "invalid_codex_json", err)
	}

	downstreamStream := false
	if rawStream, exists := payload["stream"]; exists {
		stream, ok := rawStream.(bool)
		if !ok {
			return nil, false, newCodexProviderError(http.StatusBadRequest, "invalid_codex_stream", errors.New("stream must be a boolean"))
		}
		downstreamStream = stream
	}
	if input, ok := payload["input"].(string); ok {
		payload["input"] = []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": input},
				},
			},
		}
	}
	normalizeCodexInputRoles(payload["input"])
	normalizeCodexTools(payload)

	for _, field := range []string{
		"max_output_tokens",
		"max_completion_tokens",
		"max_tokens",
		"temperature",
		"top_p",
		"top_k",
		"truncation",
		"context_management",
		"user",
		"prompt_cache_retention",
		"safety_identifier",
		"stream_options",
	} {
		delete(payload, field)
	}
	if serviceTier, exists := payload["service_tier"]; exists {
		if tier, ok := serviceTier.(string); !ok || tier != "priority" {
			delete(payload, "service_tier")
		}
	}

	payload["store"] = false
	payload["include"] = []string{"reasoning.encrypted_content"}
	if compact {
		delete(payload, "stream")
	} else {
		payload["stream"] = true
		delete(payload, "previous_response_id")
	}

	cleaned, err := json.Marshal(payload)
	if err != nil {
		return nil, false, newCodexProviderError(http.StatusBadRequest, "invalid_codex_json", err)
	}
	if len(cleaned) > codexOAuthRequestLimit {
		return nil, false, newCodexProviderError(http.StatusRequestEntityTooLarge, "codex_request_too_large", errors.New("sanitized request body exceeds 32 MiB"))
	}
	return cleaned, downstreamStream, nil
}

func normalizeCodexInputRoles(input any) {
	items, ok := input.([]any)
	if !ok {
		return
	}
	for _, item := range items {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if role, ok := message["role"].(string); ok && role == "system" {
			message["role"] = "developer"
		}
	}
}

func normalizeCodexTools(payload map[string]any) {
	normalizeCodexToolArray(payload["tools"])
	toolChoice, ok := payload["tool_choice"].(map[string]any)
	if !ok {
		return
	}
	normalizeCodexToolType(toolChoice)
	normalizeCodexToolArray(toolChoice["tools"])
}

func normalizeCodexToolArray(raw any) {
	tools, ok := raw.([]any)
	if !ok {
		return
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if ok {
			normalizeCodexToolType(tool)
		}
	}
}

func normalizeCodexToolType(tool map[string]any) {
	toolType, ok := tool["type"].(string)
	if ok && (toolType == "web_search_preview" || toolType == "web_search_preview_2025_03_11") {
		tool["type"] = "web_search"
	}
}

func validCodexJSONObject(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil || payload == nil {
		return false
	}
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func readAndCloseCodexBody(body io.ReadCloser, limit int64) ([]byte, error) {
	defer body.Close()
	buffered, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buffered)) > limit {
		return nil, errCodexOAuthResponseTooLarge
	}
	return buffered, nil
}

func bufferedCodexResponse(upstream *http.Response, status int, contentType string, body []byte) *http.Response {
	header := make(http.Header)
	if upstream != nil && upstream.Header != nil {
		header = upstream.Header.Clone()
	}
	header.Del("Content-Encoding")
	header.Del("Transfer-Encoding")
	header.Del("Content-Length")
	header.Set("Content-Type", contentType)
	header.Set("Content-Length", strconv.Itoa(len(body)))
	statusText := http.StatusText(status)
	if statusText == "" {
		statusText = "Unknown Status"
	}
	response := &http.Response{
		Status:        strconv.Itoa(status) + " " + statusText,
		StatusCode:    status,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	if upstream != nil {
		response.Proto = upstream.Proto
		response.ProtoMajor = upstream.ProtoMajor
		response.ProtoMinor = upstream.ProtoMinor
		response.Request = upstream.Request
	}
	return response
}

func codexResponseHeaders(source http.Header) http.Header {
	header := make(http.Header)
	for _, name := range []string{
		"Content-Type",
		"Retry-After",
		"OpenAI-Request-ID",
		"X-Request-ID",
	} {
		if values := source.Values(name); len(values) != 0 {
			for _, value := range values {
				header.Add(name, value)
			}
		}
	}
	header.Set("Cache-Control", "no-store")
	return header
}

func redactCodexResponseSecrets(body []byte, secrets ...string) []byte {
	redacted := bytes.Clone(body)
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret != "" {
			redacted = bytes.ReplaceAll(redacted, []byte(secret), []byte("[redacted]"))
		}
	}
	return redacted
}

func codexContentType(value string) string {
	if separator := strings.IndexByte(value, ';'); separator >= 0 {
		value = value[:separator]
	}
	return strings.ToLower(strings.TrimSpace(value))
}

type codexLimitedReadCloser struct {
	body      io.ReadCloser
	remaining int64
	cancel    context.CancelFunc
}

func (reader *codexLimitedReadCloser) Read(buffer []byte) (int, error) {
	if reader.remaining == 0 {
		var probe [1]byte
		count, err := reader.body.Read(probe[:])
		if count > 0 {
			_ = reader.body.Close()
			reader.cancel()
			return 0, errCodexOAuthResponseTooLarge
		}
		return 0, err
	}
	if int64(len(buffer)) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	count, err := reader.body.Read(buffer)
	reader.remaining -= int64(count)
	return count, err
}

func (reader *codexLimitedReadCloser) Close() error {
	err := reader.body.Close()
	reader.cancel()
	return err
}

type codexSSEAggregation struct {
	completed []byte
	errorBody []byte
}

func aggregateCodexSSE(stream []byte, now time.Time) ([]byte, int, *time.Duration, error) {
	aggregation := codexSSEAggregation{}
	var eventData bytes.Buffer
	consume := func(data []byte) error {
		data = bytes.TrimSpace(data)
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			return nil
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(data, &envelope); err != nil {
			return err
		}
		var eventType string
		if err := json.Unmarshal(envelope["type"], &eventType); err != nil {
			return nil
		}
		switch eventType {
		case "response.completed":
			responseBody := bytes.TrimSpace(envelope["response"])
			if !validCodexJSONObject(responseBody) {
				return errors.New("response.completed does not contain a response object")
			}
			aggregation.completed = bytes.Clone(responseBody)
		case "error":
			aggregation.errorBody = codexErrorBody(envelope["error"])
		case "response.failed":
			var failedResponse map[string]json.RawMessage
			if json.Unmarshal(envelope["response"], &failedResponse) == nil {
				aggregation.errorBody = codexErrorBody(failedResponse["error"])
			}
		}
		return nil
	}
	flush := func() error {
		if eventData.Len() == 0 {
			return nil
		}
		data := bytes.Clone(eventData.Bytes())
		eventData.Reset()
		return consume(data)
	}

	for _, line := range bytes.Split(stream, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			if err := flush(); err != nil {
				return nil, 0, nil, err
			}
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := line[len("data:"):]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		trimmed := bytes.TrimSpace(value)
		if eventData.Len() == 0 && (bytes.Equal(trimmed, []byte("[DONE]")) || json.Valid(trimmed)) {
			if err := consume(trimmed); err != nil {
				return nil, 0, nil, err
			}
			continue
		}
		if eventData.Len() > 0 {
			eventData.WriteByte('\n')
		}
		eventData.Write(value)
	}
	if err := flush(); err != nil {
		return nil, 0, nil, err
	}
	if len(aggregation.completed) != 0 {
		return aggregation.completed, http.StatusOK, nil, nil
	}
	if len(aggregation.errorBody) != 0 {
		retryAfter, usageLimit := codexUsageLimitRetryAfter(aggregation.errorBody, now)
		status := http.StatusBadGateway
		if usageLimit {
			status = http.StatusTooManyRequests
		}
		return aggregation.errorBody, status, retryAfter, nil
	}
	return nil, 0, nil, errCodexOAuthStreamIncomplete
}

func codexErrorBody(raw json.RawMessage) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !json.Valid(raw) {
		return []byte(`{"error":{"message":"Codex upstream request failed","type":"upstream_error"}}`)
	}
	body, err := json.Marshal(map[string]json.RawMessage{"error": raw})
	if err != nil {
		return []byte(`{"error":{"message":"Codex upstream request failed","type":"upstream_error"}}`)
	}
	return body
}

func applyCodexUsageLimit(response *http.Response, body []byte, now time.Time) {
	retryAfter, usageLimit := codexUsageLimitRetryAfter(body, now)
	if !usageLimit {
		return
	}
	response.StatusCode = http.StatusTooManyRequests
	response.Status = "429 " + http.StatusText(http.StatusTooManyRequests)
	if retryAfter != nil {
		setCodexRetryAfter(response.Header, *retryAfter)
	}
}

func codexUsageLimitRetryAfter(body []byte, now time.Time) (*time.Duration, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if decoder.Decode(&payload) != nil || payload == nil {
		return nil, false
	}
	errorPayload, ok := payload["error"].(map[string]any)
	if !ok {
		return nil, false
	}
	errorType, _ := errorPayload["type"].(string)
	if errorType != "usage_limit_reached" {
		return nil, false
	}
	if resetsAt, ok := codexInteger(errorPayload["resets_at"]); ok && resetsAt > 0 {
		resetTime := time.Unix(resetsAt, 0)
		if resetTime.After(now) {
			duration := resetTime.Sub(now)
			return clampCodexRetryAfter(duration), true
		}
	}
	if resetsInSeconds, ok := codexInteger(errorPayload["resets_in_seconds"]); ok && resetsInSeconds > 0 {
		maxSeconds := int64(codexOAuthMaxRetryAfter / time.Second)
		if resetsInSeconds > maxSeconds {
			duration := codexOAuthMaxRetryAfter
			return &duration, true
		}
		duration := time.Duration(resetsInSeconds) * time.Second
		return clampCodexRetryAfter(duration), true
	}
	return nil, true
}

func codexInteger(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		if integer, err := typed.Int64(); err == nil {
			return integer, true
		}
		decimal, err := typed.Float64()
		if err == nil && decimal >= -1<<53 && decimal <= 1<<53 && math.Trunc(decimal) == decimal {
			return int64(decimal), true
		}
	case float64:
		if typed >= -1<<53 && typed <= 1<<53 && math.Trunc(typed) == typed {
			return int64(typed), true
		}
	case string:
		integer, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err == nil {
			return integer, true
		}
	}
	return 0, false
}

func clampCodexRetryAfter(duration time.Duration) *time.Duration {
	if duration <= 0 {
		return nil
	}
	if duration > codexOAuthMaxRetryAfter {
		duration = codexOAuthMaxRetryAfter
	}
	return &duration
}

func setCodexRetryAfter(header http.Header, duration time.Duration) {
	if header == nil || duration <= 0 {
		return
	}
	seconds := int64((duration + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	header.Set("Retry-After", strconv.FormatInt(seconds, 10))
}
