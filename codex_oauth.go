package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	codexOAuthClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexOAuthAuthorizeURL    = "https://auth.openai.com/oauth/authorize"
	codexOAuthTokenURL        = "https://auth.openai.com/oauth/token"
	codexOAuthRedirectURI     = "http://localhost:1455/auth/callback"
	codexOAuthCallbackAddress = "127.0.0.1:1455"
	codexOAuthCallbackHost    = "localhost:1455"
	codexOAuthCallbackPath    = "/auth/callback"
	codexOAuthSuccessPath     = "/success"

	codexOAuthSessionLifetime         = 5 * time.Minute
	codexOAuthDefaultPollInterval     = time.Second
	codexOAuthDefaultSessionLimit     = 1
	codexOAuthHTTPTimeout             = 30 * time.Second
	codexOAuthMaxResponseBody         = 64 << 10
	codexOAuthMaxAdminSecretBytes     = 4096
	codexOAuthMaxTokenLifetime        = 365 * 24 * time.Hour
	codexOAuthSessionIDRandomBytes    = 32
	codexOAuthStateRandomBytes        = 32
	codexOAuthVerifierRandomBytes     = 64
	codexOAuthMaxAuthorizationCodeLen = 4096
	codexOAuthCallbackShutdownTimeout = 2 * time.Second
)

var (
	errCodexOAuthInvalidAdminSession = errors.New("Codex OAuth 管理员会话标识无效")
	errCodexOAuthSessionLimit        = errors.New("Codex OAuth 登录会话数量已达上限")
	errCodexOAuthSessionNotFound     = errors.New("Codex OAuth 登录会话不存在")
	errCodexOAuthSessionExpired      = errors.New("Codex OAuth 登录会话已过期")
	errCodexOAuthAccountInvalid      = errors.New("Codex OAuth 目标账号无效")
	errCodexOAuthCallbackUnavailable = errors.New("Codex OAuth 本地回调端口不可用")
	errCodexOAuthRefreshTokenMissing = errors.New("Codex OAuth Refresh Token 为空")
)

type codexBrowserPollStatus string

const (
	codexBrowserPollPending  codexBrowserPollStatus = "pending"
	codexBrowserPollComplete codexBrowserPollStatus = "complete"
	codexBrowserPollFailed   codexBrowserPollStatus = "failed"
)

// codexAdminSessionID is an opaque digest used only to bind a browser login to
// the already-authenticated management session that created it. The caller must
// validate the administrator session before deriving this value.
type codexAdminSessionID struct {
	digest [sha256.Size]byte
}

// newCodexAdminSessionID hashes an administrator session secret so the OAuth
// manager never retains the raw cookie or CSRF token. Public administration can
// pass the validated admin cookie value; local administration can pass the
// application CSRF token.
func newCodexAdminSessionID(sessionSecret string) (codexAdminSessionID, error) {
	if sessionSecret == "" || len(sessionSecret) > codexOAuthMaxAdminSecretBytes {
		return codexAdminSessionID{}, errCodexOAuthInvalidAdminSession
	}
	return codexAdminSessionID{digest: sha256.Sum256([]byte(sessionSecret))}, nil
}

func (id codexAdminSessionID) valid() bool {
	return id.digest != [sha256.Size]byte{}
}

// codexOAuthCredentials contains server-side secrets and display metadata.
// Secret fields are excluded from JSON to reduce the chance of returning them
// through an administration response accidentally.
type codexOAuthCredentials struct {
	AccessToken  string    `json:"-"`
	RefreshToken string    `json:"-"`
	IDToken      string    `json:"-"`
	TokenType    string    `json:"-"`
	ExpiresAt    time.Time `json:"expiresAt,omitempty"`

	// AccountID, Email and PlanType are decoded from an unverified JWT payload.
	// They are display metadata only and must never authorize a request,
	// establish ownership, influence routing, or replace a validated administrator session.
	AccountID string `json:"accountId,omitempty"`
	Email     string `json:"email,omitempty"`
	PlanType  string `json:"planType,omitempty"`
}

type codexBrowserLogin struct {
	SessionID           string    `json:"sessionId"`
	AuthorizationURL    string    `json:"authorizationUrl"`
	ExpiresAt           time.Time `json:"expiresAt"`
	PollIntervalSeconds int       `json:"pollIntervalSeconds"`
	AccountID           string    `json:"accountId"`
	AccountRevision     int       `json:"accountRevision"`
}

