package release

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"

	"wasmgate/internal/config"
	"wasmgate/internal/controlplane/options"
	"wasmgate/internal/controlplane/wasmgatepb"
)

// Reloader applies a validated configuration to the running data plane.
type Reloader interface {
	Reload(ctx context.Context, cfg *config.Config) (uint64, error)
}

// AppliedState is the release state persisted by the session package.
type AppliedState struct {
	ReleaseID    string `json:"release_id,omitempty"`
	ConfigSHA256 string `json:"config_sha256,omitempty"`
	Version      uint64 `json:"config_version,omitempty"`
}

// State stores the release currently active in the data plane.
type State interface {
	Get() AppliedState
	Set(AppliedState)
	Save() error
}

// maxArtifactSize caps a single downloaded artifact to protect the disk from a
// broken or hostile control plane.
const maxArtifactSize = 256 << 20 // 256 MiB

// applyRelease verifies the release signature, downloads the artifacts,
// validates the configuration, reloads the data plane and reports the outcome
// to the control plane.
func Apply(
	ctx context.Context,
	conn *grpc.ClientConn,
	release *wasmgatepb.ApplyRelease,
	opts options.Options,
	key *ecdsa.PublicKey,
	reloader Reloader,
	state State,
	log *slog.Logger,
) *wasmgatepb.ApplyResult {
	if log == nil {
		log = slog.Default()
	}
	result := &wasmgatepb.ApplyResult{
		ReleaseId:       release.GetReleaseId(),
		DeploymentId:    release.GetDeploymentId(),
		AppliedAtUnixMs: time.Now().UnixMilli(),
	}

	applyCtx := ctx
	if timeout := release.GetTimeoutMs(); timeout > 0 {
		var cancel context.CancelFunc
		applyCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
		defer cancel()
	}

	fail := func(err error) *wasmgatepb.ApplyResult {
		result.Ok = false
		result.Error = err.Error()
		log.Error("release apply failed", "release_id", result.ReleaseId, "err", err)
		return result
	}

	// The signature is checked before the document is parsed: an unsigned or
	// altered release must not even reach the configuration parser. Local mode
	// has no release key; it trusts the loopback control plane and validates the
	// manifest fields only.
	var manifest releaseManifest
	var err error
	if key == nil {
		manifest, err = parseUnsignedRelease(release)
	} else {
		manifest, err = verifyRelease(key, release)
	}
	if err != nil {
		return fail(fmt.Errorf("verify release: %w", err))
	}
	if opts.Environment != "" && !strings.EqualFold(manifest.Environment, opts.Environment) {
		return fail(fmt.Errorf(
			"release environment %q does not match this agent's environment %q",
			manifest.Environment,
			opts.Environment))
	}

	cfg, dir, err := PrepareConfig(opts, release)
	if err != nil {
		return fail(err)
	}

	// Remember every artifact this release overwrites, so a rejected reload can
	// put the previous release back on disk.
	restorer := &artifactRestorer{}
	for _, artifact := range release.GetArtifacts() {
		if err := DownloadArtifact(applyCtx, conn, dir, artifact, restorer); err != nil {
			restorer.restore()
			return fail(fmt.Errorf("artifact %q: %w", artifact.GetName(), err))
		}
	}

	// Keep the previous document so a rejected reload leaves the data plane and
	// the file on disk consistent.
	previous, previousReadErr := os.ReadFile(opts.ConfigPath) // #nosec G304 -- operator-configured path
	if err := writeFileAtomic(opts.ConfigPath, release.GetConfigYaml()); err != nil {
		restorer.restore()
		return fail(fmt.Errorf("write configuration: %w", err))
	}

	version, err := reloader.Reload(applyCtx, cfg)
	if err != nil {
		if previousReadErr == nil {
			if restoreErr := writeFileAtomic(opts.ConfigPath, previous); restoreErr != nil {
				log.Error("failed to restore the previous configuration file", "err", restoreErr)
			}
		}
		restorer.restore()
		return fail(fmt.Errorf("reload: %w", err))
	}

	restorer.discard()

	sum := sha256.Sum256(release.GetConfigYaml())
	next := AppliedState{
		ReleaseID:    release.GetReleaseId(),
		ConfigSHA256: hex.EncodeToString(sum[:]),
		Version:      version,
	}
	state.Set(next)
	if err := state.Save(); err != nil {
		log.Warn("failed to persist applied state", "err", err)
	}

	result.Ok = true
	result.Listeners = listenerStatuses(cfg)
	log.Info(
		"release applied",
		"release_id", next.ReleaseID,
		"config_version", version,
		"sha256", next.ConfigSHA256)
	return result
}

