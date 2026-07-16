package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	adminSessionCookie     = "cqr_admin_session"
	adminSessionLifetime   = 12 * time.Hour
	adminPasswordMinBytes  = 12
	adminPasswordMaxBytes  = 1024
	adminPasswordRounds    = 600000
	maxAdminSessions       = 64
	maxLoginFailures       = 5
	maxGlobalLoginFailures = 50
	maxLoginAttemptEntries = 4096
	loginFailureWindow     = 15 * time.Minute
	loginBlockTime         = 15 * time.Minute
	globalLoginClientKey   = "__global__"
)

type requestAccess int

const (
	requestAccessDenied requestAccess = iota
	requestAccessLocal
	requestAccessPublic
)

type loginAttempt struct {
	Failures     int
	LastFailure time.Time
	BlockedUntil time.Time
}

func hashAdminPassword(password string) (string, error) {
	if len(password) < adminPasswordMinBytes || len(password) > adminPasswordMaxBytes {
		return "", fmt.Errorf("管理员密码长度必须为 %d 到 %d 个字节", adminPasswordMinBytes, adminPasswordMaxBytes)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2SHA256([]byte(password), salt, adminPasswordRounds, sha256.Size)
	return "pbkdf2-sha256$" + strconv.Itoa(adminPasswordRounds) + "$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(key), nil
}

func validAdminPasswordHash(encoded string) bool {
	_, _, _, ok := parseAdminPasswordHash(encoded)
	return ok
}

func verifyAdminPassword(encoded, password string) bool {
	rounds, salt, expected, ok := parseAdminPasswordHash(encoded)
	if !ok || len(password) > adminPasswordMaxBytes {
		return false
	}
	actual := pbkdf2SHA256([]byte(password), salt, rounds, len(expected))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func parseAdminPasswordHash(encoded string) (int, []byte, []byte, bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return 0, nil, nil, false
	}
	rounds, err := strconv.Atoi(parts[1])
	if err != nil || rounds < 100000 || rounds > 2000000 {
		return 0, nil, nil, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return 0, nil, nil, false
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(key) != sha256.Size {
		return 0, nil, nil, false
	}
	return rounds, salt, key, true
}

func pbkdf2SHA256(password, salt []byte, rounds, keyLength int) []byte {
	result := make([]byte, 0, keyLength)
	block := make([]byte, len(salt)+4)
	copy(block, salt)
	for counter := uint32(1); len(result) < keyLength; counter++ {
		binary.BigEndian.PutUint32(block[len(salt):], counter)
		mac := hmac.New(sha256.New, password)
		_, _ = mac.Write(block)
		u := mac.Sum(nil)
		t := append([]byte(nil), u...)
		for iteration := 1; iteration < rounds; iteration++ {
			mac = hmac.New(sha256.New, password)
			_, _ = mac.Write(u)
			u = mac.Sum(nil)
			for index := range t {
				t[index] ^= u[index]
			}
		}
		remaining := keyLength - len(result)
		if remaining < len(t) {
			t = t[:remaining]
		}
		result = append(result, t...)
	}
	return result
}

func gatewayTokenHash(token string) [sha256.Size]byte {
	return sha256.Sum256([]byte(token))
}

func (a *application) rebuildGatewayTokenIndexLocked() {
	index := make(map[[sha256.Size]byte]string, len(a.cfg.GatewayTokens))
	for _, token := range a.cfg.GatewayTokens {
		if token.Token != "" {
			index[gatewayTokenHash(token.Token)] = token.ID
		}
	}
	a.gatewayTokenIndex = index
}

func requestGatewayToken(r *http.Request) string {
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(authorization) >= 7 && strings.EqualFold(authorization[:7], "Bearer ") {
		return strings.TrimSpace(authorization[7:])
	}
	return strings.TrimSpace(r.Header.Get("X-API-Key"))
}

func (a *application) gatewayTokenID(r *http.Request) (string, bool) {
	token := requestGatewayToken(r)
	if token == "" {
		return "", false
	}
	digest := gatewayTokenHash(token)
	a.mu.Lock()
	id, ok := a.gatewayTokenIndex[digest]
	a.mu.Unlock()
	return id, ok
}

func (a *application) classifyRequest(r *http.Request) requestAccess {
	access, _ := a.classifyAdminRequest(r)
	return access
}

func (a *application) classifyAdminRequest(r *http.Request) (requestAccess, bool) {
	if validLocalHost(r.Host) && directLoopbackRequest(r) {
		a.mu.Lock()
		sessionRequired := a.cfg.AllowPublicAccess
		a.mu.Unlock()
		return requestAccessLocal, sessionRequired
	}
	a.mu.Lock()
	allowPublic := a.cfg.AllowPublicAccess
	publicBaseURL := a.cfg.PublicBaseURL
	a.mu.Unlock()
	if allowPublic && validPublicHost(r.Host, publicBaseURL) && validPublicProxyHeaders(r) {
		return requestAccessPublic, true
	}
	return requestAccessDenied, false
}

func directLoopbackRequest(r *http.Request) bool {
	if r.RemoteAddr == "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback() && r.Header.Get("Forwarded") == "" && r.Header.Get("X-Forwarded-For") == ""
}

func validPublicHost(host, publicBaseURL string) bool {
	parsed, err := url.Parse(publicBaseURL)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(canonicalHost(host, "https"), canonicalHost(parsed.Host, parsed.Scheme))
}

func validPublicProxyHeaders(r *http.Request) bool {
	if r == nil {
		return false
	}
	protoValues := r.Header.Values("X-Forwarded-Proto")
	if len(protoValues) != 1 || !strings.EqualFold(strings.TrimSpace(protoValues[0]), "https") {
		return false
	}
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}
	remoteIP := net.ParseIP(strings.Trim(remoteHost, "[]"))
	if remoteIP == nil || !remoteIP.IsLoopback() {
		return false
	}
	forwardedValues := r.Header.Values("X-Forwarded-For")
	if len(forwardedValues) != 1 {
		return false
	}
	forwarded := strings.TrimSpace(forwardedValues[0])
	if forwarded == "" || strings.Contains(forwarded, ",") {
		return false
	}
	return net.ParseIP(strings.Trim(forwarded, "[]")) != nil
}

func validPublicOrigin(origin, publicBaseURL string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	public, err := url.Parse(publicBaseURL)
	if err != nil || public.Scheme != "https" {
		return false
	}
	return strings.EqualFold(canonicalHost(parsed.Host, parsed.Scheme), canonicalHost(public.Host, public.Scheme))
}

func canonicalHost(value, scheme string) string {
	host := strings.TrimSpace(strings.ToLower(value))
	name, port, err := net.SplitHostPort(host)
	if err == nil {
		if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
			return strings.Trim(name, "[]")
		}
		return net.JoinHostPort(strings.Trim(name, "[]"), port)
	}
	return strings.Trim(host, "[]")
}

