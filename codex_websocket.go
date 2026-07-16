package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

const (
	codexWebSocketHost              = "chatgpt.com"
	codexWebSocketPath              = "/backend-api/codex/responses"
	codexWebSocketBetaHeader        = "responses_websockets=2026-02-06"
	codexWebSocketConnectTimeout    = 15 * time.Second
	codexWebSocketIdleTimeout       = 5 * time.Minute
	codexWebSocketMaxAge            = 55 * time.Minute
	codexWebSocketHandshakeAttempts = 2
)

type codexWebSocketRequestSentError struct {
	Err error
}

func (err *codexWebSocketRequestSentError) Error() string {
	if err == nil || err.Err == nil {
		return "Codex WebSocket request may have been sent"
	}
	return "Codex WebSocket request may have been sent: " + err.Err.Error()
}

func (err *codexWebSocketRequestSentError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Err
}

type codexWebSocketDialFunc func(context.Context, http.Header) (*websocket.Conn, error)

type codexWebSocketCacheKey struct {
	accountID string
	revision  int
	sessionID string
}

type codexWebSocketCacheEntry struct {
	connection     *websocket.Conn
	createdAt      time.Time
	busy           bool
	idleTimer      *time.Timer
	idleGeneration uint64
}

type codexWebSocketPool struct {
	mu      sync.Mutex
	entries map[codexWebSocketCacheKey]*codexWebSocketCacheEntry
}

type codexWebSocketLease struct {
	pool        *codexWebSocketPool
	key         codexWebSocketCacheKey
	entry       *codexWebSocketCacheEntry
	connection  *websocket.Conn
	releaseOnce sync.Once
}

var sharedCodexWebSocketPool = &codexWebSocketPool{
	entries: make(map[codexWebSocketCacheKey]*codexWebSocketCacheEntry),
}

