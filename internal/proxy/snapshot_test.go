package proxy

import (
	"context"
	"sync"
	"testing"
	"time"

	"wasmgate/internal/config"
)

// Tests for snapshot.go: versioning, the atomic publish of a new snapshot and
// the listener resolution done by build, followed by the benchmarks for the
// hot-reload seam.

func TestReloadBumpsVersion(t *testing.T) {
	cfg := &config.Config{
		Version:   1,
		Listeners: []config.ListenerConfig{{Name: "edge", Addr: "127.0.0.1:0", UpstreamPool: "web"}},
		Pools:     map[string]config.PoolConfig{"web": {Servers: []string{"a"}}},
	}
	rt := NewRuntime(cfg)
	if got := rt.Load().Version; got != 1 {
		t.Fatalf("initial version = %d, want 1", got)
	}
	if got := rt.Reload(cfg); got != 2 {
		t.Fatalf("reloaded version = %d, want 2", got)
	}
}

// TestConcurrentReloadVersionsUnique verifies that concurrent reloads never
// publish the same version (the read-modify-write must be serialized).
func TestConcurrentReloadVersionsUnique(t *testing.T) {
	cfg := &config.Config{
		Version:   1,
		Listeners: []config.ListenerConfig{{Name: "edge", Addr: "127.0.0.1:0", UpstreamPool: "web"}},
		Pools:     map[string]config.PoolConfig{"web": {Servers: []string{"a"}}},
	}
	rt := NewRuntime(cfg)

	const n = 64
	versions := make([]uint64, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			versions[i] = rt.Reload(cfg)
		}(i)
	}
	wg.Wait()

	seen := map[uint64]bool{1: true} // the initial snapshot
	for _, v := range versions {
		if seen[v] {
			t.Fatalf("duplicate version %d published", v)
		}
		seen[v] = true
	}
	if len(seen) != n+1 {
		t.Fatalf("got %d distinct versions, want %d", len(seen), n+1)
	}
}

// TestBuildResolvesListenerToPool covers the pre-resolution in build: the data
// path must find the pool by a single map lookup, and a limiter is exposed only
// when a limit is actually configured (nil means unlimited).
func TestBuildResolvesListenerToPool(t *testing.T) {
	unlimited := NewRuntime(edgeConfig([]string{"a"}, nil))
	rl := unlimited.Load().listeners["edge"]
	if rl.pool == nil {
		t.Fatal("listener was not resolved to its pool")
	}
	if rl.pool.Name() != "web" {
		t.Fatalf("resolved pool = %q, want web", rl.pool.Name())
	}
	if rl.limiter != nil {
		t.Fatal("max_conns 0 exposed a limiter, want nil (unlimited)")
	}

	limited := NewRuntime(edgeConfig([]string{"a"}, func(lc *config.ListenerConfig) { lc.MaxConns = 3 }))
	rl = limited.Load().listeners["edge"]
	if rl.limiter == nil {
		t.Fatal("max_conns 3 exposed no limiter")
	}
	if got := rl.limiter.limit(); got != 3 {
		t.Fatalf("limiter max = %d, want 3", got)
	}
}

func TestReloadCheckedFailClosedPreservesLiveState(t *testing.T) {
	cfg1 := &config.Config{
		Version: 1,
		Listeners: []config.ListenerConfig{
			{Name: "l1", Addr: "127.0.0.1:0", UpstreamPool: "web", MaxConns: 5},
			{Name: "l2", Addr: "127.0.0.1:0", UpstreamPool: "web"},
		},
		Pools: map[string]config.PoolConfig{"web": {Servers: []string{"127.0.0.1:9001"}}},
	}
	rt, err := NewRuntimeChecked(cfg1)
	if err != nil {
		t.Fatalf("NewRuntimeChecked: %v", err)
	}

	// Second reload changes l1.MaxConns to 99, but l2 has a corrupt plugin.
	// ReloadChecked must fail AND leave l1's live limiter at 5.
	cfgBad := &config.Config{
		Version: 1,
		Listeners: []config.ListenerConfig{
			{Name: "l1", Addr: "127.0.0.1:0", UpstreamPool: "web", MaxConns: 99},
			{Name: "l2", Addr: "127.0.0.1:0", UpstreamPool: "web", Plugin: config.PluginConfig{Wasm: []byte("not-a-wasm")}},
		},
		Pools: map[string]config.PoolConfig{"web": {Servers: []string{"127.0.0.1:9001"}}},
	}
	if _, err := rt.ReloadChecked(cfgBad); err == nil {
		t.Fatal("ReloadChecked succeeded with corrupt plugin")
	}
	if got := rt.Load().listeners["l1"].limiter.limit(); got != 5 {
		t.Fatalf("live limiter was mutated on failed reload: got %d, want 5", got)
	}
}