type codexBrowserPollResult struct {
	Status            codexBrowserPollStatus `json:"status"`
	RetryAfterSeconds int                    `json:"retryAfterSeconds,omitempty"`
	ExpiresAt         time.Time              `json:"expiresAt,omitempty"`
	AccountID         string                 `json:"accountId"`
	AccountRevision   int                    `json:"accountRevision"`
	Error             string                 `json:"error,omitempty"`
	Credentials       *codexOAuthCredentials  `json:"-"`
}

type codexOAuthUpstreamError struct {
	Operation  string
	StatusCode int
}

func (e *codexOAuthUpstreamError) Error() string {
	return fmt.Sprintf("Codex OAuth %s 返回 HTTP %d", e.Operation, e.StatusCode)
}

type codexBrowserSession struct {
	owner           codexAdminSessionID
	accountID       string
	accountRevision int
	state           string
	codeVerifier    string
	expiresAt       time.Time
	status          codexBrowserPollStatus
	failure         string
	credentials     *codexOAuthCredentials
	server          *http.Server
}

type codexRefreshCall struct {
	done        chan struct{}
	credentials codexOAuthCredentials
	err         error
}

// codexAuthManager owns one short-lived browser login and collapses concurrent
// refreshes for the same account.
type codexAuthManager struct {
	client      *http.Client
	maxSessions int
	now         func() time.Time

	sessionsMu sync.Mutex
	sessions   map[string]*codexBrowserSession

	refreshMu sync.Mutex
	refreshes map[string]*codexRefreshCall
}

