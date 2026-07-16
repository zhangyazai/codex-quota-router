package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	logFilename      = "router.log"
	maxLogFileSize   = 10 << 20
	logRotationRetry = time.Minute
)

var errUnsafeLogPath = errors.New("unsafe log path")

func applicationLogPath(goos, temporaryRoot, runtimeRoot, configPath string) string {
	switch goos {
	case "linux":
		if filepath.IsAbs(runtimeRoot) {
			return filepath.Join(runtimeRoot, configDirectory, logFilename)
		}
		directory := configDirectory + "-" + logReference(filepath.Clean(configPath))
		return filepath.Join(temporaryRoot, directory, logFilename)
	case "darwin":
		directory := configDirectory + "-" + logReference(filepath.Clean(configPath))
		return filepath.Join(temporaryRoot, directory, logFilename)
	default:
		return filepath.Join(filepath.Dir(configPath), logFilename)
	}
}

type rotatingLogWriter struct {
	mu                  sync.Mutex
	path                string
	file                *os.File
	size                int64
	maxSize             int64
	closed              bool
	retryAt             time.Time
	suspended           bool
	reopenAfterRotation bool
}

func openApplicationLogger(configPath string) (*slog.Logger, *rotatingLogWriter, error) {
	logPath, err := resolveApplicationLogPath(configPath)
	if err != nil {
		return nil, nil, err
	}
	writer, err := openRotatingLogWriter(logPath, maxLogFileSize)
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return logger, writer, nil
}

func resolveApplicationLogPath(configPath string) (string, error) {
	runtimeRoot := ""
	if runtime.GOOS == "linux" {
		candidate := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR"))
		if filepath.IsAbs(candidate) {
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				runtimeRoot = candidate
			}
		}
	}
	logPath := applicationLogPath(runtime.GOOS, os.TempDir(), runtimeRoot, configPath)
	directory := filepath.Dir(logPath)
	if err := ensureLogDirectory(directory); err != nil {
		if runtime.GOOS != "linux" || runtimeRoot == "" || errors.Is(err, errUnsafeLogPath) {
			return "", err
		}
		logPath = applicationLogPath(runtime.GOOS, os.TempDir(), "", configPath)
		directory = filepath.Dir(logPath)
		if err := ensureLogDirectory(directory); err != nil {
			return "", err
		}
	}
	return logPath, nil
}

func appendStartupFailure(configPath string, startupErr error) {
	logPath, err := resolveApplicationLogPath(configPath)
	if err != nil || validateLogFile(logPath) != nil {
		return
	}
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	if file.Chmod(0o600) != nil {
		return
	}
	info, err := file.Stat()
	if err != nil {
		return
	}
	line := fmt.Sprintf("time=%s level=ERROR msg=service_start_failed stage=listen error_kind=%s\n",
		time.Now().UTC().Format(time.RFC3339Nano), logErrorKind(startupErr))
	if info.Size()+int64(len(line)) > maxLogFileSize {
		return
	}
	_, _ = file.WriteString(line)
}

func ensureLogDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: 日志目录不是普通目录", errUnsafeLogPath)
	}
	return os.Chmod(path, 0o700)
}

func openRotatingLogWriter(path string, maxSize int64) (*rotatingLogWriter, error) {
	if err := validateLogFile(path); err != nil {
		return nil, err
	}
	writer := &rotatingLogWriter{path: path, maxSize: maxSize}
	if err := writer.openLocked(os.O_APPEND); err != nil {
		return nil, err
	}
	return writer, nil
}

func validateLogFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: 日志文件不是普通文件", errUnsafeLogPath)
	}
	return nil
}

