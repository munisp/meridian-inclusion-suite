package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// HTTPClient talks to the core ledger service (SPEC §2 ledger REST surface).
type HTTPClient struct {
	base string
	hc   *http.Client
}

func NewHTTPClient(base string) *HTTPClient {
	return &HTTPClient{base: strings.TrimRight(base, "/"), hc: &http.Client{Timeout: 10 * time.Second}}
}

func (h *HTTPClient) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Name", "inclusion")
	// B3 #5: service-to-service auth — shared env-injected token in prod,
	// forgeable X-Dev-Role only as the dev fallback.
	if tok := os.Getenv("MERIDIAN_SERVICE_TOKEN"); tok != "" {
		req.Header.Set("X-Service-Token", tok)
	} else if tok := os.Getenv("LEDGER_SERVICE_TOKEN"); tok != "" {
		req.Header.Set("X-Service-Token", tok)
	} else {
		req.Header.Set("X-Dev-Role", "operator") // dev only
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("ledger svc %s %s: %d %s", method, path, resp.StatusCode, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (h *HTTPClient) CreateAccounts(accts []Account) error {
	return h.do(http.MethodPost, "/v1/accounts", map[string]any{"accounts": accts}, nil)
}

type transferResp struct {
	ID         string `json:"id"`
	TransferID string `json:"transfer_id"`
}

func (h *HTTPClient) Transfer(t Transfer) (string, error) {
	var out transferResp
	err := h.do(http.MethodPost, "/v1/transfers", t, &out)
	return firstID(out), err
}

func (h *HTTPClient) PendingTransfer(t Transfer) (string, error) {
	var out transferResp
	err := h.do(http.MethodPost, "/v1/transfers/pending", t, &out)
	return firstID(out), err
}

func (h *HTTPClient) PostPending(pendingID string, amount uint64) (string, error) {
	return h.PostPendingAs(pendingID, "", amount)
}

// PostPendingAs posts a pending transfer under a caller-chosen post id
// (idempotent replay semantics live server-side; see core ledger service).
func (h *HTTPClient) PostPendingAs(pendingID, postID string, amount uint64) (string, error) {
	var out transferResp
	// B3 #13: the core ledger post contract is amount_kobo (0 => full
	// pending amount). Sending "amount" was silently decoded as 0, making
	// any partial capture post the FULL hold.
	err := h.do(http.MethodPost, "/v1/transfers/"+pendingID+"/post", map[string]any{"amount_kobo": amount, "post_id": postID}, &out)
	return firstID(out), err
}

// LookupTransfer returns a transfer by id: GET /v1/transfers/{id}.
func (h *HTTPClient) LookupTransfer(id string) (Transfer, error) {
	var out Transfer
	err := h.do(http.MethodGet, "/v1/transfers/"+id, nil, &out)
	return out, err
}

func (h *HTTPClient) VoidPending(pendingID string) (string, error) {
	var out transferResp
	err := h.do(http.MethodPost, "/v1/transfers/"+pendingID+"/void", nil, &out)
	return firstID(out), err
}

func (h *HTTPClient) Balance(accountID string) (Balance, error) {
	var out Balance
	err := h.do(http.MethodGet, "/v1/accounts/"+accountID+"/balance", nil, &out)
	return out, err
}

func firstID(r transferResp) string {
	if r.ID != "" {
		return r.ID
	}
	return r.TransferID
}

// NewClientFromEnv returns an HTTPClient when LEDGER_URL is set, otherwise the
// dev in-memory TigerBeetle-semantics client (§1.5 fallback). PROFILE=prod
// refuses to boot without LEDGER_URL: the volatile in-mem dev ledger would
// silently lose all financial state on restart (V2 round, B3 #4 repair).
func NewClientFromEnv() Client {
	if u := os.Getenv("LEDGER_URL"); u != "" {
		return NewHTTPClient(u)
	}
	if os.Getenv("PROFILE") == "prod" {
		log.Fatal("PROFILE=prod requires LEDGER_URL: refusing to boot with the volatile in-memory dev ledger")
	}
	return NewDevClient()
}