func newCodexAuthManager(client *http.Client, _ int) *codexAuthManager {
	if client == nil {
		client = &http.Client{}
	} else {
		cloned := *client
		client = &cloned
	}
	if client.Timeout <= 0 || client.Timeout > codexOAuthHTTPTimeout {
		client.Timeout = codexOAuthHTTPTimeout
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &codexAuthManager{
		client:      client,
		maxSessions: codexOAuthDefaultSessionLimit,
		now:         time.Now,
		sessions:    make(map[string]*codexBrowserSession),
		refreshes:   make(map[string]*codexRefreshCall),
	}
}

func (m *codexAuthManager) startBrowserLogin(
	ctx context.Context,
	owner codexAdminSessionID,
	accountID string,
	revision int,
) (codexBrowserLogin, error) {
	if !owner.valid() {
		return codexBrowserLogin{}, errCodexOAuthInvalidAdminSession
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || len(accountID) > 128 || revision < 1 {
		return codexBrowserLogin{}, errCodexOAuthAccountInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return codexBrowserLogin{}, err
	}

	sessionID, err := randomCodexSessionID()
	if err != nil {
		return codexBrowserLogin{}, err
	}
	state, err := randomCodexOAuthValue(codexOAuthStateRandomBytes)
	if err != nil {
		return codexBrowserLogin{}, err
	}
	codeVerifier, codeChallenge, err := generateCodexPKCE()
	if err != nil {
		return codexBrowserLogin{}, err
	}
	authorizationURL := codexBrowserAuthorizationURL(state, codeChallenge)
	expiresAt, err := m.reserveBrowserSession(sessionID, &codexBrowserSession{
		owner:           owner,
		accountID:       accountID,
		accountRevision: revision,
		state:           state,
		codeVerifier:    codeVerifier,
		status:          codexBrowserPollPending,
	})
	if err != nil {
		return codexBrowserLogin{}, err
	}

	return codexBrowserLogin{
		SessionID:           sessionID,
		AuthorizationURL:    authorizationURL,
		ExpiresAt:           expiresAt,
		PollIntervalSeconds: int(codexOAuthDefaultPollInterval / time.Second),
		AccountID:           accountID,
		AccountRevision:     revision,
	}, nil
}

func (m *codexAuthManager) pollBrowserLogin(owner codexAdminSessionID, sessionID string) (codexBrowserPollResult, error) {
	if !owner.valid() {
		return codexBrowserPollResult{}, errCodexOAuthInvalidAdminSession
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || len(sessionID) > 128 {
		return codexBrowserPollResult{}, errCodexOAuthSessionNotFound
	}

	now := m.now()
	m.sessionsMu.Lock()
	session, ok := m.sessions[sessionID]
	if !ok || session.owner != owner {
		m.sessionsMu.Unlock()
		return codexBrowserPollResult{}, errCodexOAuthSessionNotFound
	}
	if !now.Before(session.expiresAt) {
		delete(m.sessions, sessionID)
		session.state = ""
		session.codeVerifier = ""
		server := session.server
		m.sessionsMu.Unlock()
		shutdownCodexBrowserServer(server)
		return codexBrowserPollResult{}, errCodexOAuthSessionExpired
	}
	result := codexBrowserPollResult{
		Status:          session.status,
		ExpiresAt:       session.expiresAt,
		AccountID:       session.accountID,
		AccountRevision: session.accountRevision,
		Error:           session.failure,
	}
	switch session.status {
	case codexBrowserPollPending:
		result.RetryAfterSeconds = int(codexOAuthDefaultPollInterval / time.Second)
	case codexBrowserPollComplete:
		if session.credentials == nil {
			result.Status = codexBrowserPollFailed
			result.Error = "token_exchange_failed"
		} else {
			credentials := *session.credentials
			result.ExpiresAt = credentials.ExpiresAt
			result.Credentials = &credentials
		}
		delete(m.sessions, sessionID)
	case codexBrowserPollFailed:
		delete(m.sessions, sessionID)
	default:
		result.Status = codexBrowserPollFailed
		result.Error = "invalid_session_state"
		delete(m.sessions, sessionID)
	}
	m.sessionsMu.Unlock()
	return result, nil
}

// refreshTokens coalesces simultaneous refreshes for the same account so a
// rotating Refresh Token is submitted only once. All concurrent callers receive
// the same result.
func (m *codexAuthManager) refreshTokens(ctx context.Context, current codexOAuthCredentials) (codexOAuthCredentials, error) {
	if strings.TrimSpace(current.RefreshToken) == "" {
		return codexOAuthCredentials{}, errCodexOAuthRefreshTokenMissing
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := codexRefreshKey(current)

	m.refreshMu.Lock()
	if call := m.refreshes[key]; call != nil {
		m.refreshMu.Unlock()
		select {
		case <-ctx.Done():
			return codexOAuthCredentials{}, ctx.Err()
		case <-call.done:
			return call.credentials, call.err
		}
	}
	call := &codexRefreshCall{done: make(chan struct{})}
	m.refreshes[key] = call
	m.refreshMu.Unlock()

	credentials, err := m.refreshTokensOnce(ctx, current)

	m.refreshMu.Lock()
	call.credentials = credentials
	call.err = err
	delete(m.refreshes, key)
	close(call.done)
	m.refreshMu.Unlock()
	return credentials, err
}

func (m *codexAuthManager) reserveBrowserSession(sessionID string, session *codexBrowserSession) (time.Time, error) {
	for {
		now := m.now()
		m.sessionsMu.Lock()
		expiredServers := m.cleanupExpiredBrowserSessionsLocked(now)
		if len(expiredServers) != 0 {
			m.sessionsMu.Unlock()
			for _, server := range expiredServers {
				shutdownCodexBrowserServer(server)
			}
			continue
		}
		if len(m.sessions) >= m.maxSessions {
			m.sessionsMu.Unlock()
			return time.Time{}, errCodexOAuthSessionLimit
		}
		if _, exists := m.sessions[sessionID]; exists {
			m.sessionsMu.Unlock()
			return time.Time{}, errCodexOAuthSessionLimit
		}

		listener, err := net.Listen("tcp4", codexOAuthCallbackAddress)
		if err != nil {
			m.sessionsMu.Unlock()
			return time.Time{}, fmt.Errorf("%w: %v", errCodexOAuthCallbackUnavailable, err)
		}
		expiresAt := now.Add(codexOAuthSessionLifetime)
		session.expiresAt = expiresAt
		mux := http.NewServeMux()
		server := &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      codexOAuthHTTPTimeout + 5*time.Second,
			IdleTimeout:       30 * time.Second,
			MaxHeaderBytes:    16 << 10,
			ErrorLog:          log.New(io.Discard, "", 0),
		}
		session.server = server
		mux.HandleFunc(codexOAuthCallbackPath, func(w http.ResponseWriter, r *http.Request) {
			m.handleBrowserCallback(sessionID, session, server, w, r)
		})
		mux.HandleFunc(codexOAuthSuccessPath, func(w http.ResponseWriter, r *http.Request) {
			m.handleBrowserSuccess(session, server, w, r)
		})
		m.sessions[sessionID] = session
		m.sessionsMu.Unlock()

		go func() {
			if errServe := server.Serve(listener); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
				m.failBrowserSession(sessionID, session, "callback_server_failed")
			}
		}()
		time.AfterFunc(codexOAuthSessionLifetime, func() {
			m.expireBrowserSession(sessionID, session)
		})
		return expiresAt, nil
	}
}

func (m *codexAuthManager) cleanupExpiredBrowserSessionsLocked(now time.Time) []*http.Server {
	var servers []*http.Server
	for sessionID, session := range m.sessions {
		if session == nil || !now.Before(session.expiresAt) {
			delete(m.sessions, sessionID)
			if session != nil {
				session.state = ""
				session.codeVerifier = ""
				if session.server != nil {
					servers = append(servers, session.server)
				}
			}
		}
	}
	return servers
}

func (m *codexAuthManager) expireBrowserSession(sessionID string, expected *codexBrowserSession) {
	m.sessionsMu.Lock()
	session, ok := m.sessions[sessionID]
	if !ok || session != expected {
		m.sessionsMu.Unlock()
		return
	}
	delete(m.sessions, sessionID)
	session.state = ""
	session.codeVerifier = ""
	server := session.server
	m.sessionsMu.Unlock()
	shutdownCodexBrowserServer(server)
}

func (m *codexAuthManager) handleBrowserCallback(
	sessionID string,
	expected *codexBrowserSession,
	server *http.Server,
	w http.ResponseWriter,
	r *http.Request,
) {
	setCodexBrowserResponseHeaders(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.EqualFold(r.Host, codexOAuthCallbackHost) || r.URL.Path != codexOAuthCallbackPath {
		http.NotFound(w, r)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "invalid callback", http.StatusBadRequest)
		return
	}
	stateValues := query["state"]
	if len(stateValues) != 1 || stateValues[0] == "" {
		http.Error(w, "invalid state", http.StatusForbidden)
		return
	}
	codeVerifier, ok := m.consumeBrowserState(sessionID, expected, stateValues[0])
	if !ok {
		http.Error(w, "invalid state", http.StatusForbidden)
		return
	}

	failure := ""
	if errorValues := query["error"]; len(errorValues) != 0 {
		failure = "authorization_denied"
	}
	codeValues := query["code"]
	code := ""
	if failure == "" {
		if len(codeValues) != 1 {
			failure = "authorization_code_missing"
		} else {
			code = strings.TrimSpace(codeValues[0])
			if code == "" || len(code) > codexOAuthMaxAuthorizationCodeLen {
				failure = "authorization_code_invalid"
			}
		}
	}

	if failure != "" {
		m.failBrowserSession(sessionID, expected, failure)
	} else {
		exchangeCtx, cancel := context.WithTimeout(context.Background(), codexOAuthHTTPTimeout)
		credentials, exchangeErr := m.exchangeBrowserAuthorizationCode(exchangeCtx, code, codeVerifier)
		cancel()
		if exchangeErr != nil {
			m.failBrowserSession(sessionID, expected, "token_exchange_failed")
		} else {
			m.completeBrowserSession(sessionID, expected, credentials)
		}
	}
	http.Redirect(w, r, codexOAuthSuccessPath, http.StatusSeeOther)
	time.AfterFunc(10*time.Second, func() {
		shutdownCodexBrowserServer(server)
	})
}

func (m *codexAuthManager) consumeBrowserState(
	sessionID string,
	expected *codexBrowserSession,
	state string,
) (string, bool) {
	now := m.now()
	m.sessionsMu.Lock()
	session, ok := m.sessions[sessionID]
	if !ok || session != expected || session.status != codexBrowserPollPending ||
		!now.Before(session.expiresAt) || session.state == "" || session.state != state {
		m.sessionsMu.Unlock()
		return "", false
	}
	codeVerifier := session.codeVerifier
	session.state = ""
	session.codeVerifier = ""
	m.sessionsMu.Unlock()
	return codeVerifier, codeVerifier != ""
}

func (m *codexAuthManager) completeBrowserSession(
	sessionID string,
	expected *codexBrowserSession,
	credentials codexOAuthCredentials,
) {
	m.sessionsMu.Lock()
	session, ok := m.sessions[sessionID]
	if ok && session == expected && session.status == codexBrowserPollPending {
		stored := credentials
		session.credentials = &stored
		session.status = codexBrowserPollComplete
		session.failure = ""
	}
	m.sessionsMu.Unlock()
}

func (m *codexAuthManager) failBrowserSession(sessionID string, expected *codexBrowserSession, failure string) {
	m.sessionsMu.Lock()
	session, ok := m.sessions[sessionID]
	if ok && session == expected && session.status == codexBrowserPollPending {
		session.state = ""
		session.codeVerifier = ""
		session.credentials = nil
		session.status = codexBrowserPollFailed
		session.failure = failure
	}
	m.sessionsMu.Unlock()
}

func (m *codexAuthManager) handleBrowserSuccess(
	session *codexBrowserSession,
	server *http.Server,
	w http.ResponseWriter,
	r *http.Request,
) {
	setCodexBrowserResponseHeaders(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.EqualFold(r.Host, codexOAuthCallbackHost) || r.URL.Path != codexOAuthSuccessPath || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	m.sessionsMu.Lock()
	terminal := session != nil && (session.status == codexBrowserPollComplete || session.status == codexBrowserPollFailed)
	m.sessionsMu.Unlock()
	if !terminal {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, codexOAuthSuccessHTML)
	go shutdownCodexBrowserServer(server)
}

func setCodexBrowserResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func shutdownCodexBrowserServer(server *http.Server) {
	if server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), codexOAuthCallbackShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
	}
}

type codexOAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

func codexBrowserAuthorizationURL(state, codeChallenge string) string {
	params := url.Values{
		"client_id":                  {codexOAuthClientID},
		"response_type":              {"code"},
		"redirect_uri":               {codexOAuthRedirectURI},
		"scope":                      {"openid email profile offline_access"},
		"state":                      {state},
		"code_challenge":             {codeChallenge},
		"code_challenge_method":      {"S256"},
		"prompt":                     {"login"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return codexOAuthAuthorizeURL + "?" + params.Encode()
}

func generateCodexPKCE() (string, string, error) {
	codeVerifier, err := randomCodexOAuthValue(codexOAuthVerifierRandomBytes)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256([]byte(codeVerifier))
	return codeVerifier, base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

func (m *codexAuthManager) exchangeBrowserAuthorizationCode(ctx context.Context, code, codeVerifier string) (codexOAuthCredentials, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {codexOAuthClientID},
		"code":          {strings.TrimSpace(code)},
		"redirect_uri":  {codexOAuthRedirectURI},
		"code_verifier": {strings.TrimSpace(codeVerifier)},
	}
	response, err := m.postTokenForm(ctx, "交换浏览器授权码", form)
	if err != nil {
		return codexOAuthCredentials{}, err
	}
	return m.credentialsFromTokenResponse(response, codexOAuthCredentials{})
}

func (m *codexAuthManager) refreshTokensOnce(ctx context.Context, current codexOAuthCredentials) (codexOAuthCredentials, error) {
	form := url.Values{
		"client_id":     {codexOAuthClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {strings.TrimSpace(current.RefreshToken)},
		"scope":         {"openid profile email"},
	}
	response, err := m.postTokenForm(ctx, "刷新 Token", form)
	if err != nil {
		return codexOAuthCredentials{}, err
	}
	return m.credentialsFromTokenResponse(response, current)
}

func (m *codexAuthManager) postTokenForm(ctx context.Context, operation string, form url.Values) (codexOAuthTokenResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, codexOAuthTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return codexOAuthTokenResponse{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := m.client.Do(request)
	if err != nil {
		return codexOAuthTokenResponse{}, err
	}
	defer response.Body.Close()
	responseBody, err := readCodexOAuthBody(response.Body)
	if err != nil {
		return codexOAuthTokenResponse{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return codexOAuthTokenResponse{}, &codexOAuthUpstreamError{Operation: operation, StatusCode: response.StatusCode}
	}
	var payload codexOAuthTokenResponse
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return codexOAuthTokenResponse{}, errors.New("Codex OAuth Token 响应无效")
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return codexOAuthTokenResponse{}, errors.New("Codex OAuth Token 响应缺少 Access Token")
	}
	return payload, nil
}

func (m *codexAuthManager) credentialsFromTokenResponse(response codexOAuthTokenResponse, previous codexOAuthCredentials) (codexOAuthCredentials, error) {
	credentials := previous
	credentials.AccessToken = strings.TrimSpace(response.AccessToken)
	if refreshToken := strings.TrimSpace(response.RefreshToken); refreshToken != "" {
		credentials.RefreshToken = refreshToken
	}
	if idToken := strings.TrimSpace(response.IDToken); idToken != "" {
		credentials.IDToken = idToken
	}
	credentials.TokenType = strings.TrimSpace(response.TokenType)
	if credentials.TokenType == "" {
		credentials.TokenType = "Bearer"
	}
	credentials.ExpiresAt = tokenExpiry(m.now(), response.ExpiresIn)
	if credentials.RefreshToken == "" {
		return codexOAuthCredentials{}, errors.New("Codex OAuth Token 响应缺少 Refresh Token")
	}
	if credentials.IDToken != "" {
		identity, err := parseCodexOAuthDisplayIdentity(credentials.IDToken)
		if err == nil {
			credentials.AccountID = identity.AccountID
			credentials.Email = identity.Email
			credentials.PlanType = identity.PlanType
		}
	}
	return credentials, nil
}

type codexOAuthDisplayIdentity struct {
	AccountID string
	Email     string
	PlanType  string
}

// parseCodexOAuthDisplayIdentity decodes only the JWT payload. It deliberately
// does not verify the signature, issuer, audience or expiry. The values may be
// shown to the administrator and the server-issued account ID may be echoed
// back to the fixed Codex upstream, but none of them grants local permissions.
func parseCodexOAuthDisplayIdentity(idToken string) (codexOAuthDisplayIdentity, error) {
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) != 3 {
		return codexOAuthDisplayIdentity{}, errors.New("Codex OAuth ID Token 格式无效")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return codexOAuthDisplayIdentity{}, errors.New("Codex OAuth ID Token Payload 无效")
	}
	var claims struct {
		Email string `json:"email"`
		Auth  struct {
			AccountID string `json:"chatgpt_account_id"`
			PlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return codexOAuthDisplayIdentity{}, errors.New("Codex OAuth ID Token Claims 无效")
	}
	return codexOAuthDisplayIdentity{
		AccountID: strings.TrimSpace(claims.Auth.AccountID),
		Email:     strings.TrimSpace(claims.Email),
		PlanType:  strings.TrimSpace(claims.Auth.PlanType),
	}, nil
}

func tokenExpiry(now time.Time, expiresIn int64) time.Time {
	if expiresIn <= 0 {
		return time.Time{}
	}
	maxSeconds := int64(codexOAuthMaxTokenLifetime / time.Second)
	if expiresIn > maxSeconds {
		expiresIn = maxSeconds
	}
	return now.Add(time.Duration(expiresIn) * time.Second).UTC()
}

func codexRefreshKey(credentials codexOAuthCredentials) string {
	// Use the server-issued Refresh Token rather than unverified JWT display
	// claims to identify concurrent refreshes of the same stored credential.
	digest := sha256.Sum256([]byte("refresh:" + strings.TrimSpace(credentials.RefreshToken)))
	return string(digest[:])
}

func randomCodexSessionID() (string, error) {
	value, err := randomCodexOAuthValue(codexOAuthSessionIDRandomBytes)
	if err != nil {
		return "", err
	}
	return "codex_" + value, nil
}

func randomCodexOAuthValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func readCodexOAuthBody(reader io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, codexOAuthMaxResponseBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > codexOAuthMaxResponseBody {
		return nil, errors.New("Codex OAuth 响应过大")
	}
	return body, nil
}

const codexOAuthSuccessHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>Codex 登录已处理</title>
</head>
<body>
<main>
<h1>Codex 登录已处理</h1>
<p>请关闭此页面并返回管理页面查看登录结果。</p>
</main>
</body>
</html>`