func (a *application) publicAdminEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.AllowPublicAccess && a.cfg.AllowPublicAdmin && a.cfg.AdminPasswordHash != ""
}

func (a *application) adminSessionValid(r *http.Request) bool {
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	digest := gatewayTokenHash(cookie.Value)
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	expiresAt, ok := a.adminSessions[digest]
	if !ok || !now.Before(expiresAt) {
		delete(a.adminSessions, digest)
		return false
	}
	return true
}

func (a *application) codexAdminSessionID(r *http.Request, sessionRequired bool) (codexAdminSessionID, error) {
	if !sessionRequired {
		return newCodexAdminSessionID(a.csrfToken)
	}
	if !a.adminSessionValid(r) {
		return codexAdminSessionID{}, errCodexOAuthInvalidAdminSession
	}
	cookie, err := r.Cookie(adminSessionCookie)
	if err != nil || cookie.Value == "" {
		return codexAdminSessionID{}, errCodexOAuthInvalidAdminSession
	}
	return newCodexAdminSessionID(cookie.Value)
}

func (a *application) createAdminSession(w http.ResponseWriter, secure bool) error {
	value, err := randomToken()
	if err != nil {
		return err
	}
	now := a.now()
	digest := gatewayTokenHash(value)
	a.mu.Lock()
	for key, expiresAt := range a.adminSessions {
		if !now.Before(expiresAt) {
			delete(a.adminSessions, key)
		}
	}
	if len(a.adminSessions) >= maxAdminSessions {
		var oldestKey [sha256.Size]byte
		oldest := time.Time{}
		for key, expiresAt := range a.adminSessions {
			if oldest.IsZero() || expiresAt.Before(oldest) {
				oldestKey = key
				oldest = expiresAt
			}
		}
		delete(a.adminSessions, oldestKey)
	}
	a.adminSessions[digest] = now.Add(adminSessionLifetime)
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: adminSessionCookie, Value: value, Path: "/", MaxAge: int(adminSessionLifetime.Seconds()),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func (a *application) clearAdminSession(w http.ResponseWriter, r *http.Request, access requestAccess) {
	if cookie, err := r.Cookie(adminSessionCookie); err == nil && cookie.Value != "" {
		digest := gatewayTokenHash(cookie.Value)
		a.mu.Lock()
		delete(a.adminSessions, digest)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: adminSessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: access == requestAccessPublic, SameSite: http.SameSiteStrictMode,
	})
}