func (w *rotatingLogWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, os.ErrClosed
	}
	if w.file == nil {
		if err := w.openLocked(os.O_APPEND); err != nil {
			return 0, err
		}
		if w.reopenAfterRotation {
			w.reopenAfterRotation = false
			w.retryAt = time.Time{}
			w.suspended = false
			_ = w.writeMarkerLocked("log_writes_resumed", "reopen_recovered")
		}
	}
	nextSize := w.size + int64(len(data))
	if w.maxSize > 0 && w.size > 0 && nextSize > w.maxSize {
		now := time.Now()
		if w.retryAt.IsZero() || !now.Before(w.retryAt) {
			if err := w.rotateLocked(); err != nil {
				w.retryAt = now.Add(logRotationRetry)
				if w.file == nil {
					return 0, err
				}
			} else {
				w.retryAt = time.Time{}
				w.suspended = false
			}
		}
	}
	if w.maxSize > 0 && w.size+int64(len(data)) > 2*w.maxSize {
		if !w.suspended {
			_ = w.writeMarkerLocked("log_writes_suspended", "rotation_failed")
			w.suspended = true
		}
		return len(data), nil
	}
	written, err := w.file.Write(data)
	w.size += int64(written)
	return written, err
}

func (w *rotatingLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *rotatingLogWriter) rotateLocked() error {
	file := w.file
	w.file = nil
	if err := file.Close(); err != nil {
		if recoverErr := w.recoverLocked("close_failed"); recoverErr != nil {
			return recoverErr
		}
		return err
	}
	if err := replaceFile(w.path, w.path+".1"); err != nil {
		if recoverErr := w.recoverLocked("replace_failed"); recoverErr != nil {
			return recoverErr
		}
		return err
	}
	w.reopenAfterRotation = true
	if err := w.openLocked(os.O_APPEND); err != nil {
		return err
	}
	w.reopenAfterRotation = false
	return nil
}

func (w *rotatingLogWriter) openLocked(flag int) error {
	file, err := os.OpenFile(w.path, flag|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return err
	}
	w.file = file
	w.size = info.Size()
	return nil
}

func (w *rotatingLogWriter) recoverLocked(reason string) error {
	if err := w.openLocked(os.O_APPEND); err != nil {
		return err
	}
	if w.suspended {
		return nil
	}
	return w.writeMarkerLocked("log_rotation_fallback", reason)
}

func (w *rotatingLogWriter) writeMarkerLocked(event, reason string) error {
	marker := fmt.Sprintf("time=%s level=WARN msg=%s reason=%s\n",
		time.Now().UTC().Format(time.RFC3339Nano), event, reason)
	written, err := w.file.WriteString(marker)
	w.size += int64(written)
	return err
}

func (a *application) logEvent(ctx context.Context, level slog.Level, event string, attributes ...any) {
	if a.logger == nil {
		return
	}
	a.logger.Log(ctx, level, event, attributes...)
}

func logReference(value string) string {
	if value == "" {
		return "none"
	}
	digest := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(digest[:6])
}

func proxyRouteKind(path string) string {
	path = strings.TrimPrefix(path, "/v1")
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "root"
	}
	segment, _, _ := strings.Cut(path, "/")
	switch segment {
	case "responses", "chat", "models":
		return segment
	default:
		return "other"
	}
}

func logErrorKind(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected_eof"
	}
	if os.IsPermission(err) {
		return "permission"
	}
	if os.IsNotExist(err) {
		return "not_found"
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return "dns"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	var operationError *net.OpError
	if errors.As(err, &operationError) {
		if operationError.Op == "listen" {
			return "listen"
		}
		if operationError.Op == "dial" {
			return "connect"
		}
		return "network"
	}
	return "other"
}

func upstreamOperationForLog(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return "invalid_url"
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case strings.HasSuffix(path, "/api/usage/token"):
		return "balance_token"
	case strings.HasSuffix(path, "/api/status"):
		return "status"
	case strings.HasSuffix(path, "/api/user/login"):
		return "account_login"
	case strings.HasSuffix(path, "/api/user/self"):
		return "account_balance"
	case strings.HasSuffix(path, "/dashboard/billing/subscription"):
		return "billing_subscription"
	case strings.HasSuffix(path, "/dashboard/billing/usage"):
		return "billing_usage"
	default:
		return "upstream_json"
	}
}

func upstreamReferenceForLog(target string) string {
	origin, ok := normalizedOrigin(target)
	if !ok {
		return "invalid"
	}
	return logReference(origin)
}
