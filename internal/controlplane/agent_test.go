package controlplane

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"wasmgate/internal/config"
)

type testReloader struct{}

func (testReloader) Reload(context.Context, *config.Config) (uint64, error) { return 1, nil }

func TestNewAgentRejectsNilReloader(t *testing.T) {
	if _, err := NewAgent(Options{}, nil, nil); err == nil {
		t.Fatal("NewAgent accepted a nil reloader")
	}
}

func TestNewAgentRunStopsOnCancelledContext(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "release.pub")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	agent, err := NewAgent(Options{
		Endpoint:   "http://127.0.0.1:1",
		Insecure:   true,
		ConfigPath: filepath.Join(dir, "wasmgate.yaml"),
		StateDir:   filepath.Join(dir, "state"),
		ReleaseKey: keyPath,
	}, testReloader{}, nil)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := agent.Run(ctx); err != nil {
		t.Fatalf("Run on cancelled context: %v", err)
	}
}
