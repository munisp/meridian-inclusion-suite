package ledger

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// B3 #13 regression: the core ledger post contract is `amount_kobo`
// (0 => full pending amount). The client previously sent `amount`, which
// the server decoded as 0 — any partial capture silently posted the FULL
// hold.
func TestPostPendingAsSendsAmountKobo(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"tx-post-1"}`))
	}))
	defer srv.Close()

	c := NewHTTPClient(srv.URL)
	if _, err := c.PostPendingAs("pend-1", "post-1", 42_500); err != nil {
		t.Fatal(err)
	}
	if got["amount_kobo"] != float64(42_500) {
		t.Fatalf("server received %v, want amount_kobo=42500", got)
	}
	if _, bad := got["amount"]; bad {
		t.Fatal("client still sends the silently-dropped `amount` field")
	}
}
