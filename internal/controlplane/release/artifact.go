package release

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"wasmgate/internal/config"
	"wasmgate/internal/controlplane/wasmgatepb"
)

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
