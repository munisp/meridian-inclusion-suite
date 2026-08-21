package httpx

import (
	"net/http"
	"testing"
	"time"
)

// QA-04/05/06: the shared server helper must set full timeout defaults.
func TestNewServerTimeouts(t *testing.T) {
	srv := NewServer(":0", http.NewServeMux())
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s", srv.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != 30*time.Second {
		t.Fatalf("ReadTimeout = %s", srv.ReadTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Fatalf("IdleTimeout = %s", srv.IdleTimeout)
	}
	if srv.WriteTimeout != 60*time.Second {
		t.Fatalf("WriteTimeout = %s", srv.WriteTimeout)
	}
}
