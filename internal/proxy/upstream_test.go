package proxy

import (
	"fmt"
	"sync/atomic"
	"testing"
)

// Tests for upstream.go: the balancing strategies, followed by the benchmark for
// the cost of choosing an upstream.

func TestRoundRobinDistribution(t *testing.T) {
	p := newPool("p", []string{"a", "b", "c"}, "round_robin", newRegistry())
	counts := map[string]int{}
	for i := 0; i < 9; i++ {
		u := p.Next()
		counts[u.addr]++
		p.Release(u)
	}
	for _, a := range []string{"a", "b", "c"} {
		if counts[a] != 3 {
			t.Fatalf("upstream %s got %d picks, want 3", a, counts[a])
		}
	}
}

func TestLeastConnPicksLeastActive(t *testing.T) {
	p := newPool("p", []string{"a", "b"}, "least_conn", newRegistry())
	p.servers[0].active.Store(5)
	p.servers[1].active.Store(1)

	u := p.Next()
	if u.addr != "b" {
		t.Fatalf("least_conn picked %s, want b", u.addr)
	}
}

// TestLeastConnTieGoesToFirst pins the documented tie-break: config order.
func TestLeastConnTieGoesToFirst(t *testing.T) {
	servers := []*upstream{{addr: "a"}, {addr: "b"}}
	if u := newBalancer("least_conn").Next(servers); u.addr != "a" {
		t.Fatalf("tie picked %s, want a (first in config order)", u.addr)
	}
}

// TestNewBalancerUnknownFallsBackToRoundRobin covers the safety net: config
// validation rejects unknown balance values long before this point, so the
// default branch must never produce a nil balancer.
func TestNewBalancerUnknownFallsBackToRoundRobin(t *testing.T) {
	for _, kind := range []string{"", "least_con", "random"} {
		if got := newBalancer(kind).Name(); got != "round_robin" {
			t.Errorf("newBalancer(%q) = %s, want round_robin", kind, got)
		}
	}
	if got := newBalancer("least_conn").Name(); got != "least_conn" {
		t.Errorf("newBalancer(least_conn) = %s, want least_conn", got)
	}
}

// TestBalancersSkipUnhealthyAndFailOpen checks that both round_robin and
// least_conn skip an upstream marked unhealthy while a healthy peer exists, and
// fall back if every upstream is unhealthy.
func TestBalancersSkipUnhealthyAndFailOpen(t *testing.T) {
	for _, kind := range []string{"round_robin", "least_conn"} {
		a := &upstream{addr: "a"}
		b := &upstream{addr: "b"}
		a.MarkUnhealthy()
		bal := newBalancer(kind)
		for i := 0; i < 4; i++ {
			if got := bal.Next([]*upstream{a, b}); got != b {
				t.Fatalf("%s picked unhealthy %s instead of healthy b", kind, got.addr)
			}
		}
		// When all upstreams are marked unhealthy, Next must still return a
		// non-nil endpoint (fail-open).
		b.MarkUnhealthy()
		if got := bal.Next([]*upstream{a, b}); got == nil {
			t.Fatalf("%s returned nil when all upstreams were unhealthy", kind)
		}
	}
}

// benchEndpoints builds n upstreams with staggered active counts, so least_conn
// has a real choice to make.
func benchEndpoints(n int) []*upstream {
	servers := make([]*upstream, n)
	for i := range servers {
		servers[i] = &upstream{addr: "127.0.0.1:9000"}
		servers[i].active.Store(int64(i % 3))
	}
	return servers
}

// BenchmarkBalancer measures the per-connection cost of picking an upstream.
// Both strategies are swept over a few pool sizes: round_robin jumps straight to
// the next index, while least_conn scans the whole pool, so the latter's cost is
// expected to grow with the pool size and the former's to stay flat.
func BenchmarkBalancer(b *testing.B) {
	strategies := []string{"round_robin", "least_conn"}
	sizes := []int{1, 8, 64}
	for _, size := range sizes {
		servers := benchEndpoints(size)
		for _, name := range strategies {
			b.Run(fmt.Sprintf("%s/servers=%d", name, size), func(b *testing.B) {
				bal := newBalancer(name)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					sinkUpstream = bal.Next(servers)
				}
			})
		}
	}

	// baseline_inline is what round_robin does with nothing in between: no
	// Balancer interface, no method call, no *upstream slice. It answers "is
	// 8 ns a lot?" — if it lands on the same figure, the entire cost is the
	// shared atomic cursor (a locked RMW on a cache line) and none of it is our
	// dispatch, so there is nothing left to optimise here.
	b.Run("baseline_inline/servers=64", func(b *testing.B) {
		var cursor atomic.Uint64
		servers := benchEndpoints(64)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			sinkUpstream = servers[(cursor.Add(1)-1)%uint64(len(servers))]
		}
	})
}

// BenchmarkBalancerParallel runs the choice with every P competing for one pool.
// round_robin's cursor and least_conn's active counts are shared by all
// connections, so this is where an uncontended 8 ns turns into cache-line
// traffic between cores. It is the honest number for "N workers dialling at
// once"; the single-goroutine figures above are the per-call floor.
func BenchmarkBalancerParallel(b *testing.B) {
	servers := benchEndpoints(64)
	for _, name := range []string{"round_robin", "least_conn"} {
		b.Run(name, func(b *testing.B) {
			bal := newBalancer(name)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					sinkUpstream = bal.Next(servers)
				}
			})
		})
	}
}
