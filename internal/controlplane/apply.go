package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"

	"wasmgate/internal/config"
	"wasmgate/internal/controlplane/wasmgatepb"
)

// maxArtifactSize caps a single downloaded artifact to protect the disk from a
// broken or hostile control plane.
const maxArtifactSize = 256 << 20 // 256 MiB

// applyRelease verifies the release signature, downloads the artifacts,
// validates the configuration, reloads the data plane and reports the outcome
// to the control plane.
func (a *Agent) applyRelease(
	ctx context.Context,
	conn *grpc.ClientConn,
	release *wasmgatepb.ApplyRelease,
	state *stateStore,
) *wasmgatepb.ApplyResult {
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
		a.log.Error("release apply failed", "release_id", result.ReleaseId, "err", err)
		return result
	}

	// The signature is checked before the document is parsed: an unsigned or
	// altered release must not even reach the configuration parser.
	if err := verifyRelease(a.releaseKey, release); err != nil {
		return fail(fmt.Errorf("verify release: %w", err))
	}

	cfg, dir, err := a.prepareConfig(release)
	if err != nil {
		return fail(err)
	}

	// Remember every artifact this release overwrites, so a rejected reload can
	// put the previous release back on disk.
	restorer := &artifactRestorer{}
	for _, artifact := range release.GetArtifacts() {
		if err := a.downloadArtifact(applyCtx, conn, dir, artifact, restorer); err != nil {
			restorer.restore()
			return fail(fmt.Errorf("artifact %q: %w", artifact.GetName(), err))
		}
	}

	// Keep the previous document so a rejected reload leaves the data plane and
	// the file on disk consistent.
	previous, previousReadErr := os.ReadFile(a.opts.ConfigPath) // #nosec G304 -- operator-configured path
	if err := writeFileAtomic(a.opts.ConfigPath, release.GetConfigYaml()); err != nil {
		restorer.restore()
		return fail(fmt.Errorf("write configuration: %w", err))
	}

	version, err := a.reloader.Reload(applyCtx, cfg)
	if err != nil {
		if previousReadErr == nil {
			if restoreErr := writeFileAtomic(a.opts.ConfigPath, previous); restoreErr != nil {
				a.log.Error("failed to restore the previous configuration file", "err", restoreErr)
			}
		}
		restorer.restore()
		return fail(fmt.Errorf("reload: %w", err))
	}

	restorer.discard()

	sum := sha256.Sum256(release.GetConfigYaml())
	next := appliedState{
		ReleaseID:    release.GetReleaseId(),
		ConfigSHA256: hex.EncodeToString(sum[:]),
		Version:      version,
	}
	state.set(next)
	if err := saveApplied(a.opts.StateDir, next); err != nil {
		a.log.Warn("failed to persist applied state", "err", err)
	}

	result.Ok = true
	result.Listeners = listenerStatuses(cfg)
	a.log.Info(
		"release applied",
		"release_id", next.ReleaseID,
		"config_version", version,
		"sha256", next.ConfigSHA256)
	return result
}

// prepareConfig verifies the document hash, parses and validates the
// configuration and anchors relative artifact paths at the configuration
// directory.
func (a *Agent) prepareConfig(release *wasmgatepb.ApplyRelease) (*config.Config, string, error) {
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

	dir, err := filepath.Abs(filepath.Dir(a.opts.ConfigPath))
	if err != nil {
		return nil, "", fmt.Errorf("configuration directory: %w", err)
	}
	cfg.Dir = dir
	return cfg, dir, nil
}

// downloadArtifact streams one content-addressed artifact next to the
// configuration file, verifies its SHA-256 and size, and remembers the file it
// replaces so a failed reload can put the previous release back on disk.
func (a *Agent) downloadArtifact(
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
type artifactRestorer struct {
	entries []artifactBackup
}

// artifactBackup is one overwritten file; backup is empty when the file did not
// exist before the release.
type artifactBackup struct {
	target string
	backup string
}

// save moves an existing target aside before it is overwritten.
func (r *artifactRestorer) save(target string) error {
	entry := artifactBackup{target: target}

	switch _, err := os.Stat(target); {
	case err == nil:
		entry.backup = fmt.Sprintf("%s.wasmgate-backup-%d", target, len(r.entries))
		if err := os.Rename(target, entry.backup); err != nil {
			return err
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return err
	}

	r.entries = append(r.entries, entry)
	return nil
}

// restore puts every remembered file back, newest first.
func (r *artifactRestorer) restore() {
	for i := len(r.entries) - 1; i >= 0; i-- {
		entry := r.entries[i]
		if entry.backup != "" {
			_ = os.Rename(entry.backup, entry.target)
			continue
		}
		_ = os.Remove(entry.target)
	}
}

// discard drops the backups after a successful apply.
func (r *artifactRestorer) discard() {
	for _, entry := range r.entries {
		if entry.backup != "" {
			_ = os.Remove(entry.backup)
		}
	}
}

// safeJoin resolves an artifact install path inside base and rejects absolute
// paths and any path that escapes base.
// safeJoin resolves an artifact install path inside base.
//
// Release paths are portable POSIX-style relative paths: the function rejects
// backslashes, drive letters, NUL bytes, absolute paths and any path that
// escapes base. Rejecting Windows separators keeps a release portable and
// stops a name that is harmless on Linux from escaping on Windows.
func safeJoin(base, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("artifact name is empty")
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("artifact name %q contains a NUL byte", name)
	}
	if strings.ContainsAny(name, `\:`) {
		return "", fmt.Errorf("artifact name %q must use '/' separators and no drive letters", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("artifact name %q contains a control character", name)
		}
	}
	if path.IsAbs(name) {
		return "", fmt.Errorf("artifact name %q is absolute", name)
	}

	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("artifact name %q escapes the configuration directory", name)
	}

	target := filepath.Join(base, filepath.FromSlash(clean))
	rel, err := filepath.Rel(base, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("artifact name %q escapes the configuration directory", name)
	}
	return target, nil
}

// writeFileAtomic replaces path with data through a temporary file so a crash
// never leaves a half-written configuration behind.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}

	temp, err := os.CreateTemp(dir, ".wasmgate-config-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}

	return publishFile(tempName, path)
}

// publishFile moves tempName onto target, replacing an existing file. Windows
// refuses to rename over an existing file, so the target is removed first when
// the direct rename fails.
func publishFile(tempName, target string) error {
	if err := os.Rename(tempName, target); err == nil {
		return nil
	}

	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(tempName, target)
}

// listenerStatuses reports the listeners that are active in the new snapshot.
func listenerStatuses(cfg *config.Config) []*wasmgatepb.ListenerStatus {
	statuses := make([]*wasmgatepb.ListenerStatus, 0, len(cfg.Listeners))
	for _, listener := range cfg.Listeners {
		statuses = append(statuses, &wasmgatepb.ListenerStatus{
			Name:    listener.Name,
			Address: listener.Addr,
			Bound:   true,
		})
	}
	return statuses
}

// isSha256 reports whether value is a 64-character hexadecimal digest.
func isSha256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
