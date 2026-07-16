package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestResponsesContextSticksAndReplaysAcrossQuotaFailover(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyRoundRobin, func() time.Time { return now },
		testAccount("a", "https://first.invalid/v1", "key-a"),
		testAccount("b", "https://second.invalid/v1", "key-b"),
	)
	firstCalls := 0
	secondCalls := 0
	var stickyBody []byte
	var replayedBody []byte
	app.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(request.Body)
		switch request.URL.Host {
		case "first.invalid":
			firstCalls++
			if firstCalls == 1 {
				return responsesTestSSE(request,
					`data: {"type":"response.completed","response":{"id":"resp_a","output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]}}`+"\n\n"), nil
			}
			stickyBody = append([]byte(nil), body...)
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"insufficient_quota","message":"quota exhausted"}}`)),
				Request:    request,
			}, nil
		case "second.invalid":
			secondCalls++
			replayedBody = append([]byte(nil), body...)
			return responsesTestSSE(request,
				`data: {"type":"response.completed","response":{"id":"resp_b","output":[{"type":"message","role":"assistant","content":[]}]}}`+"\n\n"), nil
		default:
			t.Fatalf("unexpected upstream host %q", request.URL.Host)
			return nil, nil
		}
	})}

	first := proxyRequest(app, `{"model":"gpt-test","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"id":"resp_a"`) {
		t.Fatalf("first response status=%d body=%s", first.Code, first.Body.String())
	}
	second := proxyRequest(app, `{"model":"gpt-test","stream":true,"previous_response_id":"resp_a","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"id":"resp_b"`) {
		t.Fatalf("second response status=%d body=%s", second.Code, second.Body.String())
	}
	if firstCalls != 2 || secondCalls != 1 {
		t.Fatalf("session was not sticky before failover: first=%d second=%d", firstCalls, secondCalls)
	}
	if !strings.Contains(string(stickyBody), `"previous_response_id":"resp_a"`) {
		t.Fatalf("healthy owner did not receive its reusable response id: %s", stickyBody)
	}

	var payload map[string]any
	if err := json.Unmarshal(replayedBody, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["previous_response_id"]; exists {
		t.Fatalf("old response id reached the replacement account: %s", replayedBody)
	}
	input, ok := payload["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("unexpected replay input: %#v", payload["input"])
	}
	wantTypes := []string{"message", "function_call", "function_call_output"}
	for index, want := range wantTypes {
		item, ok := input[index].(map[string]any)
		if !ok || item["type"] != want {
			t.Fatalf("input[%d]=%#v, want type %q", index, input[index], want)
		}
	}
	include, ok := payload["include"].([]any)
	if !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("reasoning replay data was not requested: %#v", payload["include"])
	}
}

func TestResponsesStateIsIsolatedAndExpires(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	app := &application{now: func() time.Time { return now }}
	cache := app.getResponsesStateCache()
	input := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user"}`)}
	if !cache.store("token-a", "resp_shared", "account-a", 7, true, input, now) {
		t.Fatal("state was not stored")
	}
	if _, ok := cache.lookup("token-b", "resp_shared", now); ok {
		t.Fatal("response state crossed gateway token boundary")
	}
	entry, ok := cache.lookup("token-a", "resp_shared", now)
	if !ok || entry.accountID != "account-a" || entry.accountRevision != 7 ||
		!entry.upstreamReusable || len(entry.input) != 1 {
		t.Fatalf("stored state missing: %#v ok=%v", entry, ok)
	}
	now = now.Add(responsesStateTTL)
	if _, ok := cache.lookup("token-a", "resp_shared", now); ok {
		t.Fatal("expired response state remained available")
	}

	requestState := &responsesRequestState{input: input, cacheable: true}
	result := app.storeResponsesResponse(
		"token-a", requestState, accountConfig{ID: "account-a", Revision: 1}, "text/event-stream",
		[]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_incomplete\",\"output\":[]}}\n\n"),
	)
	if !result.terminalFailure {
		t.Fatal("incomplete stream was not treated as a terminal failure")
	}
	if _, ok := cache.lookup("token-a", "resp_incomplete", now); ok {
		t.Fatal("incomplete stream was cached")
	}
}

func TestResponsesOwnerRevisionChangeStopsBeforeRouting(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	owner := testAccount("a", "https://first.invalid/v1", "key-a")
	owner.Revision = 2
	app := newTestApplication(t, strategyPriority, func() time.Time { return now }, owner,
		testAccount("b", "https://second.invalid/v1", "key-b"),
	)
	input := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[]}`)}
	if !app.getResponsesStateCache().store("tok_test", "resp_old", owner.ID, 1, true, input, now) {
		t.Fatal("response state was not stored")
	}
	calls := 0
	app.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, nil
	})}

	response := proxyRequest(app, `{"model":"gpt-test","previous_response_id":"resp_old","input":"continue"}`)
	if response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), `"code":"session_account_changed"`) || calls != 0 {
		t.Fatalf("revision mismatch status=%d calls=%d body=%s", response.Code, calls, response.Body.String())
	}
}

func TestResponsesSSEQuotaBeforeOutputSwitchesAccounts(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	app := newTestApplication(t, strategyPriority, func() time.Time { return now },
		testAccount("a", "https://first.invalid/v1", "key-a"),
		testAccount("b", "https://second.invalid/v1", "key-b"),
	)
	firstCalls := 0
	secondCalls := 0
	app.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "first.invalid" {
			firstCalls++
			return responsesTestSSE(request,
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_a\",\"output\":[]}}\n\n"+
					"data: {\"type\":\"error\",\"error\":{\"type\":\"usage_limit_reached\",\"resets_in_seconds\":3600}}\n\n"), nil
		}
		secondCalls++
		return responsesTestSSE(request,
			`data: {"type":"response.completed","response":{"id":"resp_b","output":[]}}`+"\n\n"), nil
	})}

	response := proxyRequest(app, `{"model":"gpt-test","stream":true,"input":"hello"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"resp_b"`) ||
		strings.Contains(response.Body.String(), "usage_limit_reached") || strings.Contains(response.Body.String(), `"id":"resp_a"`) {
		t.Fatalf("pre-output quota event leaked: status=%d body=%s", response.Code, response.Body.String())
	}
	if firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("quota stream calls: first=%d second=%d", firstCalls, secondCalls)
	}
}

func TestResponsesSSEAfterOutputIsNeverReplayed(t *testing.T) {
	app := newTestApplication(t, strategyPriority, time.Now,
		testAccount("a", "https://first.invalid/v1", "key-a"),
		testAccount("b", "https://second.invalid/v1", "key-b"),
	)
	firstCalls := 0
	secondCalls := 0
	app.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "first.invalid" {
			firstCalls++
			return responsesTestSSE(request,
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_a\",\"output\":[]}}\n\n"+
					"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"+
					"data: {\"type\":\"error\",\"error\":{\"type\":\"usage_limit_reached\"}}\n\n"), nil
		}
		secondCalls++
		return responsesTestSSE(request,
			`data: {"type":"response.completed","response":{"id":"resp_b","output":[]}}`+"\n\n"), nil
	})}

	response := proxyRequest(app, `{"model":"gpt-test","stream":true,"input":"hello"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "partial") ||
		!strings.Contains(response.Body.String(), "usage_limit_reached") {
		t.Fatalf("started output was not preserved: status=%d body=%s", response.Code, response.Body.String())
	}
	if firstCalls != 1 || secondCalls != 0 {
		t.Fatalf("started stream was replayed: first=%d second=%d", firstCalls, secondCalls)
	}
}

func responsesTestSSE(request *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}
}
