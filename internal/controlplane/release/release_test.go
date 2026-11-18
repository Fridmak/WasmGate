package release

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"wasmgate/internal/config"
	"wasmgate/internal/controlplane/options"
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
	if err != nil || ok != filepath.Join(base, "plugins", "ipguard.wasm") {
		t.Fatalf("safeJoin(valid) = %q, %v", ok, err)
	}
	for _, name := range []string{"", "   ", "../escape.wasm", "a/../../escape.wasm", `..\escape.wasm`, `plugins\..\..\escape.wasm`, "C:/escape.wasm", `C:\escape.wasm`, `\\server\share\escape.wasm`, "/etc/passwd", "plugins/\x00.wasm"} {
		if _, err := safeJoin(base, name); err == nil {
			t.Fatalf("safeJoin(%q) accepted an unsafe path", name)
		}
	}
	if _, err := safeJoin(base, filepath.Join(base, "abs.wasm")); err == nil {
		t.Fatal("safeJoin accepted an absolute path")
	}
}

func TestPrepareConfig(t *testing.T) {
	dir := t.TempDir()
	opts := options.Options{ConfigPath: filepath.Join(dir, "wasmgate.yaml")}
	digest := sha256.Sum256([]byte(validConfig))
	cfg, gotDir, err := PrepareConfig(opts, &wasmgatepb.ApplyRelease{ConfigYaml: []byte(validConfig), ConfigSha256: strings.ToUpper(hex.EncodeToString(digest[:]))})
	if err != nil || cfg.Dir != gotDir || gotDir != dir || len(cfg.Listeners) != 1 {
		t.Fatalf("PrepareConfig = cfg=%v dir=%q err=%v", cfg, gotDir, err)
	}
	if _, _, err := PrepareConfig(opts, &wasmgatepb.ApplyRelease{ConfigYaml: []byte(validConfig), ConfigSha256: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("PrepareConfig accepted a wrong SHA-256")
	}
	if _, _, err := PrepareConfig(opts, &wasmgatepb.ApplyRelease{ConfigYaml: []byte("version: 99\n")}); err == nil {
		t.Fatal("PrepareConfig accepted an invalid document")
	}
}

func TestWriteFileAtomicReplacesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "wasmgate.yaml")
	if err := writeFileAtomic(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "second" {
		t.Fatalf("content = %q, err=%v", got, err)
	}
}

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

type testState struct {
	st    AppliedState
	saved bool
}

func (s *testState) Get() AppliedState   { return s.st }
func (s *testState) Set(st AppliedState) { s.st = st }
func (s *testState) Save() error         { s.saved = true; return nil }

type testReloader struct {
	version uint64
	err     error
}

func (r testReloader) Reload(context.Context, *config.Config) (uint64, error) {
	return r.version, r.err
}

func signedRelease(t *testing.T, key *ecdsa.PrivateKey, releaseID, environment string) *wasmgatepb.ApplyRelease {
	t.Helper()
	sum := sha256.Sum256([]byte(validConfig))
	digest := hex.EncodeToString(sum[:])
	manifest := fmt.Sprintf("%s\n%s\n%s\ncfg-1\n%s\n", manifestMarker, releaseID, environment, digest)
	manifestDigest := sha256.Sum256([]byte(manifest))
	r, s, err := ecdsa.Sign(rand.Reader, key, manifestDigest[:])
	if err != nil {
		t.Fatal(err)
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return &wasmgatepb.ApplyRelease{
		ReleaseId:         releaseID,
		ConfigSha256:      digest,
		ConfigYaml:        []byte(validConfig),
		Manifest:          []byte(manifest),
		ManifestSignature: signature,
	}
}

func TestApplyPersistsStateAndWritesConfig(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "wasmgate.yaml")
	state := &testState{}
	result := Apply(
		context.Background(), nil,
		signedRelease(t, key, "release-1", "prod"),
		options.Options{ConfigPath: configPath, Environment: "prod"},
		&key.PublicKey, testReloader{version: 7}, state, slog.Default(),
	)
	if !result.GetOk() || result.GetListeners() == nil {
		t.Fatalf("Apply = %+v", result)
	}
	if state.st.ReleaseID != "release-1" || state.st.Version != 7 || !state.saved {
		t.Fatalf("state = %+v, saved=%v", state.st, state.saved)
	}
	content, err := os.ReadFile(configPath)
	if err != nil || string(content) != validConfig {
		t.Fatalf("config = %q, err=%v", content, err)
	}
}

