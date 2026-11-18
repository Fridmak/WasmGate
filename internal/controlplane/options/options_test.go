package options

import "testing"

func TestOptionsNormalize(t *testing.T) {
	opts := Options{Endpoint: "http://127.0.0.1:9096", Insecure: true, ConfigPath: "wasmgate.yaml", ReleaseKey: "release-signing.key.pub"}
	if err := opts.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if opts.Heartbeat != DefaultHeartbeat || opts.RenewBefore != DefaultRenewBefore || len(opts.AbiVersions) == 0 || opts.Name == "" || opts.StateDir == "" {
		t.Fatalf("defaults were not applied: %+v", opts)
	}
	if err := (&Options{Endpoint: "http://127.0.0.1:9096", ConfigPath: "x.yaml", ReleaseKey: "key.pub"}).Normalize(); err == nil {
		t.Fatal("Normalize accepted http:// without Insecure")
	}
	if err := (&Options{Endpoint: "http://cp.example:9096", Insecure: true, ConfigPath: "x.yaml", ReleaseKey: "key.pub"}).Normalize(); err == nil {
		t.Fatal("Normalize accepted insecure http:// on a non-loopback host")
	}
	if err := (&Options{Endpoint: "https://cp.example:9096", ConfigPath: "x.yaml"}).Normalize(); err == nil {
		t.Fatal("Normalize accepted options without a release key")
	}
}

func TestLocalModeDefaultsAndValidation(t *testing.T) {
	opts := Options{Local: true, ConfigPath: "wasmgate.yaml", StateDir: t.TempDir()}
	if err := opts.Normalize(); err != nil {
		t.Fatalf("local Normalize: %v", err)
	}
	if opts.Endpoint != "http://127.0.0.1:9096" || !opts.Insecure {
		t.Fatalf("local defaults = endpoint %q insecure %v", opts.Endpoint, opts.Insecure)
	}

	for name, candidate := range map[string]Options{
		"https":     {Local: true, Endpoint: "https://127.0.0.1:9096", ConfigPath: "x.yaml"},
		"remote":    {Local: true, Endpoint: "http://192.0.2.1:9096", ConfigPath: "x.yaml"},
		"token":     {Local: true, EnrollToken: "token", ConfigPath: "x.yaml"},
		"server CA": {Local: true, ServerCA: "ca.pem", ConfigPath: "x.yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := candidate.Normalize(); err == nil {
				t.Fatalf("local mode accepted %s", name)
			}
		})
	}
}