// prepareConfig verifies the document hash, parses and validates the
// configuration and anchors relative artifact paths at the configuration
// directory.
func PrepareConfig(opts options.Options, release *wasmgatepb.ApplyRelease) (*config.Config, string, error) {
	document := release.GetConfigYaml()
	if len(document) == 0 {
		return nil, "", errors.New("release contains no configuration")
	}

	sum := sha256.Sum256(document)
	digest := hex.EncodeToString(sum[:])
	if want := release.GetConfigSha256(); want != "" && !strings.EqualFold(want, digest) {
		return nil, "", fmt.Errorf("configuration SHA-256 mismatch: got %s, want %s", digest, want)
	}

	cfg, err := config.Parse(document)
	if err != nil {
		return nil, "", fmt.Errorf("parse configuration: %w", err)
	}

	dir, err := filepath.Abs(filepath.Dir(opts.ConfigPath))
	if err != nil {
		return nil, "", fmt.Errorf("configuration directory: %w", err)
	}
	cfg.Dir = dir
	return cfg, dir, nil
}

// downloadArtifact streams one content-addressed artifact next to the
// configuration file, verifies its SHA-256 and size, and remembers the file it
// replaces so a failed reload can put the previous release back on disk.
func DownloadArtifact(
	ctx context.Context,
	conn *grpc.ClientConn,
	dir string,
	ref *wasmgatepb.ArtifactRef,
	restorer *artifactRestorer,
) error {
	id := strings.ToLower(strings.TrimSpace(ref.GetId()))
	if !isSha256(id) {
		return fmt.Errorf("id %q is not a SHA-256 hex string", ref.GetId())
	}

	target, err := safeJoin(dir, ref.GetName())
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("create artifact directory: %w", err)
	}

	stream, err := wasmgatepb.NewAgentArtifactsClient(conn).Download(
		ctx,
		&wasmgatepb.ArtifactDownloadRequest{Id: id})
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	temp, err := os.CreateTemp(dir, ".wasmgate-artifact-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()

	hasher := sha256.New()
	var size int64

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = temp.Close()
			return fmt.Errorf("receive: %w", err)
		}

		data := chunk.GetData()
		size += int64(len(data))
		if size > maxArtifactSize {
			_ = temp.Close()
			return fmt.Errorf("artifact exceeds %d bytes", maxArtifactSize)
		}
		if _, err := temp.Write(data); err != nil {
			_ = temp.Close()
			return fmt.Errorf("write: %w", err)
		}
		hasher.Write(data) //nolint:errcheck // hash.Hash.Write never returns an error
	}

	if err := temp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}

	if got := hex.EncodeToString(hasher.Sum(nil)); got != id {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, id)
	}
	if want := ref.GetSize(); want > 0 && size != want {
		return fmt.Errorf("size mismatch: got %d bytes, want %d", size, want)
	}

	if err := restorer.save(target); err != nil {
		return fmt.Errorf("back up the previous artifact: %w", err)
	}
	if err := publishFile(tempName, target); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

// artifactRestorer remembers the files a release replaced so the previous state
// can be restored when the reload fails. A restore is best effort: the error
// that caused it is what the operator needs to see.
