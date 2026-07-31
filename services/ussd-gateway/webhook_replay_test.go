// webhook_replay_test.go — audit M-1: timestamp tolerance + replay cache
// on the USSD aggregator webhook.
package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/webhookguard"
)

func newReplayTestServer(t *testing.T) *server {
	t.Helper()
	t.Setenv("USSD_AGGREGATOR_KEY", "agg-secret")
	graph, err := LoadMenuGraph()
	if err != nil {
		t.Fatal(err)
	}
	return &server{
		graph:  graph,
		engine: NewEngine(graph, RegisterActions(&memPub{})),
		store:  NewInMemSessionStore(180),
		guard:  webhookguard.NewGuard("X-Aggregator-Timestamp", "X-Aggregator-Nonce", true, nil),
	}
}

func signedWebhookReq(t *testing.T, session, ts, nonce string) *http.Request {
	t.Helper()
	form := url.Values{"sessionId": {session}, "phoneNumber": {"+2348000000000"}, "text": {""}}
	body := form.Encode()
	req := httptest.NewRequest("POST", "/webhook/ussd", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Aggregator-Signature", signAggregator("agg-secret", []byte(body)))
	if ts != "" {
		req.Header.Set("X-Aggregator-Timestamp", ts)
	}
	if nonce != "" {
		req.Header.Set("X-Aggregator-Nonce", nonce)
	}
	return req
}

func TestWebhookExpiredTimestampRejected(t *testing.T) {
	srv := newReplayTestServer(t)
	stale := strconv.FormatInt(time.Now().Add(-10*time.Minute).Unix(), 10)
	rec := httptest.NewRecorder()
	srv.webhook(rec, signedWebhookReq(t, "exp-1", stale, "nonce-exp"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired timestamp: got %d, want 401", rec.Code)
	}
}

func TestWebhookMissingHeadersFailClosed(t *testing.T) {
	srv := newReplayTestServer(t) // RequireHeaders=true (prod semantics)
	rec := httptest.NewRecorder()
	srv.webhook(rec, signedWebhookReq(t, "miss-1", "", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing ts/nonce: got %d, want 401", rec.Code)
	}
}

func TestWebhookReplayDeduped409(t *testing.T) {
	srv := newReplayTestServer(t)
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	rec := httptest.NewRecorder()
	srv.webhook(rec, signedWebhookReq(t, "rep-1", ts, "nonce-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("first delivery: got %d, want 200", rec.Code)
	}
	// replayed webhook (same nonce) -> 409 dedup
	rec = httptest.NewRecorder()
	srv.webhook(rec, signedWebhookReq(t, "rep-1", ts, "nonce-1"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("replayed webhook: got %d, want 409", rec.Code)
	}
	// new nonce -> 200
	rec = httptest.NewRecorder()
	srv.webhook(rec, signedWebhookReq(t, "rep-2", ts, "nonce-2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("new nonce: got %d, want 200", rec.Code)
	}
}
