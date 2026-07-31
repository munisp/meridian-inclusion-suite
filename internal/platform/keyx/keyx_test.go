package keyx

import (
	"os"
	"path/filepath"
	"testing"
)

func reset(t *testing.T) {
	t.Helper()
	SetProvider(ChainProvider{EnvProvider{}, FileProvider{Dir: os.Getenv("KEY_FILE_DIR")}})
	t.Setenv("APP_PROFILE", "")
	t.Setenv("MERIDIAN_PROFILE", "")
	t.Setenv("AUTH_MODE", "")
}

func TestDevDefaultAllowedInDevProfile(t *testing.T) {
	reset(t)
	v, err := Key("TEST_KEYX_MISSING", "dev-value")
	if err != nil || v != "dev-value" {
		t.Fatalf("dev profile should allow dev default, got %q err=%v", v, err)
	}
}

func TestProdFailsClosedWhenUnset(t *testing.T) {
	reset(t)
	t.Setenv("APP_PROFILE", "prod")
	if _, err := Key("TEST_KEYX_MISSING", "dev-value"); err == nil {
		t.Fatal("profile=prod must fail closed when key is unset")
	}
}

func TestProdUsesEnvWhenSet(t *testing.T) {
	reset(t)
	t.Setenv("APP_PROFILE", "prod")
	t.Setenv("TEST_KEYX_SET", "real-secret")
	v, err := Key("TEST_KEYX_SET", "dev-value")
	if err != nil || v != "real-secret" {
		t.Fatalf("env provider failed: %q %v", v, err)
	}
}

func TestKeycloakModeImpliesProd(t *testing.T) {
	reset(t)
	t.Setenv("AUTH_MODE", "keycloak")
	if !Prod() {
		t.Fatal("AUTH_MODE=keycloak must imply prod profile")
	}
}

func TestFileProvider(t *testing.T) {
	reset(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "TEST_FILE_KEY"), []byte("file-secret\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	SetProvider(ChainProvider{EnvProvider{}, FileProvider{Dir: dir}})
	v, err := Key("TEST_FILE_KEY", "")
	if err != nil || v != "file-secret" {
		t.Fatalf("file provider failed: %q %v", v, err)
	}
}

func TestKMSStubDelegatesAndAudits(t *testing.T) {
	reset(t)
	t.Setenv("TEST_KMS_KEY", "via-env")
	SetProvider(KMSStubProvider{URI: "kms://vault/test", Fallback: EnvProvider{}})
	v, err := Key("TEST_KMS_KEY", "")
	if err != nil || v != "via-env" {
		t.Fatalf("kms stub failed: %q %v", v, err)
	}
}
