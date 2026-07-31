// Package keyx provides the platform key-management abstraction (HARDENING:
// HSM/KMS). Keys are resolved through a KeyProvider chain:
//
//	1. environment variable (mounted secret env)
//	2. file provider (KEY_FILE_DIR/<NAME>, mounted secret volume)
//	3. dev default — ONLY when profile=dev, and always logged
//
// In profile=prod (APP_PROFILE=prod|production, or AUTH_MODE=keycloak) a
// missing key fails closed with a clear error; no public dev default is ever
// used. A KMS stub provider is included for wiring against a real KMS/HSM
// later (SIMULATED — logs and delegates to env/file).
package keyx

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Provider resolves a named key to its secret material.
type Provider interface {
	// Key returns the secret for name, or an error if unavailable.
	Key(name string) (string, error)
}

// EnvProvider resolves keys from environment variables.
type EnvProvider struct{}

func (p EnvProvider) Key(name string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("env var %s not set", name)
}

// FileProvider resolves keys from files in a directory (mounted secrets).
type FileProvider struct{ Dir string }

func (p FileProvider) Key(name string) (string, error) {
	if p.Dir == "" {
		return "", fmt.Errorf("KEY_FILE_DIR not set")
	}
	b, err := os.ReadFile(filepath.Join(p.Dir, name))
	if err != nil {
		return "", fmt.Errorf("read key file %s: %w", name, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("key file %s is empty", name)
	}
	return v, nil
}

// KMSStubProvider is a stand-in for a real KMS/HSM-backed provider. It logs
// every key access (as an HSM would audit) and delegates to the fallback
// chain. SIMULATED: replace with a KMS client (envelope decryption) in prod.
type KMSStubProvider struct {
	URI      string // e.g. kms://vault/meridian (audit label only)
	Fallback Provider
}

func (p KMSStubProvider) Key(name string) (string, error) {
	log.Printf("component=keyx provider=kms-stub uri=%s key=%s action=resolve (SIMULATED)", p.URI, name)
	if p.Fallback == nil {
		return "", fmt.Errorf("kms-stub has no fallback provider")
	}
	return p.Fallback.Key(name)
}

// ChainProvider tries each provider in order.
type ChainProvider []Provider

func (c ChainProvider) Key(name string) (string, error) {
	var errs []string
	for _, p := range c {
		if v, err := p.Key(name); err == nil {
			return v, nil
		} else {
			errs = append(errs, err.Error())
		}
	}
	return "", fmt.Errorf("no provider could resolve key: %s", strings.Join(errs, "; "))
}

// Profile returns the deployment profile: APP_PROFILE / MERIDIAN_PROFILE,
// default "dev". AUTH_MODE=keycloak also implies prod.
func Profile() string {
	p := strings.ToLower(os.Getenv("APP_PROFILE"))
	if p == "" {
		p = strings.ToLower(os.Getenv("MERIDIAN_PROFILE"))
	}
	if p == "" && strings.EqualFold(os.Getenv("AUTH_MODE"), "keycloak") {
		return "prod"
	}
	if p == "" {
		return "dev"
	}
	return p
}

// Prod reports whether the profile requires fail-closed key resolution.
func Prod() bool { return Profile() == "prod" || Profile() == "production" }

var defaultProvider Provider = ChainProvider{EnvProvider{}, FileProvider{Dir: os.Getenv("KEY_FILE_DIR")}}
var mu sync.RWMutex

// SetProvider overrides the process-wide provider (prod wiring / tests).
func SetProvider(p Provider) {
	mu.Lock()
	defer mu.Unlock()
	defaultProvider = p
}

// DefaultProvider returns the process-wide provider.
func DefaultProvider() Provider {
	mu.RLock()
	defer mu.RUnlock()
	return defaultProvider
}

var devWarnOnce sync.Map

// Key resolves name via the provider chain. In prod profile a missing key is
// a hard error (fail closed). In dev, devDefault is returned with a logged
// warning when nothing else resolves.
func Key(name, devDefault string) (string, error) {
	if v, err := DefaultProvider().Key(name); err == nil {
		return v, nil
	}
	if Prod() {
		return "", fmt.Errorf("keyx: %s is required in profile=prod (set env var or mount KEY_FILE_DIR/%s); refusing insecure dev default", name, name)
	}
	if devDefault == "" {
		return "", fmt.Errorf("keyx: %s not resolvable and no dev default available", name)
	}
	devWarnOnce.LoadOrStore(name, func() struct{} {
		log.Printf("profile=dev component=keyx key=%s source=insecure-dev-default (DO NOT deploy with profile=dev)", name)
		return struct{}{}
	})
	return devDefault, nil
}

// MustKey is Key but fatal in prod when unresolvable (startup fail-closed).
func MustKey(name, devDefault string) string {
	v, err := Key(name, devDefault)
	if err != nil {
		log.Fatalf("keyx: %v", err)
	}
	return v
}