func (a *application) loginAllowed(key string) (bool, time.Duration) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	keys := []string{key, globalLoginClientKey}
	for index, attemptKey := range keys {
		if index > 0 && attemptKey == key {
			continue
		}
		attempt := a.loginAttempts[attemptKey]
		if !attempt.BlockedUntil.IsZero() && now.Before(attempt.BlockedUntil) {
			return false, attempt.BlockedUntil.Sub(now)
		}
		if !attempt.LastFailure.IsZero() && now.Sub(attempt.LastFailure) > loginFailureWindow {
			delete(a.loginAttempts, attemptKey)
		}
	}
	return true, 0
}

func (a *application) recordLoginFailure(key string) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for storedKey, stored := range a.loginAttempts {
		if now.Sub(stored.LastFailure) > loginFailureWindow && !now.Before(stored.BlockedUntil) {
			delete(a.loginAttempts, storedKey)
		}
	}
	record := func(attemptKey string, limit int) {
		if _, exists := a.loginAttempts[attemptKey]; !exists && len(a.loginAttempts) >= maxLoginAttemptEntries {
			oldestKey := ""
			oldest := time.Time{}
			for storedKey, stored := range a.loginAttempts {
				if storedKey == globalLoginClientKey {
					continue
				}
				if oldest.IsZero() || stored.LastFailure.Before(oldest) {
					oldestKey = storedKey
					oldest = stored.LastFailure
				}
			}
			if oldestKey != "" {
				delete(a.loginAttempts, oldestKey)
			}
		}
		attempt := a.loginAttempts[attemptKey]
		if attempt.LastFailure.IsZero() || now.Sub(attempt.LastFailure) > loginFailureWindow {
			attempt.Failures = 0
		}
		attempt.Failures++
		attempt.LastFailure = now
		if attempt.Failures >= limit {
			attempt.BlockedUntil = now.Add(loginBlockTime)
		}
		a.loginAttempts[attemptKey] = attempt
	}
	record(key, maxLoginFailures)
	if key != globalLoginClientKey {
		record(globalLoginClientKey, maxGlobalLoginFailures)
	}
}

func (a *application) clearLoginFailures(key string) {
	a.mu.Lock()
	delete(a.loginAttempts, key)
	a.mu.Unlock()
}

func loginClientKey(r *http.Request) string {
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}
	remoteIP := net.ParseIP(strings.Trim(remoteHost, "[]"))
	if remoteIP != nil && remoteIP.IsLoopback() {
		forwardedValues := r.Header.Values("X-Forwarded-For")
		if len(forwardedValues) > 1 {
			return "forwarded_chain"
		}
		forwarded := ""
		if len(forwardedValues) == 1 {
			forwarded = strings.TrimSpace(forwardedValues[0])
		}
		if forwarded != "" {
			if strings.Contains(forwarded, ",") {
				return "forwarded_chain"
			}
			if ip := net.ParseIP(strings.Trim(forwarded, "[]")); ip != nil {
				return ip.String()
			}
			return "forwarded_invalid"
		}
	}
	if remoteIP != nil {
		return remoteIP.String()
	}
	return "unknown"
}

func (a *application) verifyPublicAdminPassword(password string) bool {
	a.mu.Lock()
	hash := a.cfg.AdminPasswordHash
	a.mu.Unlock()
	return hash != "" && verifyAdminPassword(hash, password)
}

func (a *application) acquireLoginGate() bool {
	select {
	case a.loginGate <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *application) releaseLoginGate() {
	select {
	case <-a.loginGate:
	default:
	}
}

func validateAdminPasswordInput(password string) error {
	if len(password) < adminPasswordMinBytes || len(password) > adminPasswordMaxBytes {
		return errors.New("管理员密码至少需要 12 个字节，且不能超过 1024 个字节")
	}
	return nil
}
