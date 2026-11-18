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
