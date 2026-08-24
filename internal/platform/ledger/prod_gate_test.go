package ledger

import (
	"os"
	"os/exec"
	"testing"
)

// Regression (B3 #4 repair, V2 round): NewClientFromEnv silently fell back
// to the volatile in-memory dev ledger even under PROFILE=prod — a prod
// restart would lose all financial state. Prod now refuses to boot without
// LEDGER_URL (log.Fatal). Verified via a subprocess because the gate exits.

func TestProdGateRefusesBootWithoutLedgerURL(t *testing.T) {
	if os.Getenv("GO_WANT_LEDGER_GATE_HELPER") == "1" {
		os.Unsetenv("LEDGER_URL")
		os.Setenv("PROFILE", "prod")
		NewClientFromEnv() // must log.Fatal before returning
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestProdGateRefusesBootWithoutLedgerURL")
	cmd.Env = append(os.Environ(), "GO_WANT_LEDGER_GATE_HELPER=1")
	err := cmd.Run()
	if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 1 {
		t.Fatalf("helper exit = %v, want exit code 1 (log.Fatal boot refusal)", err)
	}
}

func TestDevProfileStillGetsDevClient(t *testing.T) {
	t.Setenv("LEDGER_URL", "")
	t.Setenv("PROFILE", "dev")
	c := NewClientFromEnv()
	if c == nil {
		t.Fatal("dev profile must fall back to the in-memory dev client")
	}
	if _, ok := c.(*HTTPClient); ok {
		t.Fatal("dev profile without LEDGER_URL must NOT get an HTTP client")
	}
}

func TestProdWithLedgerURLGetsHTTPClient(t *testing.T) {
	t.Setenv("LEDGER_URL", "http://127.0.0.1:1")
	t.Setenv("PROFILE", "prod")
	c := NewClientFromEnv()
	if _, ok := c.(*HTTPClient); !ok {
		t.Fatalf("prod with LEDGER_URL must get an HTTP client, got %T", c)
	}
}
