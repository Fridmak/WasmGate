package runtime

import (
	"net"
	"testing"
	"time"

	"wasmgate/internal/config"
)

// Tests for registry.go and limits.go: the reload-safe connection limiter
// (max_conns), rate limits and the registry that keeps their counters alive
// across reloads, followed by the limiter benchmarks.

// TestLimiterLimitChangeOnReloadTakesEffect covers the documented contract: a
// reload reuses the registry limiter and just re-points its limit, so a lowered
// max_conns refuses new connections immediately while the active count drains
// down to it — and the count is never reset.
func TestLimiterLimitChangeOnReloadTakesEffect(t *testing.T) {
	withMax := func(maxConns int) *config.Config {
		return edgeConfig([]string{"127.0.0.1:9000"}, func(lc *config.ListenerConfig) { lc.MaxConns = maxConns })
	}

	rt := New(withMax(2))
	first := rt.Load().listeners["edge"].Limiter
	if !first.Acquire() {
		t.Fatal("max_conns 2 refused the first connection below the limit")
	}
	if !first.Acquire() {
		t.Fatal("max_conns 2 refused the second connection below the limit")
	}
	if first.Acquire() {
		t.Fatal("max_conns 2 admitted a third connection")
	}

	rt.Reload(withMax(1))
	second := rt.Load().listeners["edge"].Limiter
	if second != first {
		t.Fatal("reload replaced the limiter instead of reusing the registry object")
	}
	if second.Acquire() {
		t.Fatal("lowered max_conns 1 admitted a connection while 2 were active")
	}

	first.Release() // an in-flight connection finishes
	first.Release()
	if !second.Acquire() {
		t.Fatal("max_conns 1 refused a connection after the count drained to zero")
	}
	if second.Acquire() {
		t.Fatal("max_conns 1 admitted a second connection")
	}
}

// TestRateLimitSurvivesReload checks that the per-IP sliding window is kept in
// the registry: a reload that keeps the same window reuses the limiter so
// connections already counted in the window are not forgotten.
func TestRateLimitSurvivesReload(t *testing.T) {
	withRate := func(limit int) *config.Config {
		return edgeConfig([]string{"127.0.0.1:9000"}, func(lc *config.ListenerConfig) {
			lc.RateLimit = config.RateLimit{Max: limit, Window: time.Minute}
		})
	}
	rt := New(withRate(2))
	ip := net.ParseIP("192.0.2.1")
	now := time.Now()
	first := rt.Load().listeners["edge"].RateLimiter
	if !first.Allow(ip, now) {
		t.Fatal("first connection refused")
	}
	if !first.Allow(ip, now) {
		t.Fatal("second connection refused")
	}

	rt.Reload(withRate(2))
	second := rt.Load().listeners["edge"].RateLimiter
	if second != first {
		t.Fatal("reload replaced the rate limiter instead of reusing the registry object")
	}
	if second.Allow(ip, now) {
		t.Fatal("third connection after reload was allowed; the window was reset")
	}
}

// TestConnLimiterUnlimitedAdmitsEverything covers the max_conns 0 branch: an
// unlimited limiter never reserves, so the counter stays at zero.
func TestConnLimiterUnlimitedAdmitsEverything(t *testing.T) {
	l := &ConnLimiter{}
	for i := 0; i < 10; i++ {
		if !l.Acquire() {
			t.Fatalf("call %d: unlimited limiter refused a connection", i)
		}
	}
	if got := l.cur.Load(); got != 0 {
		t.Fatalf("cur = %d after unlimited acquires, want 0 (never reserves)", got)
	}
}

// TestConnLimiterReleaseBelowZeroIsNoop covers the defensive branch: a release
// with no matching acquire must not drive the counter negative (a negative count
// would make the limiter admit more than max_conns).
func TestConnLimiterReleaseBelowZeroIsNoop(t *testing.T) {
	l := &ConnLimiter{}
	l.max.Store(1)

	l.Release()
	if got := l.cur.Load(); got != 0 {
		t.Fatalf("cur = %d after a release with no acquire, want 0", got)
	}
}

// TestRegistryPrunesDrainedState covers registry.prune: state that the config no
// longer references is dropped once it has drained to zero, but live counters are
// kept — removing them early would lose a Release and reset a listener's count if
// the name came back.
func TestRegistryPrunesDrainedState(t *testing.T) {
	const oldAddr = "127.0.0.1:9000"
	rt := New(edgeConfig([]string{oldAddr}, func(lc *config.ListenerConfig) { lc.MaxConns = 5 }))

	// One in-flight connection keeps both the limiter and the upstream alive.
	rl := rt.Load().listeners["edge"]
	if !rl.Limiter.Acquire() {
		t.Fatal("acquire refused below the limit")
	}
	up := rl.Pool.Next()

	// A valid config that references neither the "edge" listener nor oldAddr.
	other := validated(t, edgeConfig([]string{"127.0.0.1:9999"}, func(lc *config.ListenerConfig) {
		lc.Name = "other"
	}))

	rt.Reload(other)
	rt.registry.mu.Lock()
	_, limKept := rt.registry.limiters["edge"]
	_, upKept := rt.registry.upstreams[oldAddr]
	rt.registry.mu.Unlock()
	if !limKept || !upKept {
		t.Fatalf("live state was pruned: limiter kept=%v upstream kept=%v", limKept, upKept)
	}

	// Once drained, the next reload collects them.
	rl.Limiter.Release()
	rl.Pool.Release(up)
	rt.Reload(other)

	rt.registry.mu.Lock()
	defer rt.registry.mu.Unlock()
	if _, ok := rt.registry.limiters["edge"]; ok {
		t.Error("drained limiter was not pruned")
	}
	if _, ok := rt.registry.upstreams[oldAddr]; ok {
		t.Error("drained upstream was not pruned")
	}
	if _, ok := rt.registry.limiters["other"]; !ok {
		t.Error("the reloaded listener has no entry in the registry")
	}
}

