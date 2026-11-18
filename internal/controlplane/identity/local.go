package identity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnsureLocalID returns the stable agent id used in trusted loopback mode.
//
// Local mode has no certificate, private key or CA. Only agent.id is persisted,
// so a restart keeps the same agent row in the control plane. The requested id
// is used when the state directory is empty; otherwise a previously stored id
// wins, which prevents an accidental duplicate agent after a restart.
func EnsureLocalID(dir, requested string) (*Identity, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("control plane: state dir is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("control plane: create state dir: %w", err)
	}

	path := filepath.Join(dir, agentIDFile)
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-configured state dir
	switch {
	case err == nil:
		id := strings.TrimSpace(string(raw))
		if !guidPattern.MatchString(id) {
			return nil, fmt.Errorf("control plane: %s does not contain a GUID", path)
		}
		return &Identity{AgentID: id}, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("control plane: read %s: %w", path, err)
	}

	id := strings.TrimSpace(requested)
	if id == "" {
		generated, err := NewAgentID()
		if err != nil {
			return nil, err
		}
		id = generated
	} else if !guidPattern.MatchString(id) {
		return nil, fmt.Errorf("control plane: agent id %q is not a GUID", id)
	}

	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("control plane: write %s: %w", path, err)
	}
	return &Identity{AgentID: id}, nil
}
