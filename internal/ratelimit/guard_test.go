package ratelimit

import (
	"net"
	"testing"
)

func BenchmarkGuardAcquireRelease(b *testing.B) {
	g := NewGuard(1<<30, 1<<30, 64, 56)
	ip := net.ParseIP("192.0.2.1")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !g.Acquire(ip) {
			b.Fatal("limit reached")
		}
		g.Release(ip)
	}
}