func newCodexWebSocketDialer(base *http.Transport) codexWebSocketDialFunc {
	if base == nil {
		return nil
	}
	return func(ctx context.Context, header http.Header) (*websocket.Conn, error) {
		target := &url.URL{Scheme: "wss", Host: codexWebSocketHost, Path: codexWebSocketPath}
		origin := &url.URL{Scheme: "https", Host: codexWebSocketHost}
		connection, err := dialCodexWebSocketTarget(ctx, target, base)
		if err != nil {
			return nil, err
		}
		if deadline, ok := ctx.Deadline(); ok {
			if err = connection.SetDeadline(deadline); err != nil {
				_ = connection.Close()
				return nil, err
			}
		}
		config := &websocket.Config{
			Location: target,
			Origin:   origin,
			Version:  websocket.ProtocolVersionHybi13,
			Header:   header.Clone(),
		}
		stopCancel := context.AfterFunc(ctx, func() {
			_ = connection.SetDeadline(time.Now())
		})
		webSocket, err := websocket.NewClient(config, connection)
		stopCancel()
		if err != nil {
			_ = connection.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		if ctx.Err() != nil {
			closeCodexWebSocket(webSocket)
			return nil, ctx.Err()
		}
		if err = webSocket.SetDeadline(time.Time{}); err != nil {
			closeCodexWebSocket(webSocket)
			return nil, err
		}
		webSocket.MaxPayloadBytes = int(codexOAuthResponseLimit)
		return webSocket, nil
	}
}

func dialCodexWebSocketWithRetry(
	ctx context.Context,
	dial codexWebSocketDialFunc,
	header http.Header,
) (*websocket.Conn, error) {
	if dial == nil {
		return nil, errors.New("Codex WebSocket transport is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var lastErr error
	for attempt := 0; attempt < codexWebSocketHandshakeAttempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, codexWebSocketConnectTimeout)
		connection, err := dial(attemptCtx, header)
		cancel()
		if err == nil {
			return connection, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

func (pool *codexWebSocketPool) acquire(
	ctx context.Context,
	key codexWebSocketCacheKey,
	dial codexWebSocketDialFunc,
	header http.Header,
) (*codexWebSocketLease, error) {
	if pool == nil {
		return nil, errors.New("Codex WebSocket pool is unavailable")
	}
	now := time.Now()
	var stale *websocket.Conn
	pool.mu.Lock()
	if pool.entries == nil {
		pool.entries = make(map[codexWebSocketCacheKey]*codexWebSocketCacheEntry)
	}
	entry := pool.entries[key]
	if entry != nil {
		if entry.idleTimer != nil {
			entry.idleGeneration++
			entry.idleTimer.Stop()
			entry.idleTimer = nil
		}
		switch {
		case entry.connection == nil:
			delete(pool.entries, key)
		case !entry.busy && now.Sub(entry.createdAt) >= codexWebSocketMaxAge:
			delete(pool.entries, key)
			stale = entry.connection
		case !entry.busy:
			entry.busy = true
			lease := &codexWebSocketLease{
				pool: pool, key: key, entry: entry, connection: entry.connection,
			}
			pool.mu.Unlock()
			return lease, nil
		}
	}
	pool.mu.Unlock()
	if stale != nil {
		closeCodexWebSocket(stale)
	}

	connection, err := dialCodexWebSocketWithRetry(ctx, dial, header)
	if err != nil {
		return nil, err
	}
	pool.mu.Lock()
	if pool.entries[key] == nil {
		entry = &codexWebSocketCacheEntry{connection: connection, createdAt: time.Now(), busy: true}
		pool.entries[key] = entry
		lease := &codexWebSocketLease{pool: pool, key: key, entry: entry, connection: connection}
		pool.mu.Unlock()
		return lease, nil
	}
	pool.mu.Unlock()
	return &codexWebSocketLease{connection: connection}, nil
}

func (lease *codexWebSocketLease) release(keep bool) {
	if lease == nil {
		return
	}
	lease.releaseOnce.Do(func() {
		if lease.pool == nil || lease.entry == nil {
			closeCodexWebSocket(lease.connection)
			return
		}
		pool := lease.pool
		pool.mu.Lock()
		entry := pool.entries[lease.key]
		if entry != lease.entry || entry.connection != lease.connection {
			pool.mu.Unlock()
			closeCodexWebSocket(lease.connection)
			return
		}
		if !keep || time.Since(entry.createdAt) >= codexWebSocketMaxAge {
			delete(pool.entries, lease.key)
			if entry.idleTimer != nil {
				entry.idleTimer.Stop()
				entry.idleTimer = nil
			}
			pool.mu.Unlock()
			closeCodexWebSocket(entry.connection)
			return
		}
		entry.busy = false
		entry.idleGeneration++
		generation := entry.idleGeneration
		entry.idleTimer = time.AfterFunc(codexWebSocketIdleTimeout, func() {
			pool.expire(lease.key, entry, generation)
		})
		pool.mu.Unlock()
	})
}

func (pool *codexWebSocketPool) expire(
	key codexWebSocketCacheKey,
	entry *codexWebSocketCacheEntry,
	generation uint64,
) {
	if pool == nil || entry == nil {
		return
	}
	pool.mu.Lock()
	if pool.entries[key] != entry || entry.busy || entry.idleGeneration != generation {
		pool.mu.Unlock()
		return
	}
	delete(pool.entries, key)
	entry.idleTimer = nil
	pool.mu.Unlock()
	closeCodexWebSocket(entry.connection)
}

func closeCodexWebSocket(connection *websocket.Conn) {
	if connection == nil {
		return
	}
	_ = connection.SetDeadline(time.Now())
	_ = connection.Close()
}

func (provider *codexProvider) sendCodexWebSocket(
	ctx context.Context,
	original *http.Request,
	body []byte,
	localAccountID string,
	accountRevision int,
	accessToken string,
	chatGPTAccountID string,
	sessionID string,
) (*http.Response, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestBody, err := codexWebSocketRequestBody(body)
	if err != nil {
		return nil, false, err
	}
	if provider == nil || provider.websocketDial == nil {
		return nil, false, errors.New("Codex WebSocket transport is unavailable")
	}
	pool := provider.websockets
	if pool == nil {
		pool = sharedCodexWebSocketPool
	}
	lease, err := pool.acquire(ctx, codexWebSocketCacheKey{
		accountID: localAccountID,
		revision:  accountRevision,
		sessionID: sessionID,
	}, provider.websocketDial, provider.codexWebSocketHeaders(accessToken, chatGPTAccountID, sessionID))
	if err != nil {
		return nil, false, err
	}
	if err = ctx.Err(); err != nil {
		lease.release(false)
		return nil, false, err
	}
	if err = lease.connection.SetDeadline(time.Time{}); err != nil {
		lease.release(false)
		return nil, false, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err = lease.connection.SetWriteDeadline(deadline); err != nil {
			lease.release(false)
			return nil, false, err
		}
	}
	stopSendCancel := context.AfterFunc(ctx, func() {
		lease.release(false)
	})
	err = websocket.Message.Send(lease.connection, string(requestBody))
	stopSendCancel()
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		lease.release(false)
		return newCodexWebSocketErrorResponse(original, &codexWebSocketRequestSentError{Err: err}), true, nil
	}
	if err = lease.connection.SetWriteDeadline(time.Time{}); err != nil {
		lease.release(false)
		return newCodexWebSocketErrorResponse(original, &codexWebSocketRequestSentError{Err: err}), true, nil
	}
	return newCodexWebSocketStreamResponse(ctx, original, lease), true, nil
}

func (provider *codexProvider) codexWebSocketHeaders(accessToken, chatGPTAccountID, sessionID string) http.Header {
	header := make(http.Header)
	header.Set("Authorization", "Bearer "+accessToken)
	header.Set("ChatGPT-Account-ID", chatGPTAccountID)
	header.Set("User-Agent", codexOAuthUserAgent)
	header.Set("Originator", codexOAuthOriginator)
	header.Set("OpenAI-Beta", codexWebSocketBetaHeader)
	header.Set("session-id", sessionID)
	header.Set("x-client-request-id", sessionID)
	if provider != nil && provider.client != nil && provider.client.Jar != nil {
		request := &http.Request{Header: header}
		for _, cookie := range provider.client.Jar.Cookies(&url.URL{
			Scheme: "https", Host: codexWebSocketHost, Path: codexWebSocketPath,
		}) {
			request.AddCookie(cookie)
		}
	}
	return header
}

func codexWebSocketRequestBody(body []byte) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		if err == nil {
			err = errors.New("request body is not a JSON object")
		}
		return nil, err
	}
	payload["type"] = json.RawMessage(`"response.create"`)
	return json.Marshal(payload)
}

type codexWebSocketBody struct {
	reader    *io.PipeReader
	close     func()
	closeOnce sync.Once
	closeErr  error
}

func (body *codexWebSocketBody) Read(buffer []byte) (int, error) {
	if body == nil || body.reader == nil {
		return 0, io.EOF
	}
	return body.reader.Read(buffer)
}

func (body *codexWebSocketBody) Close() error {
	if body == nil {
		return nil
	}
	body.closeOnce.Do(func() {
		if body.close != nil {
			body.close()
		}
		if body.reader != nil {
			body.closeErr = body.reader.Close()
		}
	})
	return body.closeErr
}

func newCodexWebSocketStreamResponse(
	ctx context.Context,
	original *http.Request,
	lease *codexWebSocketLease,
) *http.Response {
	if ctx == nil {
		ctx = context.Background()
	}
	reader, writer := io.Pipe()
	var finishOnce sync.Once
	finish := func(keep bool, err error) {
		finishOnce.Do(func() {
			lease.release(keep)
			if err != nil {
				_ = writer.CloseWithError(err)
			} else {
				_ = writer.Close()
			}
		})
	}
	stopContext := context.AfterFunc(ctx, func() {
		finish(false, ctx.Err())
	})
	body := &codexWebSocketBody{
		reader: reader,
		close: func() {
			finish(false, context.Canceled)
		},
	}
	go func() {
		keep, err := pumpCodexWebSocket(lease.connection, writer)
		stopContext()
		finish(keep, err)
	}()
	return newCodexWebSocketResponse(original, body)
}

func newCodexWebSocketErrorResponse(original *http.Request, err error) *http.Response {
	reader, writer := io.Pipe()
	_ = writer.CloseWithError(err)
	return newCodexWebSocketResponse(original, reader)
}

func newCodexWebSocketResponse(original *http.Request, body io.ReadCloser) *http.Response {
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"text/event-stream"}, "Cache-Control": []string{"no-store"}},
		Body:          body,
		ContentLength: -1,
		Request:       original,
	}
}

