package controlplane

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
)

// File names used inside Options.StateDir.
const (
	agentIDFile   = "agent.id"
	agentKeyFile  = "agent.key"
	agentCertFile = "agent.crt"
	agentCAFile   = "agents-ca.crt"
)

// certificateCommonPrefix is the CN prefix assigned by the control plane.
const certificateCommonPrefix = "wasmgate-agent:"

// guidPattern matches the canonical dashed GUID form used as an agent id.
var guidPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// identity is the enrolled agent: its id and the mTLS client certificate.
type identity struct {
	AgentID     string
	Certificate tls.Certificate
	CAPEM       []byte
}

// errIncompleteIdentity reports a state directory where only some of the
// identity files exist, for example after a crash during enrollment.
var errIncompleteIdentity = errors.New("control plane: incomplete agent identity")

// loadIdentity reads the enrolled identity from dir. It returns (nil, nil) when
// the agent has not enrolled yet.
func loadIdentity(dir string) (*identity, error) {
	idPath := filepath.Join(dir, agentIDFile)
	certPath := filepath.Join(dir, agentCertFile)
	keyPath := filepath.Join(dir, agentKeyFile)

	switch {
	case !fileExists(idPath) && !fileExists(certPath) && !fileExists(keyPath):
		return nil, nil
	case !fileExists(idPath) || !fileExists(certPath) || !fileExists(keyPath):
		return nil, errIncompleteIdentity
	}

	rawID, err := os.ReadFile(idPath) // #nosec G304 -- operator-configured state dir
	if err != nil {
		return nil, fmt.Errorf("control plane: read %s: %w", idPath, err)
	}

	agentID := strings.TrimSpace(string(rawID))
	if !guidPattern.MatchString(agentID) {
		return nil, fmt.Errorf("control plane: %s does not contain a GUID", idPath)
	}

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("control plane: load certificate: %w", err)
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("control plane: parse certificate: %w", err)
	}
	if want := certificateCommonName(agentID); leaf.Subject.CommonName != want {
		return nil, fmt.Errorf(
			"control plane: certificate CN is %q, want %q",
			leaf.Subject.CommonName, want)
	}

	caPEM, err := os.ReadFile(filepath.Join(dir, agentCAFile)) // #nosec G304
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("control plane: read agent CA: %w", err)
	}

	return &identity{AgentID: agentID, Certificate: cert, CAPEM: caPEM}, nil
}

// saveIdentity writes the identity files, creating dir when needed.
func saveIdentity(dir, agentID string, keyPEM, certPEM, caPEM []byte) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("control plane: create state dir: %w", err)
	}

	files := []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{agentKeyFile, keyPEM, 0o600},
		{agentCertFile, certPEM, 0o644},
		{agentCAFile, caPEM, 0o644},
		{agentIDFile, []byte(agentID + "\n"), 0o644},
	}

	for _, f := range files {
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, f.data, f.mode); err != nil { // #nosec G306 -- certificates and ids are public
			return fmt.Errorf("control plane: write %s: %w", path, err)
		}
	}
	return nil
}

// newAgentID returns a random RFC 4122 version 4 GUID in canonical form.
func newAgentID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("control plane: generate agent id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	return fmt.Sprintf(
		"%x-%x-%x-%x-%x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// existingAgentID returns the agent id stored in dir, if any.
func existingAgentID(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, agentIDFile)) // #nosec G304
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(raw))
	if !guidPattern.MatchString(id) {
		return ""
	}
	return id
}

// newKeyAndCSR creates an RSA private key and a PEM CSR whose CN carries the
// agent id, which is what the control plane signs.
func newKeyAndCSR(agentID string) (keyPEM, csrPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, fmt.Errorf("control plane: generate key: %w", err)
	}

	der, err := x509.CreateCertificateRequest(
		rand.Reader,
		&x509.CertificateRequest{
			Subject:            pkix.Name{CommonName: certificateCommonName(agentID)},
			SignatureAlgorithm: x509.SHA256WithRSA,
		},
		key)
	if err != nil {
		return nil, nil, fmt.Errorf("control plane: create CSR: %w", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("control plane: marshal key: %w", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}),
		nil
}

// certificateCommonName returns the subject CN for an agent id.
func certificateCommonName(agentID string) string {
	return certificateCommonPrefix + agentID
}

// buildVersion reports the module version, or "dev" for a local build.
func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	return "dev"
}

// fileExists reports whether path exists and is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
