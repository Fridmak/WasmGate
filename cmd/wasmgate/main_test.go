package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const remoteAdminConfig = `version: 1
admin_addr: "127.0.0.1:9099"
admin_allow_remote: true

listeners:
  - name: test
    addr: "127.0.0.1:18080"
    upstream_pool: web

pools:
  web:
    servers: ["127.0.0.1:9000"]
`

// runWithArgs runs the CLI entry point with a fresh flag set. This keeps tests
// independent of each other and of flags registered by the test binary.
func runWithArgs(t *testing.T, args ...string) error {
	t.Helper()

	oldArgs := os.Args
	oldCommandLine := flag.CommandLine
	t.Cleanup(func() {
		os.Args = oldArgs
		flag.CommandLine = oldCommandLine
	})

	flag.CommandLine = flag.NewFlagSet("wasmgate-test", flag.ContinueOnError)
	os.Args = append([]string{"wasmgate"}, args...)
	return run()
}

func writeConfig(t *testing.T) string {
	t.Helper()
	return writeConfigText(t, remoteAdminConfig)
}

func writeConfigText(t *testing.T, text string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "wasmgate.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// -validate must not require runtime secrets: the control plane validates
// releases in an environment that has WASMGATE_CP_ADMIN_TOKEN, not
// WASMGATE_ADMIN_TOKEN.
func TestValidateDoesNotRequireAdminToken(t *testing.T) {
	t.Setenv("WASMGATE_ADMIN_TOKEN", "")
	path := writeConfig(t)

	if err := runWithArgs(t, "-config", path, "-validate"); err != nil {
		t.Fatalf("validate should not require an admin token: %v", err)
	}
}

// The runtime path still refuses a remote admin endpoint without authentication.
func TestRunStillRequiresAdminToken(t *testing.T) {
	t.Setenv("WASMGATE_ADMIN_TOKEN", "")
	path := writeConfig(t)

	err := runWithArgs(t, "-config", path)
	if err == nil {
		t.Fatal("expected startup to fail without an admin token")
	}
	if !strings.Contains(err.Error(), "admin_allow_remote requires") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A token file is a runtime secret too; validation must not require it to exist.
func TestValidateDoesNotReadAdminTokenFile(t *testing.T) {
	t.Setenv("WASMGATE_ADMIN_TOKEN", "")
	config := strings.Replace(
		remoteAdminConfig,
		"admin_allow_remote: true",
		"admin_allow_remote: true\nadmin_token_file: \"/missing/admin.token\"",
		1,
	)
	path := writeConfigText(t, config)

	if err := runWithArgs(t, "-config", path, "-validate"); err != nil {
		t.Fatalf("validate should not read the admin token file: %v", err)
	}
}
