package main

// nip_test.go — N1 coverage: session-id format, mandatory name-enquiry
// gate, idempotent retry, TSQ ambiguity resolution, reversal (vs refund)
// flow, TSQ sweeper, sim/HTTP boundary, fail-closed live rail.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
)

func nipTestService(t *testing.T, rail NIPRail, requireNE bool) *NIPService {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/nip.json")
	if err != nil {
		t.Fatal(err)
	}
	return NewNIPService(rail, st, nil, requireNE)
}

func payoutReq(account string) PayoutRequest {
	return PayoutRequest{
		Purpose: "payout", AmountKobo: 150_000, DestAccount: account,
		DestBankCode: "058", Narration: "agent float payout",
		IdempotencyKey: "idem-" + account,
	}
}

// 1. Session ids follow the documented NIP structure: 30 digits =
// 6 institution + 8 date + 16 random suffix.
func TestNIPSessionIDFormat(t *testing.T) {
	id := NewNIPSessionID()
	if !regexp.MustCompile(`^\d{30}$`).MatchString(id) {
		t.Fatalf("session id %q is not 30 digits", id)
	}
	if id[:6] != "999999" { // placeholder institution code
		t.Fatalf("institution prefix: %s", id[:6])
	}
	if id[6:14] != time.Now().UTC().Format("20060102") {
		t.Fatalf("date segment: %s", id[6:14])
	}
	if NewNIPSessionID() == id {
		t.Fatal("session ids must be unique")
	}
}

// 2. The name-enquiry gate BLOCKS a transfer to an unverifiable account.
func TestNameEnquiryGateBlocksUnverified(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), true)
	_, err := svc.Payout(payoutReq("0000000000"))
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("expected blocked transfer, got %v", err)
	}
	// and nothing was dispatched to the rail
	res, _ := svc.rail.TransactionStatusQuery("anything")
	if res.Status == NIPStatusSuccess {
		t.Fatal("rail must not have seen the blocked transfer")
	}
}

// 3. Gate enabled: a verified account name is captured on the record.
func TestPayoutCapturesVerifiedName(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), true)
	tr, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != NIPStatusSuccess || tr.DestName == "" || tr.NameEnquiryID == "" {
		t.Fatalf("expected success with verified name, got %+v", tr)
	}
}

// 4. Gate disabled (dev flag): unverifiable account still transfers.
func TestNameEnquiryGateDisabledDev(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), false)
	tr, err := svc.Payout(payoutReq("0000000000"))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != NIPStatusSuccess || tr.DestName != "" {
		t.Fatalf("gate-off payout: %+v", tr)
	}
}

// 5. Idempotent retry: same idempotency key returns the same session id and
// does not double-dispatch to the rail.
func TestIdempotentRetrySameSessionID(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), true)
	a, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Payout(payoutReq("0123456789"))
	if err != nil {
		t.Fatal(err)
	}
	if a.SessionID != b.SessionID || a.ID != b.ID {
		t.Fatalf("idempotent retry diverged: %s vs %s", a.SessionID, b.SessionID)
	}
}

// 6. Payout without an idempotency key is rejected.
func TestPayoutRequiresIdempotencyKey(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), true)
	req := payoutReq("0123456789")
	req.IdempotencyKey = ""
	if _, err := svc.Payout(req); err == nil {
		t.Fatal("expected idempotency key requirement")
	}
}

// 7. In-flight transfer resolved by TSQ to failed -> sweeper auto-reverses.
func TestTSQSweeperResolvesFailedAndAutoReverses(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), true)
	req := payoutReq("0123456789")
	req.Narration = "HANG " + req.Narration // sim trigger: timeout in flight
	req.IdempotencyKey = "idem-hang-1"
	tr, err := svc.Payout(req)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Status != NIPStatusInFlight {
		t.Fatalf("expected in_flight, got %s", tr.Status)
	}
	n, err := svc.SweepTSQ()
	if err != nil || n != 1 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	got, ok, _ := svc.getBySession(tr.SessionID)
	if !ok || got.Status != NIPStatusReversed {
		t.Fatalf("expected auto-reversed, got %+v", got)
	}
}

// 8. TSQ resolves an ambiguous transfer to success (recovered in flight).
func TestTSQResolvesAmbiguousToSuccess(t *testing.T) {
	rail := NewNIPSim()
	svc := nipTestService(t, rail, true)
	req := payoutReq("0123456789")
	req.Narration = "HANG " + req.Narration
	req.IdempotencyKey = "idem-hang-2"
	tr, err := svc.Payout(req)
	if err != nil {
		t.Fatal(err)
	}
	TSQResolveAs(rail, tr.SessionID, NIPStatusSuccess) // beneficiary confirms credit
	n, _ := svc.SweepTSQ()
	if n != 1 {
		t.Fatalf("sweep resolved %d", n)
	}
	got, _, _ := svc.getBySession(tr.SessionID)
	if got.Status != NIPStatusSuccess {
		t.Fatalf("expected success, got %s", got.Status)
	}
	// second sweep is a no-op (already final)
	if n, _ := svc.SweepTSQ(); n != 0 {
		t.Fatalf("re-sweep should resolve 0, got %d", n)
	}
}

