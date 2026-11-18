package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadIdentityIncompleteState(t *testing.T) {
	dir := t.TempDir()
	if id, err := Load(dir); id != nil || err != nil {
		t.Fatalf("empty dir: id=%v err=%v", id, err)
	}
	if err := os.WriteFile(filepath.Join(dir, agentIDFile), []byte("6f1c9f7e-2b1a-4f3c-9d8e-7a6b5c4d3e2f\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); !errors.Is(err, ErrIncompleteIdentity) {
		t.Fatalf("partial identity error = %v", err)
	}
}

func TestSaveLoadIdentity(t *testing.T) {
	const agentID = "6f1c9f7e-2b1a-4f3c-9d8e-7a6b5c4d3e2f"
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		Subject:      pkix.Name{CommonName: certificateCommonName(agentID)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	caPEM := []byte("test-ca")

	if err := Save(dir, agentID, keyPEM, certPEM, caPEM); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := ExistingAgentID(dir); got != agentID {
		t.Fatalf("ExistingAgentID = %q", got)
	}
	id, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if id == nil || id.AgentID != agentID || string(id.CAPEM) != string(caPEM) || id.NotAfter.IsZero() {
		t.Fatalf("Load = %+v", id)
	}
}

func TestNewAgentIDIsGUID(t *testing.T) {
	a, err := NewAgentID()
	if err != nil || !guidPattern.MatchString(a) {
		t.Fatalf("NewAgentID = %q, %v", a, err)
	}
	b, err := NewAgentID()
	if err != nil || a == b {
		t.Fatalf("agent ids are not unique: %q %q", a, b)
	}
}

func TestNewKeyAndCSR(t *testing.T) {
	keyPEM, csrPEM, err := NewKeyAndCSR("6f1c9f7e-2b1a-4f3c-9d8e-7a6b5c4d3e2f")
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" {
		t.Fatalf("key block = %+v", keyBlock)
	}
	if _, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err != nil {
		t.Fatal(err)
	}
	csrBlock, _ := pem.Decode(csrPEM)
	if csrBlock == nil || csrBlock.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("csr block = %+v", csrBlock)
	}
	csr, err := x509.ParseCertificateRequest(csrBlock.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		t.Fatalf("CSR invalid: %v", err)
	}
	if got := csr.Subject.CommonName; got != "wasmgate-agent:6f1c9f7e-2b1a-4f3c-9d8e-7a6b5c4d3e2f" {
		t.Fatalf("CN = %q", got)
	}
}

func TestEnsureLocalIDCreatesOnlyAgentID(t *testing.T) {
	dir := t.TempDir()
	first, err := EnsureLocalID(dir, "")
	if err != nil {
		t.Fatalf("EnsureLocalID: %v", err)
	}
	if !guidPattern.MatchString(first.AgentID) {
		t.Fatalf("agent id = %q", first.AgentID)
	}
	for _, name := range []string{agentIDFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	for _, name := range []string{agentKeyFile, agentCertFile, agentCAFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("local mode created %s (err=%v)", name, err)
		}
	}

	second, err := EnsureLocalID(dir, "11111111-1111-1111-1111-111111111111")
	if err != nil || second.AgentID != first.AgentID {
		t.Fatalf("stable id = %q, %v (want %q)", second.AgentID, err, first.AgentID)
	}

	requested := "22222222-2222-2222-2222-222222222222"
	requestedID, err := EnsureLocalID(t.TempDir(), requested)
	if err != nil || requestedID.AgentID != requested {
		t.Fatalf("requested id = %q, %v", requestedID.AgentID, err)
	}
}
