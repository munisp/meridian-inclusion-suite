package store

// Construction-proof tests for the R4-9b unique-violation plumbing (no
// live Postgres required): SQLSTATE 23505 classification and the idempotent
// DDL carrying the levy uniqueness index.

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsUniqueViolationClassification(t *testing.T) {
	uv := &pgconn.PgError{Code: "23505", ConstraintName: "meridian_payments_levy_uniq"}
	if !IsUniqueViolation(uv) {
		t.Fatal("23505 must classify as unique violation")
	}
	if !IsUniqueViolation(fmt.Errorf("put payments: %w", uv)) {
		t.Fatal("wrapped 23505 must classify as unique violation")
	}
	for _, code := range []string{"40001", "40P01", "23503", "23502"} {
		if IsUniqueViolation(&pgconn.PgError{Code: code}) {
			t.Fatalf("%s must not classify as unique violation", code)
		}
	}
	if IsUniqueViolation(errors.New("plain error")) || IsUniqueViolation(nil) {
		t.Fatal("non-pg errors must not classify as unique violation")
	}
}

// The migration convention of this repo is the auto-applied pgDDL constant;
// pin the R4-9b index definition (idempotent, correct status predicate).
func TestPGDDLCarriesLevyUniqueIndex(t *testing.T) {
	for _, want := range []string{
		"CREATE UNIQUE INDEX IF NOT EXISTS meridian_payments_levy_uniq",
		"(doc->>'tin_hash')",
		"(doc->>'period')",
		"COALESCE(doc->>'tenant_id', '')",
		"COALESCE(doc->>'monthly', 'false')",
		"WHERE collection = 'payments'",
	} {
		if !strings.Contains(pgDDL, want) {
			t.Fatalf("pgDDL missing %q", want)
		}
	}
	for _, status := range []string{"intent", "pending_authorisation", "authorised",
		"captured_awaiting_post", "capture_in_flight", "captured", "settled", "disputed"} {
		if !strings.Contains(pgDDL, "'"+status+"'") {
			t.Fatalf("pgDDL index predicate missing status %q", status)
		}
	}
}