func pumpCodexWebSocket(connection *websocket.Conn, writer *io.PipeWriter) (bool, error) {
	for {
		if err := connection.SetReadDeadline(time.Now().Add(codexWebSocketIdleTimeout)); err != nil {
			return false, &codexWebSocketRequestSentError{Err: err}
		}
		var payload string
		if err := websocket.Message.Receive(connection, &payload); err != nil {
			return false, &codexWebSocketRequestSentError{Err: err}
		}
		frame, terminal, keep, err := codexWebSocketSSEFrame([]byte(payload))
		if err != nil {
			return false, &codexWebSocketRequestSentError{Err: err}
		}
		if len(frame) == 0 {
			continue
		}
		if _, err = writer.Write(frame); err != nil {
			return false, &codexWebSocketRequestSentError{Err: err}
		}
		if terminal {
			return keep, nil
		}
	}
}

func codexWebSocketSSEFrame(payload []byte) ([]byte, bool, bool, error) {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil, false, false, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope == nil {
		if err == nil {
			err = errors.New("event is not a JSON object")
		}
		return nil, false, false, fmt.Errorf("invalid Codex WebSocket event: %w", err)
	}
	var eventType string
	if rawType, ok := envelope["type"]; ok {
		if err := json.Unmarshal(rawType, &eventType); err != nil {
			return nil, false, false, fmt.Errorf("invalid Codex WebSocket event type: %w", err)
		}
	}
	terminal := false
	keep := false
	switch eventType {
	case "response.done", "response.incomplete":
		envelope["type"] = json.RawMessage(`"response.completed"`)
		normalized, err := json.Marshal(envelope)
		if err != nil {
			return nil, false, false, err
		}
		payload = normalized
		terminal = true
		keep = true
	case "response.completed":
		terminal = true
		keep = true
	case "error", "response.failed":
		terminal = true
	}
	frame := make([]byte, 0, len("data: ")+len(payload)+2)
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	return frame, terminal, keep, nil
}