func TestApplyRestoresConfigWhenReloadFails(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "wasmgate.yaml")
	const previous = "previous configuration"
	if err := os.WriteFile(configPath, []byte(previous), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &testState{}
	result := Apply(
		context.Background(), nil,
		signedRelease(t, key, "release-2", "prod"),
		options.Options{ConfigPath: configPath, Environment: "prod"},
		&key.PublicKey, testReloader{err: errors.New("reload failed")}, state, slog.Default(),
	)
	if result.GetOk() {
		t.Fatal("failed reload was reported as applied")
	}
	content, err := os.ReadFile(configPath)
	if err != nil || string(content) != previous {
		t.Fatalf("config after rollback = %q, err=%v", content, err)
	}
	if state.saved || state.st != (AppliedState{}) {
		t.Fatalf("failed apply changed state: %+v saved=%v", state.st, state.saved)
	}
}

func TestParseUnsignedReleaseForLocalMode(t *testing.T) {
	release := &wasmgatepb.ApplyRelease{
		ReleaseId:    "3f1d4c9e-0a2b-4c5d-8e9f-1a2b3c4d5e6f",
		ConfigSha256: "2f7c9a1b3d5e7f9012345678abcdef01",
		Manifest:     []byte(testManifest),
		Artifacts: []*wasmgatepb.ArtifactRef{
			{Id: "3e4e631671e2774007daa3772d771eb714e217a74b97c6aa944f3e445a09994b", Name: "plugins/ipguard.wasm"},
		},
	}
	manifest, err := parseUnsignedRelease(release)
	if err != nil || manifest.ReleaseID != release.ReleaseId {
		t.Fatalf("parseUnsignedRelease = %+v, %v", manifest, err)
	}
	if _, err := parseUnsignedRelease(&wasmgatepb.ApplyRelease{ReleaseId: release.ReleaseId}); err == nil {
		t.Fatal("parseUnsignedRelease accepted a missing manifest")
	}
}

func unsignedRelease(releaseID, environment string) *wasmgatepb.ApplyRelease {
	sum := sha256.Sum256([]byte(validConfig))
	digest := hex.EncodeToString(sum[:])
	manifest := fmt.Sprintf(
		"%s\n%s\n%s\ncfg-1\n%s\n",
		manifestMarker,
		releaseID,
		environment,
		digest)
	return &wasmgatepb.ApplyRelease{
		ReleaseId:    releaseID,
		ConfigSha256: digest,
		ConfigYaml:   []byte(validConfig),
		Manifest:     []byte(manifest),
	}
}

func TestApplyRejectsNilKeyOutsideLocalMode(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "wasmgate.yaml")
	state := &testState{}

	result := Apply(
		context.Background(), nil,
		unsignedRelease("release-nil-key", "prod"),
		options.Options{ConfigPath: configPath, Environment: "prod"},
		nil, testReloader{version: 1}, state, slog.Default(),
	)

	if result.GetOk() {
		t.Fatal("non-local release without a verification key was applied")
	}
	if !strings.Contains(result.GetError(), "release public key") {
		t.Fatalf("error = %q, want a missing-key error", result.GetError())
	}
	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configuration was written despite the missing key: %v", err)
	}
	if state.saved {
		t.Fatal("failed apply persisted state")
	}
}

func TestApplyLocalModeAcceptsUnsignedRelease(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "wasmgate.yaml")
	state := &testState{}

	result := Apply(
		context.Background(), nil,
		unsignedRelease("release-local", "prod"),
		options.Options{ConfigPath: configPath, Environment: "prod", Local: true},
		nil, testReloader{version: 3}, state, slog.Default(),
	)

	if !result.GetOk() {
		t.Fatalf("local unsigned release failed: %s", result.GetError())
	}
	if state.st.ReleaseID != "release-local" || state.st.Version != 3 || !state.saved {
		t.Fatalf("state = %+v, saved=%v", state.st, state.saved)
	}
	content, err := os.ReadFile(configPath)
	if err != nil || string(content) != validConfig {
		t.Fatalf("config = %q, err=%v", content, err)
	}
}

func TestApplyLocalModeStillVerifiesWhenKeyConfigured(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "wasmgate.yaml")
	release := signedRelease(t, key, "release-local-key", "prod")
	release.ManifestSignature[0] ^= 0xff

	result := Apply(
		context.Background(), nil,
		release,
		options.Options{ConfigPath: configPath, Environment: "prod", Local: true},
		&key.PublicKey, testReloader{version: 1}, &testState{}, slog.Default(),
	)

	if result.GetOk() || !strings.Contains(result.GetError(), "signature") {
		t.Fatalf("tampered local release = %+v", result)
	}
}
