package release

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"

	"wasmgate/internal/controlplane/wasmgatepb"
)

// manifestMarker identifies the canonical manifest format signed by the control
// plane. Changing it invalidates every existing signature.
const manifestMarker = "wasmgate-release-v1"

// releaseManifest is the parsed form of the manifest signed by the control plane.
type releaseManifest struct {
	ReleaseID       string
	Environment     string
	ConfigVersionID string
	ConfigSHA256    string
	Artifacts       map[string]string // install path -> content hash
}

// loadReleaseKey reads the ECDSA P-256 public key used to verify releases.
func LoadReleaseKey(path string) (*ecdsa.PublicKey, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-configured path
	if err != nil {
		return nil, fmt.Errorf("control plane: read release key: %w", err)
	}

	rest := raw
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "PUBLIC KEY" {
			continue
		}

		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("control plane: parse release key: %w", err)
		}

		key, ok := parsed.(*ecdsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("control plane: release key %s is not an ECDSA key", path)
		}
		if key.Curve != elliptic.P256() {
			return nil, fmt.Errorf("control plane: release key %s is not a P-256 key", path)
		}

		return key, nil
	}

	return nil, fmt.Errorf("control plane: %s contains no PUBLIC KEY PEM block", path)
}

// parseUnsignedRelease validates the manifest and its match with the command
// without checking a signature. It is used only in trusted loopback mode, where
// the control plane and data plane run on the same host and no release key is
// configured.
func parseUnsignedRelease(release *wasmgatepb.ApplyRelease) (releaseManifest, error) {
	manifest := release.GetManifest()
	if len(manifest) == 0 {
		return releaseManifest{}, errors.New("release manifest is missing")
	}
	parsed, err := parseReleaseManifest(manifest)
	if err != nil {
		return releaseManifest{}, err
	}
	return parsed, parsed.matches(release)
}

// verifyRelease checks that the release was signed by the control plane and that
// the signed manifest describes exactly this message. It returns the parsed
// manifest so callers can enforce environment scope.
func verifyRelease(key *ecdsa.PublicKey, release *wasmgatepb.ApplyRelease) (releaseManifest, error) {
	var zero releaseManifest
	manifest := release.GetManifest()
	signature := release.GetManifestSignature()

	if len(manifest) == 0 || len(signature) == 0 {
		return zero, errors.New("release is not signed")
	}
	if err := verifyManifestSignature(key, manifest, signature); err != nil {
		return zero, err
	}

	parsed, err := parseReleaseManifest(manifest)
	if err != nil {
		return zero, err
	}

	return parsed, parsed.matches(release)
}

// verifyManifestSignature verifies an IEEE P1363 (r||s) ECDSA P-256 signature.
func verifyManifestSignature(key *ecdsa.PublicKey, manifest, signature []byte) error {
	if len(signature) != 64 {
		return fmt.Errorf("manifest signature must be 64 bytes, got %d", len(signature))
	}

	digest := sha256.Sum256(manifest)
	r := new(big.Int).SetBytes(signature[:32])
	s := new(big.Int).SetBytes(signature[32:])
	if !ecdsa.Verify(key, digest[:], r, s) {
		return errors.New("manifest signature does not match")
	}
	return nil
}

// parseReleaseManifest reads the line-oriented manifest produced by the control
// plane.
func parseReleaseManifest(data []byte) (releaseManifest, error) {
	var manifest releaseManifest

	text := string(data)
	if !strings.HasSuffix(text, "\n") {
		return manifest, errors.New("manifest does not end with a newline")
	}

	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) < 5 || lines[0] != manifestMarker {
		return manifest, fmt.Errorf("manifest marker %q is missing", manifestMarker)
	}

	manifest.ReleaseID = lines[1]
	manifest.Environment = lines[2]
	manifest.ConfigVersionID = lines[3]
	manifest.ConfigSHA256 = lines[4]
	if manifest.ReleaseID == "" || manifest.ConfigVersionID == "" || manifest.ConfigSHA256 == "" {
		return manifest, errors.New("manifest has an empty required field")
	}

	rest := lines[5:]
	if len(rest)%2 != 0 {
		return manifest, errors.New("manifest has an incomplete artifact entry")
	}

	manifest.Artifacts = make(map[string]string, len(rest)/2)
	for i := 0; i < len(rest); i += 2 {
		name, id := rest[i], rest[i+1]
		if name == "" || id == "" {
			return manifest, errors.New("manifest has an empty artifact field")
		}
		if _, duplicate := manifest.Artifacts[name]; duplicate {
			return manifest, fmt.Errorf("manifest lists artifact %q more than once", name)
		}
		manifest.Artifacts[name] = id
	}

	return manifest, nil
}

// matches reports whether the signed manifest describes the release command.
func (m releaseManifest) matches(release *wasmgatepb.ApplyRelease) error {
	if m.ReleaseID != release.GetReleaseId() {
		return fmt.Errorf("manifest release id %q does not match %q", m.ReleaseID, release.GetReleaseId())
	}
	if !strings.EqualFold(m.ConfigSHA256, release.GetConfigSha256()) {
		return errors.New("manifest configuration hash does not match the command")
	}

	artifacts := release.GetArtifacts()
	if len(artifacts) != len(m.Artifacts) {
		return fmt.Errorf(
			"manifest lists %d artifacts, the command has %d",
			len(m.Artifacts), len(artifacts))
	}

	for _, artifact := range artifacts {
		expected, ok := m.Artifacts[artifact.GetName()]
		if !ok {
			return fmt.Errorf("artifact %q is not part of the signed manifest", artifact.GetName())
		}
		if !strings.EqualFold(expected, artifact.GetId()) {
			return fmt.Errorf("artifact %q hash does not match the signed manifest", artifact.GetName())
		}
	}

	return nil
}
