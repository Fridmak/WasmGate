package ratelimit

import (
	"net"
	"sync"
	"testing"
	"time"
)

// Tests for window.go: the sliding-window count, per-IP isolation, the
// IPv4/IPv4-mapped key equivalence, and the benchmarks that price the accept
// path.

var t0 = time.Unix(1_000_000, 0)

func TestAllowUpToMaxThenDeny(t *testing.T) {
	l := New(3, time.Second)
	ip := net.ParseIP("192.0.2.1")
	for i := 0; i < 3; i++ {
		if !l.Allow(ip, t0) {
			t.Fatalf("connection %d refused within the limit", i+1)
		}
	}
	if l.Allow(ip, t0) {
		t.Fatal("4th connection in the same window was allowed")
	}
}

func TestWindowSlides(t *testing.T) {
	l := New(2, time.Second)
	ip := net.ParseIP("192.0.2.1")
	l.Allow(ip, t0)
	l.Allow(ip, t0.Add(500*time.Millisecond))
	if l.Allow(ip, t0.Add(900*time.Millisecond)) {
		t.Fatal("third connection inside the window was allowed")
	}
	// The first two fall out of the window one by one; by 1.6s both are old.
	if !l.Allow(ip, t0.Add(1600*time.Millisecond)) {
		t.Fatal("connection after the window passed was refused")
	}
}

func TestIPsAreIndependent(t *testing.T) {
	l := New(1, time.Second)
	if !l.Allow(net.ParseIP("192.0.2.1"), t0) {
		t.Fatal("first IP refused")
	}
	if !l.Allow(net.ParseIP("192.0.2.2"), t0) {
		t.Fatal("second IP refused because of the first")
	}
}

func TestIPv4AndMappedShareKey(t *testing.T) {
	l := New(1, time.Second)
	if !l.Allow(net.ParseIP("192.0.2.9").To4(), t0) {
		t.Fatal("first connection refused")
	}
	if l.Allow(net.ParseIP("192.0.2.9"), t0) { // 16-byte mapped form of the same IP
		t.Fatal("mapped form counted under a different key")
	}
}

func TestDisabledAllowsEverything(t *testing.T) {
	l := New(0, time.Second)
	ip := net.ParseIP("192.0.2.1")
	for i := 0; i < 100; i++ {
		if !l.Allow(ip, t0) {
			t.Fatal("limit 0 must not refuse")
		}
	}
}

func TestSetMaxTakesEffectWithoutReset(t *testing.T) {
	l := New(10, time.Second)
	ip := net.ParseIP("192.0.2.1")
	for i := 0; i < 3; i++ {
		l.Allow(ip, t0)
	}
	l.SetMax(3)
	if l.Allow(ip, t0) {
		t.Fatal("lowered limit did not apply to the connections already counted")
	}
}

func TestShardResetBoundsMemory(t *testing.T) {
	l := New(1, time.Second)
	for i := 0; i < shardCount*shardCap*2; i++ {
		l.Allow(net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)), t0)
	}
	total := 0
	for i := range l.shards {
		total += len(l.shards[i].keys)
	}
	if total > shardCount*shardCap {
		t.Fatalf("tracked keys = %d, want at most %d", total, shardCount*shardCap)
	}
}

// BenchmarkAllow is one IP hammering the limiter: the single-goroutine cost of
// the check on the accept path.
func BenchmarkAllow(b *testing.B) {
	l := New(1<<30, time.Second)
	ip := net.ParseIP("192.0.2.1")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		l.Allow(ip, t0)
	}
}

// BenchmarkAllowParallel is the same check with all Ps accepting at once, over
// distinct IPs, which is the realistic accept-path mix.
func BenchmarkAllowParallel(b *testing.B) {
	l := New(1<<30, time.Second)
	var seq int
	var mu sync.Mutex
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		mu.Lock()
		seq++
		base := seq
		mu.Unlock()
		ip := net.IPv4(10, byte(base>>8), byte(base), 1)
		for pb.Next() {
			l.Allow(ip, t0)
		}
	})
}