// 9. Reversal flow: failed transfer reverses; settled transfer must use
// refund instead (CBN distinction); reversal is idempotent.
func TestReversalFlowDistinctFromRefund(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), true)
	tr, _ := svc.Payout(payoutReq("0123456789")) // success
	if _, err := svc.Reversal(tr.SessionID, "test"); err == nil {
		t.Fatal("reversal of a settled transfer must be refused (use refund)")
	}
	// force a failed record, then reverse it
	rail := svc.rail
	TSQResolveAs(rail, tr.SessionID, NIPStatusFailed)
	rec, _, _ := svc.getBySession(tr.SessionID)
	rec.Status = NIPStatusFailed
	svc.put(rec)
	rev, err := svc.Reversal(tr.SessionID, "erroneous debit")
	if err != nil || rev.Status != NIPStatusReversed {
		t.Fatalf("reversal: %+v err=%v", rev, err)
	}
	again, err := svc.Reversal(tr.SessionID, "erroneous debit")
	if err != nil || again.Status != NIPStatusReversed {
		t.Fatalf("reversal must be idempotent: %+v err=%v", again, err)
	}
}

// 10. Sim and HTTP adapters both satisfy the NIPRail boundary; the HTTP
// adapter sends NIBSS-convention auth/signature/session headers.
func TestSimHTTPBoundary(t *testing.T) {
	var _ NIPRail = NewNIPSim()
	var _ NIPRail = &NIPHTTPAdapter{}

	var gotAuth, gotSig, gotSess string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSig = r.Header.Get("X-Signature")
		gotSess = r.Header.Get("X-NIP-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			w.Write([]byte(`{"ok":true}`))
		case "/name-enquiry":
			json.NewEncoder(w).Encode(NameEnquiryResult{AccountName: "CHIDI OKAFOR", Verified: true})
		case "/funds-transfer":
			json.NewEncoder(w).Encode(NIPTransferResult{Status: NIPStatusSuccess, ResponseCode: "00"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()

	rail, err := NewNIPHTTPAdapter(server.URL, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := rail.Probe(); err != nil {
		t.Fatal(err)
	}
	ne, err := rail.NameEnquiry("0123456789", "058")
	if err != nil || !ne.Verified || ne.AccountName != "CHIDI OKAFOR" {
		t.Fatalf("name enquiry: %+v err=%v", ne, err)
	}
	res, err := rail.FundsTransfer(NIPTransferRequest{
		SessionID: NewNIPSessionID(), AmountKobo: 5000, DestAccount: "0123456789", DestBankCode: "058",
	})
	if err != nil || res.Status != NIPStatusSuccess {
		t.Fatalf("transfer: %+v err=%v", res, err)
	}
	if gotAuth != "Bearer test-key" || gotSig == "" || gotSess == "" {
		t.Fatalf("missing NIBSS-convention headers: auth=%q sig=%q sess=%q", gotAuth, gotSig, gotSess)
	}
}

// 11. HTTP adapter transport failure maps to in_flight (TSQ territory), not
// a hard failure — never blind-retry a transfer.
func TestHTTPTransportErrorIsInFlight(t *testing.T) {
	rail, err := NewNIPHTTPAdapter("http://127.0.0.1:1", "k")
	if err != nil {
		t.Fatal(err)
	}
	res, err := rail.FundsTransfer(NIPTransferRequest{SessionID: NewNIPSessionID(), AmountKobo: 100, DestAccount: "0123456789", DestBankCode: "058"})
	if err != nil {
		t.Fatalf("transfer must not hard-error: %v", err)
	}
	if res.Status != NIPStatusInFlight {
		t.Fatalf("expected in_flight, got %s", res.Status)
	}
}

// 12. Fail-closed: NIP_RAIL=live without NIP_API_URL is a startup error.
func TestFailClosedLiveWithoutURL(t *testing.T) {
	t.Setenv("NIP_RAIL", "live")
	t.Setenv("NIP_API_URL", "")
	if _, err := NewNIPRailFromEnv(); err == nil {
		t.Fatal("live rail without URL must fail closed")
	}
}

// 13. Fail-closed: NIP_RAIL=live with an unreachable endpoint is an error.
func TestFailClosedLiveUnreachable(t *testing.T) {
	t.Setenv("NIP_RAIL", "live")
	t.Setenv("NIP_API_URL", "http://127.0.0.1:1")
	if _, err := NewNIPRailFromEnv(); err == nil || !strings.Contains(err.Error(), "fail-closed") {
		t.Fatalf("expected fail-closed error, got %v", err)
	}
}

// 14. Dev profile (no NIP_API_URL) selects the simulator.
func TestDevProfileSelectsSim(t *testing.T) {
	t.Setenv("NIP_RAIL", "")
	t.Setenv("NIP_API_URL", "")
	rail, err := NewNIPRailFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if rail.Name() != "nip-sim" {
		t.Fatalf("expected sim rail, got %s", rail.Name())
	}
}

// 15. Refund rides through the same gate + idempotency as payout and is
// recorded as a distinct purpose.
func TestRefundPathGatedAndRecorded(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), true)
	req := payoutReq("0123456789")
	tr, err := svc.Refund(req)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Purpose != "refund" || tr.DestName == "" {
		t.Fatalf("refund record: %+v", tr)
	}
	if _, err := svc.Refund(payoutReq("0000000000")); err == nil {
		t.Fatal("refund must honour the name-enquiry gate")
	}
}

// countingRail wraps a rail and counts FundsTransfer dispatches.
type countingRail struct {
	NIPRail
	dispatches int
}

func (r *countingRail) FundsTransfer(req NIPTransferRequest) (NIPTransferResult, error) {
	r.dispatches++
	return r.NIPRail.FundsTransfer(req)
}

// 18. Audit funds-flow #1: the durable in_flight record is written BEFORE
// the rail dispatch, so a client retry with the same idempotency key while
// the transfer is in flight returns the same record (same session id) and
// NEVER triggers a second rail dispatch. The crash-after-dispatch window
// (transport success, process killed before the record update) is now
// impossible by construction: the record exists before dispatch, and the
// TSQ sweeper adopts and reconciles it.
func TestPayoutRetryDuringInflightSingleRailCall(t *testing.T) {
	rail := &countingRail{NIPRail: NewNIPSim()}
	svc := nipTestService(t, rail, true)
	req := payoutReq("0123456789")
	req.Narration = "HANG " + req.Narration // sim trigger: dispatch hangs in flight
	req.IdempotencyKey = "idem-inflight-retry"

	a, err := svc.Payout(req)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != NIPStatusInFlight {
		t.Fatalf("expected in_flight, got %s", a.Status)
	}
	// the pre-dispatch record is durable under BOTH lookup keys
	if ok, _ := svc.st.Get("nip_transfers", "idem:"+req.IdempotencyKey, &NIPTransfer{}); !ok {
		t.Fatal("pre-dispatch idempotency record must exist before rail dispatch")
	}
	if ok, _ := svc.st.Get("nip_transfers", a.SessionID, &NIPTransfer{}); !ok {
		t.Fatal("pre-dispatch session record must exist before rail dispatch")
	}

	// client retry while in flight: same record, NO second rail call
	b, err := svc.Payout(req)
	if err != nil {
		t.Fatal(err)
	}
	if b.SessionID != a.SessionID || b.Status != NIPStatusInFlight {
		t.Fatalf("retry must return the in-flight record, got %+v", b)
	}
	if rail.dispatches != 1 {
		t.Fatalf("retry during in-flight must not re-dispatch: %d rail calls", rail.dispatches)
	}

	// the TSQ sweeper adopts the in-flight record and resolves it
	n, err := svc.SweepTSQ()
	if err != nil || n != 1 {
		t.Fatalf("sweeper adoption: n=%d err=%v", n, err)
	}
	c, err := svc.Payout(req)
	if err != nil {
		t.Fatal(err)
	}
	if c.Status == NIPStatusInFlight || c.SessionID != a.SessionID {
		t.Fatalf("post-sweep retry must return the resolved record, got %+v", c)
	}
	if rail.dispatches != 1 {
		t.Fatalf("still exactly one rail dispatch expected, got %d", rail.dispatches)
	}
}

// 19. An idempotency key reused with a DIFFERENT payload is rejected, not
// silently bound to the original transfer.
func TestPayoutIdempotencyKeyConflict(t *testing.T) {
	svc := nipTestService(t, NewNIPSim(), true)
	req := payoutReq("0123456789")
	req.IdempotencyKey = "idem-conflict"
	if _, err := svc.Payout(req); err != nil {
		t.Fatal(err)
	}
	req.AmountKobo = 999_999 // same key, different amount
	if _, err := svc.Payout(req); err == nil {
		t.Fatal("idempotency-key reuse with a different payload must be rejected")
	}
}