func TestReloadRetiresOldEngineAfterPinnedSnapshot(t *testing.T) {
	pluginCfg := func(verdict byte) config.PluginConfig {
		return config.PluginConfig{
			Wasm:     v4Verdict(verdict),
			Timeout:  time.Second,
			DataMode: config.DataModeHeaders,
		}
	}
	cfg := edgeConfig([]string{"127.0.0.1:9000"}, func(lc *config.ListenerConfig) {
		lc.Plugin = pluginCfg(0)
	})
	rt, err := NewRuntimeChecked(cfg)
	if err != nil {
		t.Fatalf("NewRuntimeChecked: %v", err)
	}

	pinned := rt.Acquire()
	released := false
	defer func() {
		if !released {
			pinned.Release()
		}
	}()
	oldEngine := pinned.listeners["edge"].pluginV4
	if oldEngine == nil {
		t.Fatal("listener has no plugin engine")
	}

	next := edgeConfig([]string{"127.0.0.1:9000"}, func(lc *config.ListenerConfig) {
		lc.Plugin = pluginCfg(1)
	})
	if _, err := rt.ReloadChecked(next); err != nil {
		t.Fatalf("ReloadChecked: %v", err)
	}
	if got := rt.Load().listeners["edge"].pluginV4; got == oldEngine {
		t.Fatal("reload reused the old engine for changed module bytes")
	}

	// The old snapshot is still pinned, so its engine must remain usable even
	// though the registry and the current snapshot no longer reference it.
	block := []byte("GET / HTTP/1.1\r\nHost: example.test\r\n\r\n")
	if v, err := oldEngine.OnRequestHeaders(context.Background(), 1, block); err != nil || v != 0 {
		t.Fatalf("pinned old engine = (%v, %v)", v, err)
	}

	pinned.Release()
	released = true
	if _, err := oldEngine.OnRequestHeaders(context.Background(), 2, block); err == nil {
		t.Fatal("retired engine still accepts events after its last snapshot was released")
	}
}

func TestReloadRemovingPluginRetiresEngine(t *testing.T) {
	cfg := edgeConfig([]string{"127.0.0.1:9000"}, func(lc *config.ListenerConfig) {
		lc.Plugin = config.PluginConfig{
			Wasm:     v4Verdict(0),
			Timeout:  time.Second,
			DataMode: config.DataModeHeaders,
		}
	})
	rt, err := NewRuntimeChecked(cfg)
	if err != nil {
		t.Fatalf("NewRuntimeChecked: %v", err)
	}
	pinned := rt.Acquire()
	released := false
	defer func() {
		if !released {
			pinned.Release()
		}
	}()
	oldEngine := pinned.listeners["edge"].pluginV4
	if oldEngine == nil {
		t.Fatal("listener has no plugin engine")
	}

	if _, err := rt.ReloadChecked(edgeConfig([]string{"127.0.0.1:9000"}, nil)); err != nil {
		t.Fatalf("ReloadChecked: %v", err)
	}
	if got := rt.Load().listeners["edge"].pluginV4; got != nil {
		t.Fatal("plugin remained attached after removal from the listener")
	}
	if _, ok := rt.registry.plugins["edge"]; ok {
		t.Fatal("removed plugin engine stayed cached in the registry")
	}

	pinned.Release()
	released = true
	if _, err := oldEngine.OnRequestHeaders(context.Background(), 2,
		[]byte("GET / HTTP/1.1\r\nHost: example.test\r\n\r\n")); err == nil {
		t.Fatal("removed plugin engine still accepts events after its snapshot was released")
	}
}

// BenchmarkRuntimeLoad isolates the atomic snapshot load that opens every
// connection — the floor cost of the hot-reload seam.
func BenchmarkRuntimeLoad(b *testing.B) {
	rt := NewRuntime(validated(b, edgeConfig([]string{"127.0.0.1:9000"}, nil)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkSnapshot = rt.Load()
	}
}

// BenchmarkReload measures the hot-reload cold path: compile a fresh snapshot
// (pools, listener resolution, limiter wiring) and publish it atomically. This is
// the per-SIGHUP cost on top of config.Load, and it runs while existing
// connections keep using the snapshot they already captured — which is exactly
// why the data path must never take the reload lock.
func BenchmarkReload(b *testing.B) {
	cfg := validated(b, edgeConfig([]string{"127.0.0.1:9000"}, nil))
	rt := NewRuntime(cfg)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sinkVersion = rt.Reload(cfg)
	}
}
