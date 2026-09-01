package otelx

// tenant.go — tenant attribute extraction. Canonical span attribute is
// tenant.id (DESIGN-CONTRACT.md). Resolution order:
//  1. X-Meridian-Tenant request header (edge-canonical)
//  2. X-Tenant-ID request header (legacy in-repo convention)
//  3. tenant_id claim of the bearer JWT (unverified decode — telemetry only,
//     never an authz decision)
//  4. baggage entry tenant.id (inbound propagation)

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
)

// TenantKey is the canonical attribute/baggage key.
const TenantKey = "tenant.id"

// TenantAttr builds the tenant.id attribute (empty values are dropped by
// callers; attribute.Value rejects nothing but we keep spans clean).
func TenantAttr(tenant string) attribute.KeyValue {
	return attribute.String(TenantKey, tenant)
}

// TenantFromRequest resolves the tenant for an inbound request.
func TenantFromRequest(r *http.Request) string {
	if t := r.Header.Get("X-Meridian-Tenant"); t != "" {
		return t
	}
	if t := r.Header.Get("X-Tenant-ID"); t != "" {
		return t
	}
	if t := tenantFromJWT(r.Header.Get("Authorization")); t != "" {
		return t
	}
	if m := baggage.FromContext(r.Context()).Member(TenantKey); m.Value() != "" {
		return m.Value()
	}
	return ""
}

// tenantFromJWT decodes the payload of a bearer JWT WITHOUT verifying the
// signature and returns the tenant_id claim. Used strictly for telemetry
// labelling; authn/authz remain the auth middleware's job.
func tenantFromJWT(authz string) string {
	if !strings.HasPrefix(authz, "Bearer ") {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(authz, "Bearer "), ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		TenantID string `json:"tenant_id"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.TenantID
}
