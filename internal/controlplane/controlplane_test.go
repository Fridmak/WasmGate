package controlplane

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wasmgate/internal/controlplane/wasmgatepb"
)

const validConfig = `version: 1
listeners:
  - name: edge
    addr: "127.0.0.1:18080"
    upstream_pool: app
pools:
  app:
    servers: ["127.0.0.1:18081"]
`

func TestSafeJoin(t *testing.T) {
	base := t.TempDir()

	ok, err := safeJoin(base, "plugins/ipguard.wasm")
	if err != nil {
		t.Fatalf("safeJoin(valid): %v", err)
	}
	if want := filepath.Join(base, "plugins", "ipguard.wasm"); ok != want {
		t.Fatalf("safeJoin(valid) = %q, want %q", ok, want)
	}

	rejected := []string{
		"",
		"   ",
		"../escape.wasm",
		"a/../../escape.wasm",
		"..\\escape.wasm",
	}
	for _, name := range rejected {
		if _, err := safeJoin(base, name); err == nil {
			t.Fatalf("safeJoin(%q) accepted an unsafe path", name)
		}
	}

	absolute := filepath.Join(base, "abs.wasm")
	if _, err := safeJoin(base, absolute); err == nil {
		t.Fatalf("safeJoin(%q) accepted an absolute path", absolute)
	}
}

func TestPrepareConfig(t *testing.T) {
	dir := t.TempDir()
	agent := &Agent{opts: Options{ConfigPath: filepath.Join(dir, "wasmgate.yaml")}}

	digest := sha256.Sum256([]byte(validConfig))
	cfg, gotDir, err := agent.prepareConfig(&wasmgatepb.ApplyRelease{
		ConfigYaml:   []byte(validConfig),
		ConfigSha256: strings.ToUpper(hex.EncodeToString(digest[:])),
	})
	if err != nil {
		t.Fatalf("prepareConfig: %v", err)
	}
	if cfg.Dir != gotDir || gotDir != dir {
		t.Fatalf("Dir = %q, want %q", cfg.Dir, dir)
	}
	if len(cfg.Listeners) != 1 {
		t.Fatalf("listeners = %d, want 1", len(cfg.Listeners))
	}

	if _, _, err := agent.prepareConfig(&wasmgatepb.ApplyRelease{
		ConfigYaml:   []byte(validConfig),
		ConfigSha256: strings.Repeat("0", 64),
	}); err == nil {
		t.Fatal("prepareConfig accepted a wrong SHA-256")
	}

	if _, _, err := agent.prepareConfig(&wasmgatepb.ApplyRelease{
		ConfigYaml: []byte("version: 99\n"),
	}); err == nil {
		t.Fatal("prepareConfig accepted an invalid document")
	}

	if _, _, err := agent.prepareConfig(&wasmgatepb.ApplyRelease{}); err == nil {
		t.Fatal("prepareConfig accepted an empty document")
	}
}

func TestWriteFileAtomicReplacesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "wasmgate.yaml")

	if err := writeFileAtomic(path, []byte("first")); err != nil {
		t.Fatalf("writeFileAtomic(first): %v", err)
	}
	if err := writeFileAtomic(path, []byte("second")); err != nil {
		t.Fatalf("writeFileAtomic(second): %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "second" {
		t.Fatalf("content = %q, want %q", got, "second")
	}
}

func TestLoadIdentityIncompleteState(t *testing.T) {
	dir := t.TempDir()

	id, err := loadIdentity(dir)
	if id != nil || err != nil {
		t.Fatalf("empty dir: id=%v err=%v", id, err)
	}

	if err := os.WriteFile(
		filepath.Join(dir, agentIDFile),
		[]byte("6f1c9f7e-2b1a-4f3c-9d8e-7a6b5c4d3e2f\n"),
		0o644); err != nil {
		t.Fatalf("write agent id: %v", err)
	}

	if _, err := loadIdentity(dir); !errors.Is(err, errIncompleteIdentity) {
		t.Fatalf("partial identity error = %v, want errIncompleteIdentity", err)
	}
}

func TestAppliedStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got := loadApplied(dir); got != (appliedState{}) {
		t.Fatalf("loadApplied(empty) = %+v", got)
	}

	want := appliedState{ReleaseID: "release-1", ConfigSHA256: "abc", Version: 7}
	if err := saveApplied(dir, want); err != nil {
		t.Fatalf("saveApplied: %v", err)
	}
	if got := loadApplied(dir); got != want {
		t.Fatalf("loadApplied = %+v, want %+v", got, want)
	}
}

func TestNewAgentIDIsGUID(t *testing.T) {
	id, err := newAgentID()
	if err != nil {
		t.Fatalf("newAgentID: %v", err)
	}
	if !guidPattern.MatchString(id) {
		t.Fatalf("newAgentID = %q, which is not a GUID", id)
	}
	if id == mustAgentID(t) {
		t.Fatal("newAgentID returned the same id twice")
	}
}

func TestNewKeyAndCSR(t *testing.T) {
	keyPEM, csrPEM, err := newKeyAndCSR("6f1c9f7e-2b1a-4f3c-9d8e-7a6b5c4d3e2f")
	if err != nil {
		t.Fatalf("newKeyAndCSR: %v", err)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		t.Fatalf("key block = %+v", keyBlock)
	}
	if _, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err != nil {
		t.Fatalf("parse key: %v", err)
	}

	csrBlock, _ := pem.Decode(csrPEM)
	if csrBlock == nil || csrBlock.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("csr block = %+v", csrBlock)
	}
	csr, err := x509.ParseCertificateRequest(csrBlock.Bytes)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature: %v", err)
	}
	if want := "wasmgate-agent:6f1c9f7e-2b1a-4f3c-9d8e-7a6b5c4d3e2f"; csr.Subject.CommonName != want {
		t.Fatalf("CN = %q, want %q", csr.Subject.CommonName, want)
	}
}

func TestOptionsNormalize(t *testing.T) {
	opts := Options{Endpoint: "http://127.0.0.1:9096", Insecure: true, ConfigPath: "wasmgate.yaml"}
	if err := opts.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if opts.Heartbeat != DefaultHeartbeat {
		t.Fatalf("Heartbeat = %v, want %v", opts.Heartbeat, DefaultHeartbeat)
	}
	if len(opts.AbiVersions) == 0 || opts.Name == "" || opts.StateDir == "" {
		t.Fatalf("defaults were not applied: %+v", opts)
	}

	if err := (&Options{Endpoint: "http://127.0.0.1:9096", ConfigPath: "x.yaml"}).normalize(); err == nil {
		t.Fatal("normalize accepted http:// without Insecure")
	}
	if err := (&Options{Endpoint: "ftp://host:1", ConfigPath: "x.yaml"}).normalize(); err == nil {
		t.Fatal("normalize accepted an unsupported scheme")
	}
}

// mustAgentID returns a second id so the uniqueness check in
// TestNewAgentIDIsGUID compares two generated values.
func mustAgentID(t *testing.T) string {
	t.Helper()
	id, err := newAgentID()
	if err != nil {
		t.Fatalf("newAgentID: %v", err)
	}
	return id
}

func TestAgentTargetStripsScheme(t *testing.T) {
	agent := &Agent{opts: Options{Endpoint: "https://cp.example.com:9096"}}
	if got := agent.target(); got != "cp.example.com:9096" {
		t.Fatalf("target = %q", got)
	}
}