func TestRuntimePublicInspectionAndGuards(t *testing.T) {
	cfg := edgeConfig([]string{"127.0.0.1:9000"}, func(lc *config.ListenerConfig) {
		lc.MaxConns = 2
		lc.RateLimit = config.RateLimit{MaxConcurrent: 2}
	})
	rt := New(cfg)
	snap := rt.Load()
	if _, ok := snap.Listener("missing"); ok {
		t.Fatal("missing listener resolved")
	}
	rl, ok := snap.Listener("edge")
	if !ok || rl.Limiter == nil || rl.ConnGuard == nil {
		t.Fatal("public listener guards were not resolved")
	}
	if !rl.Limiter.Acquire() || rl.Limiter.Active() != 1 {
		t.Fatal("limiter did not reserve a slot")
	}
	rl.Limiter.Release()
	ip := net.ParseIP("192.0.2.20")
	if !rl.ConnGuard.Acquire(ip) {
		t.Fatal("conn guard refused the first slot")
	}
	rl.ConnGuard.Release(ip)

	if rt.Upstream("127.0.0.1:9000") == nil || len(rt.UpstreamStats()) != 1 {
		t.Fatal("upstream inspection returned incomplete state")
	}
	if err := rt.ValidatePlugins(cfg); err != nil {
		t.Fatalf("ValidatePlugins: %v", err)
	}
	if _, err := buildTLSConfig(config.TLSConfig{CertPEM: []byte("bad"), KeyPEM: []byte("bad")}); err == nil {
		t.Fatal("invalid TLS pair was accepted")
	}
}

// BenchmarkConnLimiter measures the acquire/release cycle on the three paths a
// listener can be in:
//
//	unlimited   — max_conns: 0; acquire is a single load and never reserves.
//	              (Production skips the limiter entirely in this case — the
//	              resolved listener holds a nil limiter — so this is only the
//	              primitive's floor.)
//	below_limit — a limit is configured and there is room (steady state);
//	at_limit    — the listener is full, so every acquire is refused (the flood
//	              case, where the caller then logs through the rate limiter).
func BenchmarkConnLimiter(b *testing.B) {
	b.Run("unlimited", func(b *testing.B) {
		l := &ConnLimiter{} // max 0 = unlimited
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if !l.Acquire() {
				b.Fatal("unlimited limiter refused a connection")
			}
			l.Release()
		}
	})

	b.Run("below_limit", func(b *testing.B) {
		l := &ConnLimiter{}
		l.max.Store(1 << 30)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if !l.Acquire() {
				b.Fatal("unexpected refusal below the limit")
			}
			l.Release()
		}
	})

	b.Run("at_limit", func(b *testing.B) {
		l := &ConnLimiter{}
		l.max.Store(1)
		l.cur.Store(1) // already at capacity
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if l.Acquire() {
				b.Fatal("acquire succeeded while at the limit")
			}
		}
	})
}

// BenchmarkConnLimiterParallel runs the same cycle with every P on one limiter.
// The cases above measure an atomic the core owns; here both counters sit on one
// cache line that every core has to fetch, which is what happens when a listener
// is at or near max_conns under a real connection burst. Each case is chosen so
// that no assertion can be racy: max_conns 0 never refuses, 1<<30 cannot be
// reached by P goroutines, and 1 with cur already 1 always refuses.
func BenchmarkConnLimiterParallel(b *testing.B) {
	cases := []struct {
		name string
		max  int64
		cur  int64
	}{
		{"unlimited", 0, 0},
		{"below_limit", 1 << 30, 0},
		{"at_limit", 1, 1},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			l := &ConnLimiter{}
			l.max.Store(c.max)
			l.cur.Store(c.cur)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if l.Acquire() {
						l.Release()
					}
				}
			})
		})
	}
}

// A replace policy is only valid when the guest actually declares CapMutation.
// This guards the config-to-engine capability hand-off.
func TestMutationPolicyRequiresGuestCapability(t *testing.T) {
	pc := config.PluginConfig{
		Wasm:           v4Verdict(0),
		DataMode:       config.DataModeHeaders,
		BodyPolicy:     config.BodyPolicyRequest,
		MutationPolicy: config.MutationPolicyReplace,
		Timeout:        time.Second,
	}
	if _, err := newRegistry().preparePlugin("edge", pc); err == nil {
		t.Fatal("replace policy loaded a guest without CapMutation")
	}
}
