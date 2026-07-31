package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/munisp/meridian-inclusion-suite/internal/platform/ids"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/store"
	"github.com/munisp/meridian-inclusion-suite/internal/platform/workflowx"
)

// DocRef is a document reference attached to an onboarding record (audit
// O4/O7). Payloads live in WORM object storage (MinIO with object-lock in
// prod; dev filesystem fallback); only the reference + digest sits on the
// operator record.
type DocRef struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"` // nin_slip|cac_cert|cac_status_report|photo|other
	Filename   string `json:"filename"`
	ObjectKey  string `json:"object_key"`
	SHA256     string `json:"sha256,omitempty"`
	SizeBytes  int64  `json:"size_bytes,omitempty"`
	Status     string `json:"status"` // pending_upload|uploaded|rejected
	WORM       bool   `json:"worm"`   // stored on a retention-locked (WORM) backend
	UploadedAt string `json:"uploaded_at,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// PresignResult tells the client where to PUT the payload.
type PresignResult struct {
	DocID     string            `json:"doc_id"`
	UploadURL string            `json:"upload_url"`
	Method    string            `json:"method"` // PUT
	Headers   map[string]string `json:"headers,omitempty"`
	ExpiresAt string            `json:"expires_at"`
	Backend   string            `json:"backend"` // minio_worm|dev_fs
}

// DocBackend issues presigned upload URLs and serves dev uploads.
type DocBackend interface {
	Presign(doc DocRef) (PresignResult, error)
	Name() string
	WORM() bool
}

// --- dev filesystem fallback (profile=dev only) ---

// fsDocBackend stores payloads under <dir>/docs and hands out same-service
// upload URLs (PUT /v1/docs/upload/{token}), HMAC-signed, 15-min expiry.
type fsDocBackend struct {
	dir    string
	secret string
}

func newFSDocBackend(dir string) *fsDocBackend {
	return &fsDocBackend{dir: filepath.Join(dir, "docs"), secret: hmacSHA256Hex(docUploadKey(), "fs-backend")}
}

func docUploadKey() string {
	return os.Getenv("DOC_UPLOAD_HMAC_KEY") // dev default derived below when empty
}

func (f *fsDocBackend) Name() string { return "dev_fs" }
func (f *fsDocBackend) WORM() bool   { return false }

func (f *fsDocBackend) token(docID string, exp int64) string {
	return hmacSHA256Hex(f.secret+"|"+docUploadKey(), fmt.Sprintf("%s|%d", docID, exp))
}

func (f *fsDocBackend) Presign(doc DocRef) (PresignResult, error) {
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		return PresignResult{}, err
	}
	exp := time.Now().Add(15 * time.Minute).Unix()
	tok := f.token(doc.ID, exp)
	return PresignResult{
		DocID: doc.ID, Method: http.MethodPut,
		UploadURL: fmt.Sprintf("/v1/docs/upload/%s?exp=%d&sig=%s", doc.ID, exp, tok),
		ExpiresAt: time.Unix(exp, 0).UTC().Format(time.RFC3339),
		Backend:   f.Name(),
	}, nil
}

// serveUpload handles PUT /v1/docs/upload/{doc} for the dev FS backend,
// verifying the HMAC token + expiry, writing the payload and returning the
// sha256 so the client can complete the doc ref.
func (f *fsDocBackend) serveUpload(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("doc")
	exp, sig := r.URL.Query().Get("exp"), r.URL.Query().Get("sig")
	var expUnix int64
	fmt.Sscan(exp, &expUnix)
	if sig == "" || !hmac.Equal([]byte(sig), []byte(f.token(docID, expUnix))) {
		http.Error(w, "bad upload signature", http.StatusForbidden)
		return
	}
	if time.Now().Unix() > expUnix {
		http.Error(w, "upload url expired", http.StatusGone)
		return
	}
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 20<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256(body)
	path := filepath.Join(f.dir, docID+".bin")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"doc_id":%q,"sha256":%q,"size_bytes":%d}`, docID, hex.EncodeToString(sum[:]), len(body))
}

// --- MinIO WORM backend (S3 SigV4 presigned PUT, profile=prod) ---

// minioDocBackend presigns PUT URLs against a MinIO/S3 endpoint. The target
// bucket is expected to have object-lock (WORM) enabled; that is asserted by
// config (MINIO_WORM_BUCKET) and surfaced on every DocRef.
type minioDocBackend struct {
	endpoint  string // http(s)://host[:port]
	region    string
	bucket    string
	accessKey string
	secretKey string
	worm      bool
}

