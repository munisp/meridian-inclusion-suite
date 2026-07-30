package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAggregatorSignatureVerification(t *testing.T) {
	t.Setenv("USSD_AGGREGATOR_KEY", "agg-secret")
	body := []byte("sessionId=s1&phoneNumber=%2B2348000000000&text=1")
	good := signAggregator("agg-secret", body)
	if !VerifyAggregatorSignature(good, body) {
		t.Fatal("valid signature rejected")
	}
	if VerifyAggregatorSignature("deadbeef", body) {
		t.Fatal("invalid signature accepted")
	}
	if VerifyAggregatorSignature("", body) {
		t.Fatal("empty signature accepted in prod mode")
	}
	// dev mode: key unset -> accepted
	t.Setenv("USSD_AGGREGATOR_KEY", "")
	if !VerifyAggregatorSignature("", body) {
		t.Fatal("dev mode should accept unsigned webhooks")
	}
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	t.Setenv("USSD_AGGREGATOR_KEY", "agg-secret")
	graph, err := LoadMenuGraph()
	if err != nil {
		t.Fatal(err)
	}
	srv := &server{graph: graph, engine: NewEngine(graph, RegisterActions(&memPub{})), store: NewInMemSessionStore(180)}
	form := url.Values{"sessionId": {"sig-test"}, "phoneNumber": {"+2348000000000"}, "text": {""}}
	req := httptest.NewRequest("POST", "/webhook/ussd", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Aggregator-Signature", "wrong")
	rec := httptest.NewRecorder()
	srv.webhook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	// correctly signed passes
	body := form.Encode()
	req = httptest.NewRequest("POST", "/webhook/ussd", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Aggregator-Signature", signAggregator("agg-secret", []byte(body)))
	rec = httptest.NewRecorder()
	srv.webhook(rec, req)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "CON ") {
		t.Fatalf("want CON response, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestAggregatorNotifier(t *testing.T) {
	var gotSig, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotSig = r.Header.Get("X-Aggregator-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	n := NewAggregatorNotifier(srv.URL, "k")
	if err := n.Notify("sess-1", "+2348000000000", "END done"); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if gotSig != signAggregator("k", []byte(gotBody)) {
		t.Fatal("notify signature mismatch")
	}
	var payload map[string]string
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil || payload["session_id"] != "sess-1" {
		t.Fatalf("bad payload: %s", gotBody)
	}
}
