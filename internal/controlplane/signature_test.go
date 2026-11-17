package controlplane

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wasmgate/internal/controlplane/wasmgatepb"
)

const testManifest = `wasmgate-release-v1
3f1d4c9e-0a2b-4c5d-8e9f-1a2b3c4d5e6f
prod
7c2e5a10-1b3c-4d5e-8f90-2a3b4c5d6e7f
2f7c9a1b3d5e7f9012345678abcdef01
plugins/ipguard.wasm
3e4e631671e2774007daa3772d771eb714e217a74b97c6aa944f3e445a09994b
`

func TestParseReleaseManifest(t *testing.T) {
	manifest, err := parseReleaseManifest([]byte(testManifest))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if manifest.ReleaseID != "3f1d4c9e-0a2b-4c5d-8e9f-1a2b3c4d5e6f" {
		t.Fatalf("release id = %q", manifest.ReleaseID)
	}
	if manifest.ConfigSHA256 != "2f7c9a1b3d5e7f9012345678abcdef01" {
		t.Fatalf("config hash = %q", manifest.ConfigSHA256)
	}
	if got := manifest.Artifacts["plugins/ipguard.wasm"]; got != "3e4e631671e2774007daa3772d771eb714e217a74b97c6aa944f3e445a09994b" {
		t.Fatalf("artifact hash = %q", got)
	}

	bad := map[string]string{
		"missing marker":   "release-v1\na\nb\nc\nd\n",
		"no trailing line": strings.TrimSuffix(testManifest, "\n"),
		"odd artifacts":    testManifest + "orphan\n",
		"duplicate name":   testManifest + "plugins/ipguard.wasm\nabc\n",
		"empty field":      testManifest + "\nabc\n",
	}
	for name, input := range bad {
		if _, err := parseReleaseManifest([]byte(input)); err == nil {
			t.Fatalf("%s: parsed without an error", name)
		}
	}
}

func TestManifestMatchesRelease(t *testing.T) {
	manifest, err := parseReleaseManifest([]byte(testManifest))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	release := &wasmgatepb.ApplyRelease{
		ReleaseId:    manifest.ReleaseID,
		ConfigSha256: manifest.ConfigSHA256,
		Artifacts: []*wasmgatepb.ArtifactRef{
			{Id: "3e4e631671e2774007daa3772d771eb714e217a74b97c6aa944f3e445a09994b", Name: "plugins/ipguard.wasm"},
		},
	}
	if err := manifest.matches(release); err != nil {
		t.Fatalf("match: %v", err)
	}

	if err := manifest.matches(&wasmgatepb.ApplyRelease{
		ReleaseId:    "another-release",
		ConfigSha256: manifest.ConfigSHA256,
	}); err == nil {
		t.Fatal("matched a different release id")
	}

	tampered := &wasmgatepb.ApplyRelease{
		ReleaseId:    manifest.ReleaseID,
		ConfigSha256: manifest.ConfigSHA256,
		Artifacts: []*wasmgatepb.ArtifactRef{
			{Id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "plugins/ipguard.wasm"},
		},
	}
	if err := manifest.matches(tampered); err == nil {
		t.Fatal("matched a different artifact hash")
	}

	extra := &wasmgatepb.ApplyRelease{
		ReleaseId:    manifest.ReleaseID,
		ConfigSha256: manifest.ConfigSHA256,
		Artifacts: []*wasmgatepb.ArtifactRef{
			{Id: "3e4e631671e2774007daa3772d771eb714e217a74b97c6aa944f3e445a09994b", Name: "plugins/ipguard.wasm"},
			{Id: "3e4e631671e2774007daa3772d771eb714e217a74b97c6aa944f3e445a09994b", Name: "plugins/extra.wasm"},
		},
	}
	if err := manifest.matches(extra); err == nil {
		t.Fatal("matched an artifact outside the manifest")
	}
}

func TestVerifyManifestSignature(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	digest := sha256.Sum256([]byte(testManifest))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])

	if err := verifyManifestSignature(&key.PublicKey, []byte(testManifest), signature); err != nil {
		t.Fatalf("verify: %v", err)
	}

	tampered := []byte(strings.Replace(testManifest, "prod", "dev", 1))
	if err := verifyManifestSignature(&key.PublicKey, tampered, signature); err == nil {
		t.Fatal("verified a tampered manifest")
	}

	if err := verifyManifestSignature(&key.PublicKey, []byte(testManifest), signature[:32]); err == nil {
		t.Fatal("verified a truncated signature")
	}

	unsigned := &wasmgatepb.ApplyRelease{ReleaseId: "x"}
	if _, err := verifyRelease(&key.PublicKey, unsigned); err == nil {
		t.Fatal("accepted a release without a manifest")
	}
}

func TestArtifactRestorer(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "plugin.wasm")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("write old artifact: %v", err)
	}

	restorer := &artifactRestorer{}
	if err := restorer.save(target); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := os.WriteFile(target, []byte("new"), 0o644); err != nil {
		t.Fatalf("write new artifact: %v", err)
	}

	restorer.restore()
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read restored artifact: %v", err)
	}
	if string(content) != "old" {
		t.Fatalf("restored content = %q, want %q", content, "old")
	}

	fresh := filepath.Join(dir, "fresh.wasm")
	restorer = &artifactRestorer{}
	if err := restorer.save(fresh); err != nil {
		t.Fatalf("save missing: %v", err)
	}
	if err := os.WriteFile(fresh, []byte("new"), 0o644); err != nil {
		t.Fatalf("write fresh artifact: %v", err)
	}
	restorer.restore()
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("restore kept a file that did not exist before: %v", err)
	}

	restorer = &artifactRestorer{}
	if err := restorer.save(target); err != nil {
		t.Fatalf("save for discard: %v", err)
	}
	restorer.discard()
	backups, err := filepath.Glob(target + ".wasmgate-backup-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(backups) != 0 {
		t.Fatalf("discard left backups: %v", backups)
	}
}