func newMinioDocBackendFromEnv() *minioDocBackend {
	return &minioDocBackend{
		endpoint:  strings.TrimRight(os.Getenv("MINIO_ENDPOINT"), "/"),
		region:    envOr("MINIO_REGION", "us-east-1"),
		bucket:    os.Getenv("MINIO_BUCKET"),
		accessKey: os.Getenv("MINIO_ACCESS_KEY"),
		secretKey: os.Getenv("MINIO_SECRET_KEY"),
		worm:      os.Getenv("MINIO_WORM_BUCKET") == "true",
	}
}

func (m *minioDocBackend) Name() string { return "minio_worm" }
func (m *minioDocBackend) WORM() bool   { return m.worm }

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// Presign builds an S3 SigV4 presigned PUT URL (15 min expiry). Payloads are
// immutable: the object key is content-addressed by doc id and never
// overwritten by clients (object-lock enforces WORM at the bucket level).
func (m *minioDocBackend) Presign(doc DocRef) (PresignResult, error) {
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	expires := "900"
	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, m.region)
	host := strings.TrimPrefix(strings.TrimPrefix(m.endpoint, "https://"), "http://")

	query := map[string]string{
		"X-Amz-Algorithm":     "AWS4-HMAC-SHA256",
		"X-Amz-Credential":    m.accessKey + "/" + scope,
		"X-Amz-Date":          amzDate,
		"X-Amz-Expires":       expires,
		"X-Amz-SignedHeaders": "host",
	}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonQuery strings.Builder
	for i, k := range keys {
		if i > 0 {
			canonQuery.WriteByte('&')
		}
		canonQuery.WriteString(uriEncode(k, true) + "=" + uriEncode(query[k], true))
	}
	uri := "/" + m.bucket + "/" + doc.ObjectKey
	canonical := strings.Join([]string{
		http.MethodPut, uri, canonQuery.String(),
		"host:" + host + "\n", "host", "UNSIGNED-PAYLOAD",
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonical)),
	}, "\n")
	kDate := hmacSHA256([]byte("AWS4"+m.secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, m.region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	return PresignResult{
		DocID: doc.ID, Method: http.MethodPut,
		UploadURL: fmt.Sprintf("%s%s?%s&X-Amz-Signature=%s", m.endpoint, uri, canonQuery.String(), sig),
		ExpiresAt: now.Add(15 * time.Minute).Format(time.RFC3339),
		Backend:   m.Name(),
	}, nil
}

func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else if c == '/' && !encodeSlash {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// NewDocBackendFromEnv selects the document backend per profile (fail-closed
// in prod: MINIO_ENDPOINT + MINIO_BUCKET + credentials required).
func NewDocBackendFromEnv(dataDir string) (DocBackend, error) {
	if os.Getenv("MINIO_ENDPOINT") != "" {
		m := newMinioDocBackendFromEnv()
		if m.bucket == "" || m.accessKey == "" || m.secretKey == "" {
			return nil, fmt.Errorf("MINIO_ENDPOINT set but MINIO_BUCKET/MINIO_ACCESS_KEY/MINIO_SECRET_KEY missing")
		}
		log.Printf("profile=prod component=doc-backend backend=minio bucket=%s worm=%v", m.bucket, m.worm)
		return m, nil
	}
	if workflowx.IsProdProfile() {
		return nil, fmt.Errorf("profile=prod FATAL: MINIO_ENDPOINT (+BUCKET/keys) is required for WORM document storage (no dev FS fallback)")
	}
	log.Printf("profile=dev component=doc-backend backend=fs dir=%s", dataDir)
	return newFSDocBackend(dataDir), nil
}

// DocService manages document references on onboarding records.
type DocService struct {
	st       *store.Store
	registry *Registry
	backend  DocBackend
}

func NewDocService(st *store.Store, reg *Registry, b DocBackend) *DocService {
	return &DocService{st: st, registry: reg, backend: b}
}

var docKinds = map[string]bool{
	"nin_slip": true, "cac_cert": true, "cac_status_report": true, "photo": true, "other": true,
}

// Presign registers a pending doc ref and returns the upload URL.
func (d *DocService) Presign(operatorID, kind, filename string) (PresignResult, error) {
	op, ok, err := d.registry.Get(operatorID)
	if err != nil || !ok {
		return PresignResult{}, fmt.Errorf("operator not found")
	}
	if !docKinds[kind] {
		return PresignResult{}, fmt.Errorf("kind must be one of nin_slip|cac_cert|cac_status_report|photo|other")
	}
	doc := DocRef{
		ID: ids.WithPrefix("doc"), Kind: kind, Filename: filename,
		Status: "pending_upload", WORM: d.backend.WORM(), CreatedAt: nowRFC3339(),
	}
	doc.ObjectKey = fmt.Sprintf("onboarding/%s/%s", op.ID, doc.ID)
	res, err := d.backend.Presign(doc)
	if err != nil {
		return PresignResult{}, err
	}
	if err := d.st.Put("docs", doc.ID, doc); err != nil {
		return PresignResult{}, err
	}
	return res, nil
}

// Complete marks a doc uploaded (after the client PUT to the presigned URL)
// and attaches the reference to the onboarding record.
func (d *DocService) Complete(operatorID, docID, sha256sum string, size int64) (DocRef, error) {
	var doc DocRef
	ok, err := d.st.Get("docs", docID, &doc)
	if err != nil || !ok {
		return DocRef{}, fmt.Errorf("doc not found")
	}
	if doc.Status != "pending_upload" {
		return doc, nil // idempotent
	}
	doc.Status = "uploaded"
	doc.SHA256 = sha256sum
	doc.SizeBytes = size
	doc.UploadedAt = nowRFC3339()
	if err := d.st.Put("docs", doc.ID, doc); err != nil {
		return DocRef{}, err
	}
	op, ok, err := d.registry.Get(operatorID)
	if err != nil || !ok {
		return DocRef{}, fmt.Errorf("operator not found")
	}
	replaced := false
	for i, r := range op.Documents {
		if r.ID == doc.ID {
			op.Documents[i] = doc
			replaced = true
		}
	}
	if !replaced {
		op.Documents = append(op.Documents, doc)
	}
	return doc, d.registry.Update(op)
}

// List returns doc refs for an onboarding record.
func (d *DocService) List(operatorID string) []DocRef {
	op, ok, err := d.registry.Get(operatorID)
	if err != nil || !ok {
		return []DocRef{}
	}
	return op.Documents
}

// OnboardingStatus is the resumption view (audit O4): where the subject is
// in the flow and what is still missing, so an interrupted onboarding (PWA
// or USSD) can resume at the right step.
type OnboardingStatus struct {
	OperatorID   string   `json:"operator_id"`
	Status       string   `json:"status"`
	CurrentStep  string   `json:"current_step"`
	MissingItems []string `json:"missing_items"`
	Documents    []DocRef `json:"documents"`
	ReviewStatus string   `json:"review_status,omitempty"`
	TINHash      string   `json:"tin_hash,omitempty"`
	NextActions  []string `json:"next_actions"`
}

// Status computes the resumption view for an operator.
func (d *DocService) Status(op Operator) OnboardingStatus {
	st := OnboardingStatus{
		OperatorID: op.ID, Status: op.Status, Documents: d.List(op.ID),
		ReviewStatus: op.ReviewStatus, TINHash: op.TINHash,
	}
	missing := []string{}
	if op.ConsentID == "" {
		missing = append(missing, "ndpa_consent")
	}
	hasNINSlip := false
	for _, doc := range st.Documents {
		if doc.Kind == "nin_slip" && doc.Status == "uploaded" {
			hasNINSlip = true
		}
	}
	switch op.Status {
	case "registered":
		st.CurrentStep = "identity_verification"
		missing = append(missing, "nimc_verification")
		if !hasNINSlip {
			missing = append(missing, "doc:nin_slip")
		}
		st.NextActions = []string{"POST /v1/tin/provision", "POST /v1/operators/{id}/documents/presign"}
	case "pending_review":
		st.CurrentStep = "review"
		missing = append(missing, "review_decision")
		st.NextActions = []string{"GET /v1/onboarding/{id} (poll)", "contact NRS agent"}
	case "nin_verified":
		st.CurrentStep = "tin_provisioning"
		missing = append(missing, "tin_provision")
		st.NextActions = []string{"POST /v1/tin/provision"}
	case "tin_provisioned":
		st.CurrentStep = "active"
		st.NextActions = []string{"file presumptive levy", "graduate to MBS above threshold"}
	case "graduated":
		st.CurrentStep = "graduated_mbs"
	case "rejected":
		st.CurrentStep = "rejected"
		st.NextActions = []string{"re-onboard via POST /v1/operators"}
	}
	st.MissingItems = missing
	return st
}
