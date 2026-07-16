package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	maxAdminBody    = 1 << 20
	modelsProbeTime = 30 * time.Second
	maxModelsBody   = 1 << 20
)

var (
	errCodexAccountNotFound = errors.New("Codex OAuth 账号不存在")
	errCodexAccountChanged  = errors.New("Codex OAuth 账号配置已变化")
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/assets/css web/assets/js
var webAssets embed.FS

type saveRequest struct {
	Accounts               *[]accountInput `json:"accounts"`
	Strategy               *string         `json:"strategy"`
	AllowInsecureHTTP      *bool           `json:"allowInsecureHttp"`
	RotateGatewayToken     bool            `json:"rotateGatewayToken"`
	CreateGatewayTokenName string          `json:"createGatewayTokenName"`
	DeleteGatewayTokenID   string          `json:"deleteGatewayTokenId"`
	RotateGatewayTokenID   string          `json:"rotateGatewayTokenId"`
	AllowPublicAccess      *bool           `json:"allowPublicAccess"`
	PublicBaseURL          *string         `json:"publicBaseUrl"`
	AllowPublicAdmin       *bool           `json:"allowPublicAdmin"`
	AdminPassword          string          `json:"adminPassword"`
	ClearAdminPassword     bool            `json:"clearAdminPassword"`
}

type adminLoginRequest struct {
	Password string `json:"password"`
}

type gatewayTokenSnippetRequest struct {
	ID     string `json:"id"`
	Target string `json:"target"`
}

type accountProbeRequest struct {
	AccountID         string        `json:"accountId"`
	Candidate         *accountInput `json:"candidate"`
	AllowInsecureHTTP *bool         `json:"allowInsecureHttp"`
}

type balanceRefreshRequest struct {
	AccountIDs []string `json:"accountIds"`
}

type codexOAuthStartRequest struct {
	AccountID string `json:"accountId"`
}

type codexOAuthStatusRequest struct {
	SessionID string `json:"sessionId"`
}

type codexUsageRefreshRequest struct {
	AccountIDs []string `json:"accountIds"`
}

type codexUsageRefreshReport struct {
	AccountID string              `json:"accountId"`
	OK        bool                `json:"ok"`
	Error     string              `json:"error,omitempty"`
	Usage     *codexUsageSnapshot `json:"usage,omitempty"`
}

type publicAccount struct {
	ID                     string `json:"id"`
	Name                   string `json:"name"`
	AuthType               string `json:"authType"`
	Provider               string `json:"provider"`
	BaseURL                string `json:"baseUrl"`
	CodexAuthenticated     bool   `json:"codexAuthenticated"`
	CodexEmail             string `json:"codexEmail,omitempty"`
	CodexPlanType          string `json:"codexPlanType,omitempty"`
	CodexExpiresAt         string `json:"codexExpiresAt,omitempty"`
	DisableCodexCredits    bool   `json:"disableCodexCredits"`
	QuotaResetPeriod       string `json:"quotaResetPeriod,omitempty"`
	QuotaResetTimezone     string `json:"quotaResetTimezone,omitempty"`
	QuotaResetEvery        int    `json:"quotaResetEvery,omitempty"`
	QuotaResetUnit         string `json:"quotaResetUnit,omitempty"`
	QuotaResetAnchorAt     string `json:"quotaResetAnchorAt,omitempty"`
	NewAPIAuthMode         string `json:"newApiAuthMode"`
	NewAPIUsername         string `json:"newApiUsername,omitempty"`
	NewAPIUserID           int    `json:"newApiUserId,omitempty"`
	Enabled                bool   `json:"enabled"`
	Revision               int    `json:"revision"`
	Verified               bool   `json:"verified"`
	BlockedReason          string `json:"blockedReason,omitempty"`
	KeyConfigured          bool   `json:"keyConfigured"`
	NewAPISecretConfigured bool   `json:"newApiSecretConfigured"`
}

type publicConfig struct {
	Version                 int             `json:"version"`
	Accounts                []publicAccount `json:"accounts"`
	Strategy                string          `json:"strategy"`
	AllowInsecureHTTP       bool            `json:"allowInsecureHttp"`
	GatewayTokenCount       int             `json:"gatewayTokenCount"`
	AllowPublicAccess       bool            `json:"allowPublicAccess"`
	PublicBaseURL           string          `json:"publicBaseUrl,omitempty"`
	AllowPublicAdmin        bool            `json:"allowPublicAdmin"`
	AdminPasswordConfigured bool            `json:"adminPasswordConfigured"`
	LastSwitchReason        string          `json:"lastSwitchReason,omitempty"`
	LastSwitchAt            string          `json:"lastSwitchAt,omitempty"`
}

type publicGatewayToken struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"createdAt,omitempty"`
}

func (a *application) routes() http.Handler {
	mux := http.NewServeMux()
	assetRoot, err := fs.Sub(webAssets, "web/assets")
	if err != nil {
		panic(fmt.Errorf("embedded web assets: %w", err))
	}
	assetServer := http.StripPrefix("/assets/", http.FileServer(http.FS(assetRoot)))
	mux.HandleFunc("/", a.handleIndex)
	mux.Handle("/assets/", a.handleWebAssets(assetServer))
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/v1", a.handleProxy)
	mux.HandleFunc("/v1/", a.handleProxy)
	mux.HandleFunc("/admin/", a.handleAdmin)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		access := a.classifyRequest(r)
		if access == requestAccessDenied {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		if access == requestAccessPublic {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *application) handleWebAssets(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if a.classifyRequest(r) == requestAccessPublic && !a.publicAdminEnabled() {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		assetPath := strings.TrimPrefix(r.URL.Path, "/assets/")
		if assetPath == "" || strings.HasSuffix(assetPath, "/") {
			http.NotFound(w, r)
			return
		}
		switch path.Ext(assetPath) {
		case ".css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case ".mjs":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		default:
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func validLocalHost(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || port != "4000" {
		return false
	}
	return host == "127.0.0.1" || strings.EqualFold(host, "localhost")
}

func validLocalOrigin(value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return validLocalHost(parsed.Host)
}

func (a *application) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.classifyRequest(r) == requestAccessPublic && !a.publicAdminEnabled() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'; img-src 'self'; style-src 'self'; script-src 'self'; connect-src 'self'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(indexHTML)))
	if r.Method == http.MethodGet {
		_, _ = w.Write(indexHTML)
	}
}

func (a *application) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.classifyRequest(r) == requestAccessPublic {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "codex-quota-router", "version": applicationVersion})
}

func (a *application) handleAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	access, sessionRequired := a.classifyAdminRequest(r)
	if access == requestAccessDenied || (access == requestAccessPublic && !a.publicAdminEnabled()) {
		writeAdminError(w, http.StatusNotFound, "管理接口不存在")
		return
	}
	endpoint := strings.TrimPrefix(r.URL.Path, "/admin")
	endpoint = strings.TrimPrefix(endpoint, "/api")
	if endpoint == "/login" && r.Method == http.MethodPost {
		if !sessionRequired || !a.validAdminOrigin(r.Header.Get("Origin"), access) {
			writeAdminError(w, http.StatusForbidden, "不允许的登录来源")
			return
		}
		a.handleAdminLogin(w, r, access)
		return
	}
	if sessionRequired && !a.adminSessionValid(r) {
		writeAdminError(w, http.StatusUnauthorized, "请先登录管理页面")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !a.validAdminOrigin(origin, access) {
		writeAdminError(w, http.StatusForbidden, "不允许的请求来源")
		return
	}
	unsafeMethod := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
	if unsafeMethod {
		if !a.validAdminOrigin(r.Header.Get("Origin"), access) {
			writeAdminError(w, http.StatusForbidden, "写操作必须来自已允许的管理页面")
			return
		}
		if !constantEqual(r.Header.Get("X-CSRF-Token"), a.csrfToken) {
			writeAdminError(w, http.StatusForbidden, "CSRF 校验失败")
			return
		}
	}
	switch {
	case endpoint == "/bootstrap" && r.Method == http.MethodGet:
		a.handleBootstrap(w)
	case endpoint == "/status" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, a.status())
	case endpoint == "/config" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, a.publicConfig())
	case endpoint == "/config" && r.Method == http.MethodPut:
		a.handleSave(w, r, access)
	case endpoint == "/save" && r.Method == http.MethodPost:
		a.handleSave(w, r, access)
	case endpoint == "/codex" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"snippet": a.codexSnippet()})
	case endpoint == "/codex-config" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"snippet": a.codexSnippet()})
	case endpoint == "/gateway-tokens" && r.Method == http.MethodGet:
		a.handleGatewayTokens(w, r)
	case endpoint == "/gateway-tokens/snippet" && r.Method == http.MethodPost:
		a.handleGatewayTokenSnippet(w, r)
	case endpoint == "/logout" && r.Method == http.MethodPost:
		a.clearAdminSession(w, r, access)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case endpoint == "/models" && r.Method == http.MethodPost:
		a.handleModels(w, r)
	case endpoint == "/balances/refresh" && r.Method == http.MethodPost:
		a.handleBalancesRefresh(w, r)
	case endpoint == "/balances/test" && r.Method == http.MethodPost:
		a.handleBalanceTest(w, r)
	case endpoint == "/accounts/codex/oauth/start" && r.Method == http.MethodPost:
		a.handleCodexOAuthStart(w, r, access, sessionRequired)
	case endpoint == "/accounts/codex/oauth/status" && r.Method == http.MethodPost:
		a.handleCodexOAuthStatus(w, r, sessionRequired)
	case endpoint == "/accounts/codex/logout" && r.Method == http.MethodPost:
		a.handleCodexLogout(w, r)
	case endpoint == "/accounts/codex/usage" && r.Method == http.MethodPost:
		a.handleCodexUsageRefresh(w, r)
	case endpoint == "/accounts/reset" && r.Method == http.MethodPost:
		a.handleAccountReset(w, r)
	case endpoint == "/accounts/resume" && r.Method == http.MethodPost:
		a.handleAccountResume(w, r)
	case endpoint == "/accounts/statistics/clear" && r.Method == http.MethodPost:
		a.handleAccountStatisticsClear(w, r)
	default:
		writeAdminError(w, http.StatusNotFound, "管理接口不存在")
	}
}

func (a *application) validAdminOrigin(origin string, access requestAccess) bool {
	if access == requestAccessLocal {
		return validLocalOrigin(origin)
	}
	a.mu.Lock()
	publicBaseURL := a.cfg.PublicBaseURL
	a.mu.Unlock()
	return validPublicOrigin(origin, publicBaseURL)
}

func (a *application) handleAdminLogin(w http.ResponseWriter, r *http.Request, access requestAccess) {
	key := loginClientKey(r)
	allowed, retryAfter := a.loginAllowed(key)
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int(retryAfter.Seconds()))))
		writeAdminError(w, http.StatusTooManyRequests, "登录尝试过多，请稍后重试")
		return
	}
	if !a.acquireLoginGate() {
		writeAdminError(w, http.StatusServiceUnavailable, "登录服务正忙，请稍后重试")
		return
	}
	defer a.releaseLoginGate()
	var request adminLoginRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.configMu.Lock()
	defer a.configMu.Unlock()
	if !a.verifyPublicAdminPassword(request.Password) {
		a.recordLoginFailure(key)
		writeAdminError(w, http.StatusUnauthorized, "管理员密码错误")
		return
	}
	a.clearLoginFailures(key)
	if err := a.createAdminSession(w, access == requestAccessPublic); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "无法创建管理会话")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func constantEqual(left, right string) bool {
	return left != "" && right != "" && len(left) == len(right) &&
		subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func (a *application) handleBootstrap(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"csrfToken": a.csrfToken,
		"config":    a.publicConfig(),
		"status":    a.status(),
		"version":   applicationVersion,
	})
}

func (a *application) publicConfig() publicConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	accounts := make([]publicAccount, 0, len(a.cfg.Accounts))
	for _, account := range a.cfg.Accounts {
		accounts = append(accounts, publicAccount{
			ID: account.ID, Name: account.Name, AuthType: normalizeAccountAuthType(account.AuthType),
			Provider: effectiveAccountProvider(account),
			BaseURL: account.BaseURL, CodexAuthenticated: codexAuthenticated(account),
			CodexEmail: account.CodexEmail, CodexPlanType: account.CodexPlanType,
			CodexExpiresAt: account.CodexExpiresAt, DisableCodexCredits: account.DisableCodexCredits,
			QuotaResetPeriod: account.QuotaResetPeriod,
			QuotaResetTimezone: account.QuotaResetTimezone, QuotaResetEvery: account.QuotaResetEvery,
			QuotaResetUnit: account.QuotaResetUnit, QuotaResetAnchorAt: account.QuotaResetAnchorAt,
			NewAPIAuthMode: normalizeNewAPIAuthMode(account.NewAPIAuthMode), NewAPIUsername: account.NewAPIUsername,
			NewAPIUserID: account.NewAPIUserID, NewAPISecretConfigured: account.NewAPISecret != "",
			Enabled: account.Enabled, Revision: account.Revision, Verified: account.Verified,
			BlockedReason: account.BlockedReason, KeyConfigured: account.APIKey != "",
		})
	}
	return publicConfig{
		Version: configVersion, Accounts: accounts, Strategy: a.cfg.Strategy,
		AllowInsecureHTTP: a.cfg.AllowInsecureHTTP, GatewayTokenCount: len(a.cfg.GatewayTokens),
		AllowPublicAccess: a.cfg.AllowPublicAccess, PublicBaseURL: a.cfg.PublicBaseURL,
		AllowPublicAdmin: a.cfg.AllowPublicAdmin, AdminPasswordConfigured: a.cfg.AdminPasswordHash != "",
		LastSwitchReason: a.cfg.LastSwitchReason, LastSwitchAt: a.cfg.LastSwitchAt,
	}
}

func (a *application) codexSnippet() string {
	a.mu.Lock()
	if len(a.cfg.GatewayTokens) == 0 {
		a.mu.Unlock()
		return ""
	}
	token := a.cfg.GatewayTokens[0].Token
	a.mu.Unlock()
	return codexSnippetFor("http://127.0.0.1:4000", token)
}

func codexSnippetFor(baseURL, token string) string {
	return "model_provider = \"quota_router\"\n\n" +
		"[model_providers.quota_router]\n" +
		"name = \"quota-router\"\n" +
		"base_url = " + strconv.Quote(strings.TrimRight(baseURL, "/")+"/v1") + "\n" +
		"wire_api = \"responses\"\n" +
		"experimental_bearer_token = " + strconv.Quote(token) + "\n" +
		"requires_openai_auth = true\n"
}

func (a *application) gatewayTokenSnippet(id, target string) (string, error) {
	id = strings.TrimSpace(id)
	target = strings.TrimSpace(strings.ToLower(target))
	a.mu.Lock()
	defer a.mu.Unlock()
	var token string
	for _, item := range a.cfg.GatewayTokens {
		if item.ID == id {
			token = item.Token
			break
		}
	}
	if token == "" {
		return "", errors.New("网关 Token 不存在")
	}
	if target == "token" {
		return token, nil
	}
	baseURL := "http://127.0.0.1:4000"
	if target == "public" {
		if !a.cfg.AllowPublicAccess || a.cfg.PublicBaseURL == "" {
			return "", errors.New("尚未启用公网访问")
		}
		baseURL = a.cfg.PublicBaseURL
	} else if target != "" && target != "local" {
		return "", errors.New("配置目标无效")
	}
	return codexSnippetFor(baseURL, token), nil
}

func (a *application) handleGatewayTokens(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	a.mu.Lock()
	matched := make([]publicGatewayToken, 0, min(limit, len(a.cfg.GatewayTokens)))
	total := 0
	for _, token := range a.cfg.GatewayTokens {
		if query != "" && !strings.Contains(strings.ToLower(token.Name), query) {
			continue
		}
		if total >= offset && len(matched) < limit {
			matched = append(matched, publicGatewayToken{ID: token.ID, Name: token.Name, CreatedAt: token.CreatedAt})
		}
		total++
	}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"tokens": matched, "total": total, "offset": offset, "limit": limit,
	})
}

func (a *application) handleGatewayTokenSnippet(w http.ResponseWriter, r *http.Request) {
	var request gatewayTokenSnippetRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	snippet, err := a.gatewayTokenSnippet(request.ID, request.Target)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"snippet": snippet})
}

func (a *application) handleSave(w http.ResponseWriter, r *http.Request, access requestAccess) {
	var request saveRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.AdminPassword != "" && request.ClearAdminPassword {
		writeAdminError(w, http.StatusBadRequest, "不能同时设置并清除管理员密码")
		return
	}
	adminPasswordHash := ""
	if request.AdminPassword != "" {
		if err := validateAdminPasswordInput(request.AdminPassword); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
		var err error
		adminPasswordHash, err = hashAdminPassword(request.AdminPassword)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, "无法安全保存管理员密码")
			return
		}
	}
	tokenOperations := 0
	for _, active := range []bool{
		request.RotateGatewayToken, strings.TrimSpace(request.CreateGatewayTokenName) != "",
		strings.TrimSpace(request.DeleteGatewayTokenID) != "", strings.TrimSpace(request.RotateGatewayTokenID) != "",
	} {
		if active {
			tokenOperations++
		}
	}
	if tokenOperations > 1 {
		writeAdminError(w, http.StatusBadRequest, "一次请求只能执行一个网关 Token 操作")
		return
	}
	a.configMu.Lock()
	configLocked := true
	defer func() {
		if configLocked {
			a.configMu.Unlock()
		}
	}()
	a.mu.Lock()
	candidate := cloneConfig(a.cfg)
	previousPasswordHash := a.cfg.AdminPasswordHash
	previousAllowPublicAccess := a.cfg.AllowPublicAccess
	previousAllowPublicAdmin := a.cfg.AllowPublicAdmin
	previousPublicBaseURL := a.cfg.PublicBaseURL
	if request.Strategy != nil {
		candidate.Strategy = *request.Strategy
	}
	if request.AllowInsecureHTTP != nil {
		candidate.AllowInsecureHTTP = *request.AllowInsecureHTTP
	}
	if request.AllowPublicAccess != nil {
		candidate.AllowPublicAccess = *request.AllowPublicAccess
	}
	if request.PublicBaseURL != nil {
		candidate.PublicBaseURL = *request.PublicBaseURL
	}
	if request.AllowPublicAdmin != nil {
		candidate.AllowPublicAdmin = *request.AllowPublicAdmin
	}
	if request.ClearAdminPassword {
		candidate.AdminPasswordHash = ""
	} else if adminPasswordHash != "" {
		candidate.AdminPasswordHash = adminPasswordHash
	}
	if access == requestAccessPublic && (candidate.AllowPublicAccess != a.cfg.AllowPublicAccess ||
		candidate.PublicBaseURL != a.cfg.PublicBaseURL || candidate.AllowPublicAdmin != a.cfg.AllowPublicAdmin) {
		a.mu.Unlock()
		writeAdminError(w, http.StatusForbidden, "公网访问设置只能从本机管理页面修改")
		return
	}
	issuedGatewayToken := ""
	issuedGatewayTokenID := ""
	tokenChanged := false
	if strings.TrimSpace(request.CreateGatewayTokenName) != "" {
		if len(candidate.GatewayTokens) >= maxGatewayTokens {
			a.mu.Unlock()
			writeAdminError(w, http.StatusBadRequest, fmt.Sprintf("网关 Token 数量不能超过 %d", maxGatewayTokens))
			return
		}
		token, err := newGatewayTokenConfig(request.CreateGatewayTokenName, a.now())
		if err != nil {
			a.mu.Unlock()
			writeAdminError(w, http.StatusInternalServerError, "无法生成新的网关 Token")
			return
		}
		candidate.GatewayTokens = append(candidate.GatewayTokens, token)
		issuedGatewayToken = token.Token
		issuedGatewayTokenID = token.ID
		tokenChanged = true
	} else if deleteID := strings.TrimSpace(request.DeleteGatewayTokenID); deleteID != "" {
		found := false
		next := make([]gatewayTokenConfig, 0, len(candidate.GatewayTokens))
		for _, token := range candidate.GatewayTokens {
			if token.ID == deleteID {
				found = true
				continue
			}
			next = append(next, token)
		}
		if !found {
			a.mu.Unlock()
			writeAdminError(w, http.StatusNotFound, "网关 Token 不存在")
			return
		}
		candidate.GatewayTokens = next
		tokenChanged = true
	} else if rotateID := strings.TrimSpace(request.RotateGatewayTokenID); rotateID != "" {
		found := false
		for index := range candidate.GatewayTokens {
			if candidate.GatewayTokens[index].ID != rotateID {
				continue
			}
			replacement, err := randomToken()
			if err != nil {
				a.mu.Unlock()
				writeAdminError(w, http.StatusInternalServerError, "无法生成新的网关 Token")
				return
			}
			candidate.GatewayTokens[index].Token = replacement
			issuedGatewayToken = replacement
			issuedGatewayTokenID = rotateID
			found = true
			break
		}
		if !found {
			a.mu.Unlock()
			writeAdminError(w, http.StatusNotFound, "网关 Token 不存在")
			return
		}
		tokenChanged = true
	} else if request.RotateGatewayToken {
		if len(candidate.GatewayTokens) == 0 {
			token, err := newGatewayTokenConfig("默认 Token", a.now())
			if err != nil {
				a.mu.Unlock()
				writeAdminError(w, http.StatusInternalServerError, "无法生成新的网关 Token")
				return
			}
			candidate.GatewayTokens = append(candidate.GatewayTokens, token)
			issuedGatewayToken = token.Token
			issuedGatewayTokenID = token.ID
		} else {
			replacement, err := randomToken()
			if err != nil {
				a.mu.Unlock()
				writeAdminError(w, http.StatusInternalServerError, "无法生成新的网关 Token")
				return
			}
			candidate.GatewayTokens[0].Token = replacement
			issuedGatewayToken = replacement
			issuedGatewayTokenID = candidate.GatewayTokens[0].ID
		}
		tokenChanged = true
	}
	changedAccounts := make(map[string]bool)
	if request.Accounts != nil {
		existing := make(map[string]accountConfig, len(a.cfg.Accounts))
		for _, account := range a.cfg.Accounts {
			existing[account.ID] = account
		}
		seen := make(map[string]bool, len(*request.Accounts))
		next := make([]accountConfig, 0, len(*request.Accounts))
		for index, input := range *request.Accounts {
			if err := normalizeAccountInput(&input); err != nil {
				a.mu.Unlock()
				writeAdminError(w, http.StatusBadRequest, err.Error())
				return
			}
			if input.ID == "" {
				if normalizeAccountAuthType(input.AuthType) == accountAuthAPIKey &&
					(input.ClearAPIKey || input.APIKey == "") {
					a.mu.Unlock()
					writeAdminError(w, http.StatusBadRequest, "新账号必须填写 API Key")
					return
				}
				id, err := randomAccountID()
				if err != nil {
					a.mu.Unlock()
					writeAdminError(w, http.StatusInternalServerError, "无法生成账号 ID")
					return
				}
				for seen[id] || existing[id].ID != "" {
					id, err = randomAccountID()
					if err != nil {
						a.mu.Unlock()
						writeAdminError(w, http.StatusInternalServerError, "无法生成账号 ID")
						return
					}
				}
				enabled := true
				if input.Enabled != nil {
					enabled = *input.Enabled
				}
				account, err := mergeAccountInput(accountConfig{ID: id, Enabled: enabled, Revision: 1}, input, true)
				if err != nil {
					a.mu.Unlock()
					writeAdminError(w, http.StatusBadRequest, err.Error())
					return
				}
				if account.Name == "" {
					account.Name = fmt.Sprintf("账号 %d", index+1)
				}
				seen[id] = true
				changedAccounts[id] = true
				next = append(next, account)
				continue
			}
			if seen[input.ID] {
				a.mu.Unlock()
				writeAdminError(w, http.StatusBadRequest, "账号 ID 重复")
				return
			}
			old, ok := existing[input.ID]
			if !ok {
				a.mu.Unlock()
				writeAdminError(w, http.StatusBadRequest, "不能使用未知的账号 ID")
				return
			}
			seen[input.ID] = true
			account, err := mergeAccountInput(old, input, true)
			if err != nil {
				a.mu.Unlock()
				writeAdminError(w, http.StatusBadRequest, err.Error())
				return
			}
			if account.Name == "" {
				account.Name = old.Name
			}
			proxyChanged := proxyAuthChanged(account, old)
			if proxyChanged || newAPIAuthChanged(account, old) || quotaResetChanged(account, old) {
				account.Revision = old.Revision + 1
				changedAccounts[account.ID] = true
			}
			if proxyChanged {
				account.Verified = false
				account.BlockedReason = ""
			}
			next = append(next, account)
		}
		for id := range existing {
			if !seen[id] {
				changedAccounts[id] = true
			}
		}
		candidate.Accounts = next
	}
	if err := normalizeAndValidateConfig(&candidate); err != nil {
		a.mu.Unlock()
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	a.mu.Unlock()
	if err := a.writeConfig(candidate); err != nil {
		a.configMu.Unlock()
		configLocked = false
		a.logEvent(r.Context(), slog.LevelError, "config_save_failed",
			"error_kind", logErrorKind(err), "accounts", len(candidate.Accounts),
			"changed_accounts", len(changedAccounts), "strategy", candidate.Strategy,
			"gateway_token_rotated", tokenChanged)
		writeAdminError(w, http.StatusInternalServerError, "配置保存失败")
		return
	}
	a.mu.Lock()
	a.cfg = candidate
	a.pruneRequestStatsLocked()
	a.rebuildGatewayTokenIndexLocked()
	publicSecurityChanged := previousPasswordHash != candidate.AdminPasswordHash ||
		previousAllowPublicAccess != candidate.AllowPublicAccess ||
		previousAllowPublicAdmin != candidate.AllowPublicAdmin ||
		previousPublicBaseURL != candidate.PublicBaseURL
	if publicSecurityChanged || !candidate.AllowPublicAccess {
		a.adminSessions = make(map[[32]byte]time.Time)
	}
	nextRuntime := make(map[string]*accountRuntime, len(candidate.Accounts))
	for _, account := range candidate.Accounts {
		if runtime := a.runtime[account.ID]; runtime != nil && !changedAccounts[account.ID] && runtime.Revision == account.Revision {
			nextRuntime[account.ID] = runtime
		} else {
			nextRuntime[account.ID] = &accountRuntime{Revision: account.Revision}
		}
	}
	a.runtime = nextRuntime
	if len(candidate.Accounts) == 0 {
		a.roundRobinCursor = 0
	} else {
		a.roundRobinCursor %= len(candidate.Accounts)
	}
	if a.lastRoutedAccountID != "" {
		lastRoutedFound := false
		for _, account := range candidate.Accounts {
			if account.ID == a.lastRoutedAccountID {
				a.lastRoutedAccountName = account.Name
				lastRoutedFound = true
				break
			}
		}
		if !lastRoutedFound {
			a.lastRoutedAccountID = ""
			a.lastRoutedAccountName = ""
		}
	}
	a.mu.Unlock()
	if a.codexUsage != nil {
		for id := range changedAccounts {
			a.codexUsage.Invalidate(id)
		}
	}
	var sessionErr error
	if candidate.AllowPublicAccess && publicSecurityChanged {
		sessionErr = a.createAdminSession(w, access == requestAccessPublic)
	}
	a.configMu.Unlock()
	configLocked = false
	if sessionErr != nil {
		writeAdminError(w, http.StatusInternalServerError, "配置已保存，但无法刷新管理会话，请重新登录")
		return
	}
	a.logEvent(r.Context(), slog.LevelInfo, "config_saved",
		"accounts", len(candidate.Accounts), "changed_accounts", len(changedAccounts),
		"strategy", candidate.Strategy, "gateway_token_rotated", tokenChanged)
	response := map[string]any{"ok": true, "config": a.publicConfig(), "status": a.status()}
	if issuedGatewayToken != "" {
		response["gatewayToken"] = issuedGatewayToken
		response["gatewayTokenId"] = issuedGatewayTokenID
		response["codexSnippet"] = codexSnippetFor("http://127.0.0.1:4000", issuedGatewayToken)
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *application) handleModels(w http.ResponseWriter, r *http.Request) {
	var request accountProbeRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	candidate, _, _, err := a.prepareAccountCandidate(request, false)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelsProbeTime)
	defer cancel()
	models, statusCode, err := a.probeModelsAccount(ctx, candidate)
	if err != nil {
		if statusCode != 0 && (statusCode < 200 || statusCode >= 300) {
			writeAdminError(w, http.StatusBadGateway, fmt.Sprintf("上游模型接口返回 HTTP %d", statusCode))
		} else {
			writeAdminError(w, http.StatusBadGateway, "上游模型列表响应无效")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": models})
}

func (a *application) probeModelsAccount(ctx context.Context, account accountConfig) ([]string, int, error) {
	if normalizeAccountAuthType(account.AuthType) == accountAuthCodexOAuth {
		return nil, 0, errors.New("Codex OAuth 账号不提供模型列表接口")
	}
	target, err := joinUpstreamURL(account.BaseURL, "/v1/models", "")
	if err != nil {
		return nil, 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+account.APIKey)
	request.Header.Set("Accept", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.StatusCode, errors.New("models request rejected")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxModelsBody+1))
	if err != nil || len(body) > maxModelsBody {
		return nil, response.StatusCode, errors.New("invalid models response")
	}
	models, err := parseUpstreamModels(body, account.APIKey, account.NewAPISecret)
	return models, response.StatusCode, err
}

func parseUpstreamModels(body []byte, secrets ...string) ([]string, error) {
	var payload struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	data := bytes.TrimSpace(payload.Data)
	if len(data) == 0 || data[0] != '[' {
		return nil, errors.New("models data is not an array")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	models := make([]string, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		var id string
		if err := json.Unmarshal(entry, &id); err != nil {
			var model struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(entry, &model); err != nil {
				continue
			}
			id = model.ID
		}
		id = strings.TrimSpace(id)
		sensitive := false
		for _, secret := range secrets {
			if secret != "" && (id == secret || len(secret) >= 8 && strings.Contains(id, secret)) {
				sensitive = true
				break
			}
		}
		if id == "" || sensitive || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, id)
	}
	return models, nil
}

func (a *application) prepareAccountCandidate(request accountProbeRequest, includeBalanceAuth bool) (accountConfig, accountConfig, bool, error) {
	request.AccountID = strings.TrimSpace(request.AccountID)
	if request.AccountID == "" && request.Candidate != nil {
		request.AccountID = strings.TrimSpace(request.Candidate.ID)
	}
	a.mu.Lock()
	allowInsecureHTTP := a.cfg.AllowInsecureHTTP
	var saved accountConfig
	savedFound := false
	for _, account := range a.cfg.Accounts {
		if account.ID == request.AccountID {
			saved = account
			savedFound = true
			break
		}
	}
	a.mu.Unlock()
	if request.AllowInsecureHTTP != nil {
		allowInsecureHTTP = *request.AllowInsecureHTTP
	}
	candidate := saved
	if request.Candidate != nil {
		input := *request.Candidate
		var err error
		candidate, err = mergeAccountInput(saved, input, includeBalanceAuth)
		if err != nil {
			return accountConfig{}, accountConfig{}, false, err
		}
	}
	if candidate.Name == "" {
		candidate.Name = "待查询账号"
	}
	validation := storedConfig{
		Version: configVersion, Accounts: []accountConfig{candidate}, Strategy: strategyPriority,
		AllowInsecureHTTP: allowInsecureHTTP,
	}
	if candidate.ID == "" {
		candidate.ID = "candidate"
		validation.Accounts[0].ID = candidate.ID
	}
	if validation.Accounts[0].Revision < 1 {
		validation.Accounts[0].Revision = 1
	}
	if err := normalizeAndValidateConfig(&validation); err != nil {
		return accountConfig{}, accountConfig{}, false, err
	}
	candidate = validation.Accounts[0]
	if !accountConfigured(candidate) {
		return accountConfig{}, accountConfig{}, false, errors.New("该账号尚未完整配置")
	}
	return candidate, saved, savedFound, nil
}

func (a *application) handleBalancesRefresh(w http.ResponseWriter, r *http.Request) {
	var request balanceRefreshRequest
	if r.ContentLength != 0 {
		if err := decodeJSONBody(w, r, &request); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	var ids map[string]bool
	for _, id := range request.AccountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if ids == nil {
			ids = make(map[string]bool)
		}
		ids[id] = true
	}
	maxAge := time.Duration(0)
	if r.URL.Query().Get("automatic") == "1" {
		maxAge = balanceAutoTTL
	}
	reports := a.refreshBalances(r.Context(), ids, maxAge)
	counts := countBalanceReports(reports)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     counts.Failed == 0 && counts.Canceled == 0,
		"counts": counts, "reports": reports, "status": a.status(),
	})
}

func countBalanceReports(reports []balanceRefreshReport) balanceRefreshCounts {
	counts := balanceRefreshCounts{Total: len(reports)}
	for _, report := range reports {
		switch {
		case report.Balance.RefreshStatus == balanceRefreshCanceled:
			counts.Canceled++
		case report.Balance.Status == balanceRefreshUnsupported ||
			report.Balance.RefreshStatus == balanceRefreshUnsupported:
			counts.Unsupported++
		case report.Balance.Status != "ok":
			counts.Failed++
		case report.Balance.RefreshStatus == balanceRefreshPartial:
			counts.Partial++
		case report.Balance.RefreshStatus == balanceRefreshOK || report.Balance.RefreshStatus == "":
			counts.Success++
		default:
			counts.Failed++
		}
	}
	return counts
}

func (a *application) handleBalanceTest(w http.ResponseWriter, r *http.Request) {
	var request accountProbeRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	candidate, saved, savedFound, err := a.prepareAccountCandidate(request, true)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	accountRef := logReference(candidate.ID)
	logStarted := time.Now()
	ctx, cancel := a.newBalanceRefreshContext(r.Context())
	if !a.acquireBalanceRefresh(ctx) {
		balance := canceledBalanceAttempt(a.now())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && r.Context().Err() == nil {
			balance = timeoutBalanceAttempt(balanceSnapshot{}, a.now())
		}
		cancel()
		public := publicBalanceAt(balance, a.now())
		a.logEvent(r.Context(), slog.LevelWarn, "balance_test_finished",
			"account_ref", accountRef, "status", public.Status,
			"refresh_status", public.RefreshStatus, "error_stage", public.ErrorStage,
			"error_code", public.ErrorCode, "retryable", public.Retryable,
			"duration_ms", time.Since(logStarted).Milliseconds())
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": false, "balance": public,
			"report": balanceRefreshReport{AccountID: candidate.ID, Balance: public},
		})
		return
	}
	defer a.releaseBalanceRefresh()
	balance := a.probeBalance(ctx, candidate)
	ctxErr := ctx.Err()
	cancel()
	if errors.Is(ctxErr, context.DeadlineExceeded) && r.Context().Err() == nil {
		balance = timeoutBalanceAttempt(balance, a.now())
	}
	if savedFound && sameAccountRevision(saved, candidate) && r.Context().Err() == nil {
		if merged, applied := a.applyBalance(saved, balance); applied {
			balance = merged
		}
	}
	public := publicBalanceAt(balance, a.now())
	report := balanceRefreshReport{AccountID: candidate.ID, Balance: public}
	level := slog.LevelWarn
	if public.Status == "ok" {
		level = slog.LevelInfo
	}
	a.logEvent(r.Context(), level, "balance_test_finished",
		"account_ref", accountRef, "status", public.Status,
		"refresh_status", public.RefreshStatus, "error_stage", public.ErrorStage,
		"error_code", public.ErrorCode, "retryable", public.Retryable,
		"duration_ms", time.Since(logStarted).Milliseconds())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": public.Status == "ok", "balance": public, "report": report,
	})
}

func (a *application) handleCodexOAuthStart(
	w http.ResponseWriter,
	r *http.Request,
	access requestAccess,
	sessionRequired bool,
) {
	if access != requestAccessLocal {
		writeAdminError(w, http.StatusForbidden, "Codex OAuth 浏览器登录只能从本机管理页面启动")
		return
	}
	var request codexOAuthStartRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.AccountID = strings.TrimSpace(request.AccountID)
	account, ok := a.savedAccount(request.AccountID)
	if !ok {
		writeAdminError(w, http.StatusNotFound, "账号不存在，请先保存账号")
		return
	}
	if normalizeAccountAuthType(account.AuthType) != accountAuthCodexOAuth {
		writeAdminError(w, http.StatusBadRequest, "该账号不是 Codex OAuth 登录类型")
		return
	}
	owner, err := a.codexAdminSessionID(r, sessionRequired)
	if err != nil {
		writeAdminError(w, http.StatusForbidden, "管理员会话无效")
		return
	}
	if a.codexAuth == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "Codex OAuth 登录服务不可用")
		return
	}
	login, err := a.codexAuth.startBrowserLogin(r.Context(), owner, account.ID, account.Revision)
	if err != nil {
		switch {
		case errors.Is(err, errCodexOAuthSessionLimit):
			writeAdminError(w, http.StatusConflict, "已有 Codex OAuth 登录正在进行，请先完成当前登录")
		case errors.Is(err, errCodexOAuthCallbackUnavailable):
			writeAdminError(w, http.StatusConflict, "本机端口 1455 被占用，无法接收 OpenAI 登录回调")
		default:
			writeAdminError(w, http.StatusBadGateway, "无法启动 Codex OAuth 浏览器登录")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accountId": account.ID, "sessionId": login.SessionID,
		"authorizationUrl": login.AuthorizationURL,
		"expiresAt": login.ExpiresAt, "pollIntervalSeconds": login.PollIntervalSeconds,
	})
}

func (a *application) handleCodexOAuthStatus(w http.ResponseWriter, r *http.Request, sessionRequired bool) {
	var request codexOAuthStatusRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.SessionID = strings.TrimSpace(request.SessionID)
	owner, err := a.codexAdminSessionID(r, sessionRequired)
	if err != nil {
		writeAdminError(w, http.StatusForbidden, "管理员会话无效")
		return
	}
	if a.codexAuth == nil {
		writeAdminError(w, http.StatusServiceUnavailable, "Codex OAuth 登录服务不可用")
		return
	}
	result, err := a.codexAuth.pollBrowserLogin(owner, request.SessionID)
	if err != nil {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, errCodexOAuthSessionNotFound):
			status = http.StatusNotFound
		case errors.Is(err, errCodexOAuthSessionExpired):
			status = http.StatusGone
		}
		writeAdminError(w, status, "Codex OAuth 登录会话已失效")
		return
	}
	if result.Status == codexBrowserPollFailed {
		writeAdminError(w, http.StatusBadRequest, "OpenAI 登录失败，请重新登录")
		return
	}
	if result.Status != codexBrowserPollComplete || result.Credentials == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "oauthStatus": codexBrowserPollPending,
			"retryAfterSeconds": result.RetryAfterSeconds, "expiresAt": result.ExpiresAt,
		})
		return
	}
	account, ok := a.savedAccount(result.AccountID)
	if !ok || account.Revision != result.AccountRevision ||
		normalizeAccountAuthType(account.AuthType) != accountAuthCodexOAuth {
		writeAdminError(w, http.StatusConflict, "登录期间账号配置已改变，请重新登录")
		return
	}
	updated, err := a.persistCodexCredentials(account, *result.Credentials, true)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errCodexAccountChanged) || errors.Is(err, errCodexAccountNotFound) {
			status = http.StatusConflict
		}
		writeAdminError(w, status, "Codex OAuth 已授权，但账号配置保存失败，请重新登录")
		return
	}
	a.refreshCodexUsageAccount(r.Context(), updated)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "oauthStatus": codexBrowserPollComplete, "accountId": updated.ID,
		"email": updated.CodexEmail, "planType": updated.CodexPlanType,
		"config": a.publicConfig(), "status": a.status(),
	})
}

func (a *application) handleCodexLogout(w http.ResponseWriter, r *http.Request) {
	var request codexOAuthStartRequest
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.AccountID = strings.TrimSpace(request.AccountID)
	account, ok := a.savedAccount(request.AccountID)
	if !ok {
		writeAdminError(w, http.StatusNotFound, "账号不存在")
		return
	}
	if normalizeAccountAuthType(account.AuthType) != accountAuthCodexOAuth {
		writeAdminError(w, http.StatusBadRequest, "该账号不是 Codex OAuth 登录类型")
		return
	}
	if err := a.clearCodexCredentials(account); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errCodexAccountChanged) || errors.Is(err, errCodexAccountNotFound) {
			status = http.StatusConflict
		}
		writeAdminError(w, status, "无法退出 Codex OAuth 账号")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "config": a.publicConfig(), "status": a.status(),
	})
}

func (a *application) handleCodexUsageRefresh(w http.ResponseWriter, r *http.Request) {
	var request codexUsageRefreshRequest
	if r.ContentLength != 0 {
		if err := decodeJSONBody(w, r, &request); err != nil {
			writeAdminError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	var ids map[string]bool
	for _, id := range request.AccountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if ids == nil {
			ids = make(map[string]bool)
		}
		ids[id] = true
	}
	reports := a.refreshCodexUsageAccounts(r.Context(), ids)
	ok := true
	for _, report := range reports {
		if !report.OK {
			ok = false
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": ok, "reports": reports, "status": a.status(),
	})
}

func (a *application) savedAccount(id string) (accountConfig, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	index := a.accountIndexLocked(strings.TrimSpace(id))
	if index < 0 {
		return accountConfig{}, false
	}
	return a.cfg.Accounts[index], true
}

func codexCredentialsFromAccount(account accountConfig) (codexOAuthCredentials, error) {
	if !codexAuthenticated(account) {
		return codexOAuthCredentials{}, errors.New("Codex OAuth 账号尚未登录")
	}
	expiresAt := time.Time{}
	if account.CodexExpiresAt != "" {
		var err error
		expiresAt, err = time.Parse(time.RFC3339, account.CodexExpiresAt)
		if err != nil {
			return codexOAuthCredentials{}, errors.New("Codex OAuth 过期时间无效")
		}
	}
	return codexOAuthCredentials{
		AccessToken: account.CodexAccessToken, RefreshToken: account.CodexRefreshToken,
		IDToken: account.CodexIDToken, AccountID: account.CodexAccountID,
		Email: account.CodexEmail, ExpiresAt: expiresAt, PlanType: account.CodexPlanType,
	}, nil
}

func (a *application) ensureCodexCredentials(ctx context.Context, account accountConfig) (accountConfig, codexOAuthCredentials, error) {
	credentials, err := codexCredentialsFromAccount(account)
	if err != nil {
		return accountConfig{}, codexOAuthCredentials{}, err
	}
	if credentials.ExpiresAt.After(a.now().Add(2 * time.Minute)) {
		return account, credentials, nil
	}
	if a.codexAuth == nil {
		return accountConfig{}, codexOAuthCredentials{}, errors.New("Codex OAuth 登录服务不可用")
	}
	refreshed, err := a.codexAuth.refreshTokens(ctx, credentials)
	if err != nil {
		return accountConfig{}, codexOAuthCredentials{}, err
	}
	updated, err := a.persistCodexCredentials(account, refreshed, false)
	if err != nil {
		return accountConfig{}, codexOAuthCredentials{}, err
	}
	stored, err := codexCredentialsFromAccount(updated)
	return updated, stored, err
}

func (a *application) persistCodexCredentials(
	expected accountConfig,
	credentials codexOAuthCredentials,
	login bool,
) (accountConfig, error) {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	a.mu.Lock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 {
		a.mu.Unlock()
		return accountConfig{}, errCodexAccountNotFound
	}
	current := a.cfg.Accounts[index]
	if current.Revision != expected.Revision ||
		normalizeAccountAuthType(current.AuthType) != accountAuthCodexOAuth {
		a.mu.Unlock()
		return accountConfig{}, errCodexAccountChanged
	}
	if !login && current.CodexRefreshToken != expected.CodexRefreshToken {
		a.mu.Unlock()
		return current, nil
	}
	candidate := cloneConfig(a.cfg)
	updated := &candidate.Accounts[index]
	updated.CodexAccessToken = strings.TrimSpace(credentials.AccessToken)
	updated.CodexRefreshToken = strings.TrimSpace(credentials.RefreshToken)
	updated.CodexIDToken = strings.TrimSpace(credentials.IDToken)
	updated.CodexAccountID = strings.TrimSpace(credentials.AccountID)
	updated.CodexEmail = strings.TrimSpace(credentials.Email)
	updated.CodexPlanType = strings.TrimSpace(credentials.PlanType)
	if credentials.ExpiresAt.IsZero() {
		updated.CodexExpiresAt = ""
	} else {
		updated.CodexExpiresAt = credentials.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if login {
		updated.Revision++
		updated.Verified = false
		updated.BlockedReason = ""
	}
	if err := normalizeAndValidateConfig(&candidate); err != nil {
		a.mu.Unlock()
		return accountConfig{}, err
	}
	a.mu.Unlock()
	if err := a.writeConfig(candidate); err != nil {
		return accountConfig{}, err
	}
	a.mu.Lock()
	a.cfg = candidate
	saved := candidate.Accounts[index]
	if login {
		a.runtime[saved.ID] = &accountRuntime{Revision: saved.Revision}
	}
	a.mu.Unlock()
	if a.codexUsage != nil {
		a.codexUsage.Invalidate(saved.ID)
	}
	return saved, nil
}

func (a *application) clearCodexCredentials(expected accountConfig) error {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	a.mu.Lock()
	index := a.accountIndexLocked(expected.ID)
	if index < 0 {
		a.mu.Unlock()
		return errCodexAccountNotFound
	}
	current := a.cfg.Accounts[index]
	if current.Revision != expected.Revision ||
		normalizeAccountAuthType(current.AuthType) != accountAuthCodexOAuth {
		a.mu.Unlock()
		return errCodexAccountChanged
	}
	candidate := cloneConfig(a.cfg)
	clearCodexAuth(&candidate.Accounts[index])
	candidate.Accounts[index].Revision++
	candidate.Accounts[index].Verified = false
	candidate.Accounts[index].BlockedReason = ""
	a.mu.Unlock()
	if err := a.writeConfig(candidate); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg = candidate
	a.runtime[expected.ID] = &accountRuntime{Revision: candidate.Accounts[index].Revision}
	a.mu.Unlock()
	if a.codexUsage != nil {
		a.codexUsage.Invalidate(expected.ID)
	}
	return nil
}

func (a *application) refreshCodexUsageAccount(ctx context.Context, account accountConfig) codexUsageRefreshReport {
	report := codexUsageRefreshReport{AccountID: account.ID}
	if a.codexUsage == nil {
		report.Error = "usage_unavailable"
		return report
	}
	updated, credentials, err := a.ensureCodexCredentials(ctx, account)
	if err != nil {
		report.Error = "authentication_failed"
		return report
	}
	snapshot, err := a.codexUsage.Refresh(ctx, updated.ID, credentials.AccessToken, credentials.AccountID)
	if err != nil {
		report.Error = "refresh_failed"
		return report
	}
	a.mu.Lock()
	index := a.accountIndexLocked(updated.ID)
	if index >= 0 && sameAccountRevision(a.cfg.Accounts[index], updated) {
		runtime := a.runtimeForLocked(a.cfg.Accounts[index])
		runtime.CodexUsage = snapshot
		now := a.now()
		current := a.cfg.Accounts[index]
		if snapshot.BlocksRouting(current.DisableCodexCredits) {
			until := snapshot.CooldownUntil(now)
			if !until.After(now) {
				until = now.Add(accountCooldown)
			}
			runtime.CooldownReason = "quota"
			runtime.CooldownUntil = until
		} else if runtime.CooldownReason == "quota" {
			runtime.CooldownReason = ""
			runtime.CooldownUntil = time.Time{}
		}
		a.signalProbeChangedLocked()
	}
	a.mu.Unlock()
	report.OK = true
	report.Usage = &snapshot
	return report
}

func (a *application) refreshCodexUsageAccounts(ctx context.Context, ids map[string]bool) []codexUsageRefreshReport {
	a.mu.Lock()
	accounts := make([]accountConfig, 0, len(a.cfg.Accounts))
	for _, account := range a.cfg.Accounts {
		if normalizeAccountAuthType(account.AuthType) != accountAuthCodexOAuth || !codexAuthenticated(account) {
			continue
		}
		if ids != nil {
			if !ids[account.ID] {
				continue
			}
		} else if !account.Enabled {
			continue
		}
		accounts = append(accounts, account)
	}
	a.mu.Unlock()
	reports := make([]codexUsageRefreshReport, 0, len(accounts))
	for _, account := range accounts {
		if ctx.Err() != nil {
			reports = append(reports, codexUsageRefreshReport{AccountID: account.ID, Error: "canceled"})
			continue
		}
		reports = append(reports, a.refreshCodexUsageAccount(ctx, account))
	}
	return reports
}

func (a *application) handleAccountReset(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.ID = strings.TrimSpace(request.ID)
	a.mu.Lock()
	index := a.accountIndexLocked(request.ID)
	if index < 0 {
		a.mu.Unlock()
		writeAdminError(w, http.StatusNotFound, "账号不存在")
		return
	}
	account := a.cfg.Accounts[index]
	if !account.Enabled || !accountConfigured(account) {
		a.mu.Unlock()
		writeAdminError(w, http.StatusConflict, "账号未启用或配置不完整，不能参与真实请求验证")
		return
	}
	if hardBlockedReason(account.BlockedReason) {
		a.mu.Unlock()
		writeAdminError(w, http.StatusConflict, "账号处于永久限制状态，不能通过真实请求自动恢复")
		return
	}
	runtime := a.runtimeForLocked(account)
	if runtime.ProbeInFlight {
		a.mu.Unlock()
		writeAdminError(w, http.StatusConflict, "该账号已有真实请求正在验证，请稍候")
		return
	}
	if account.BlockedReason == "" && runtime.CooldownReason == "" {
		a.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "pending": false, "message": "账号当前已可参与路由，无需恢复",
			"config": a.publicConfig(), "status": a.status(),
		})
		return
	}
	now := a.now()
	runtime.CooldownUntil = now
	runtime.FailureBarrierAt = now
	runtime.HealthState = accountHealthUnverified
	runtime.HealthCheckedAt = time.Time{}
	a.signalProbeChangedLocked()
	a.mu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "pending": true, "message": "已唤醒账号，下一次匹配的真实请求将进行单次验证；成功后才清除阻止或冷却状态",
		"config": a.publicConfig(), "status": a.status(),
	})
}

func (a *application) handleAccountResume(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.ID = strings.TrimSpace(request.ID)
	a.mu.Lock()
	index := a.accountIndexLocked(request.ID)
	if index < 0 {
		a.mu.Unlock()
		writeAdminError(w, http.StatusNotFound, "账号不存在")
		return
	}
	runtime := a.runtimeForLocked(a.cfg.Accounts[index])
	if runtime.CooldownReason != "upstream_failures" || runtime.UpstreamFailures == 0 {
		a.mu.Unlock()
		writeAdminError(w, http.StatusConflict, "账号当前未因请求失败过多而暂时停用")
		return
	}
	if runtime.ProbeInFlight {
		a.mu.Unlock()
		writeAdminError(w, http.StatusConflict, "该账号已有真实请求正在验证，请稍候")
		return
	}
	now := a.now()
	runtime.CooldownUntil = now
	runtime.FailureBarrierAt = now
	runtime.HealthState = accountHealthUnverified
	runtime.HealthCheckedAt = time.Time{}
	a.signalProbeChangedLocked()
	a.mu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "pending": true, "message": "已唤醒账号，下一次匹配的真实请求将进行单次验证；成功后才清除阻止或冷却状态", "status": a.status(),
	})
}

func (a *application) handleAccountStatisticsClear(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(w, r, &request); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	request.ID = strings.TrimSpace(request.ID)
	a.mu.Lock()
	index := a.accountIndexLocked(request.ID)
	if index < 0 {
		a.mu.Unlock()
		writeAdminError(w, http.StatusNotFound, "账号不存在")
		return
	}
	accountName := a.cfg.Accounts[index].Name
	delete(a.requestStats, request.ID)
	a.markRequestStatsDirtyLocked()
	a.mu.Unlock()
	if err := a.flushRequestStats(); err != nil {
		writeAdminError(w, http.StatusInternalServerError, "请求统计清理失败")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "message": accountName + "的请求统计已清空", "status": a.status(),
	})
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, destination any) error {
	if contentType := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		return fmt.Errorf("Content-Type 必须是 application/json")
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAdminBody))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("JSON 请求无效")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("JSON 请求只能包含一个对象")
	}
	return nil
}

func writeAdminError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message, "message": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
