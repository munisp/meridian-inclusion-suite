package ledger

import "testing"

// FF-7: DevClient.PostPendingAs must reuse the pending transfer's code
// (TigerBeetle post_pending semantics) instead of hardcoding CodeCapture.
func TestPostPendingAsReusesPendingCode(t *testing.T) {
	c := NewDevClient()
	deb := AccountID(LedgerPSSPRecon, 1)
	cred := AccountID(LedgerPSSPRecon, 2)
	if err := c.CreateAccounts([]Account{
		{ID: deb, Ledger: LedgerPSSPRecon},
		{ID: cred, Ledger: LedgerPSSPRecon},
	}); err != nil {
		t.Fatal(err)
	}

	for _, code := range []uint16{CodeHold, CodeSettle, CodeAuthorise} {
		pendingID, err := c.PendingTransfer(Transfer{
			ID:              NewTransferID(),
			DebitAccountID:  deb,
			CreditAccountID: cred,
			Ledger:          LedgerPSSPRecon,
			Code:            code,
			Amount:          1000,
		})
		if err != nil {
			t.Fatalf("pending (code=%d): %v", code, err)
		}
		postID, err := c.PostPendingAs(pendingID, NewTransferID(), 1000)
		if err != nil {
			t.Fatalf("post (code=%d): %v", code, err)
		}
		post, err := c.LookupTransfer(postID)
		if err != nil {
			t.Fatalf("lookup (code=%d): %v", code, err)
		}
		if post.Code != code {
			t.Fatalf("post code = %d, want pending code %d", post.Code, code)
		}
		if post.Pending {
			t.Fatalf("post transfer (code=%d) still pending", code)
		}
	}
}
