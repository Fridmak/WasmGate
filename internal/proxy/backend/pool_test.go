package backend

import "testing"

// Tests for pool.go: the reservation bookkeeping around the balancer.

// TestReleaseNeverGoesNegative guards against a stray double-Release driving the
// counter negative (which would break least_conn).
func TestReleaseNeverGoesNegative(t *testing.T) {
	p := newPool("p", []string{"a"}, "least_conn", newRegistry())
	u := p.Next()
	p.Release(u)
	p.Release(u) // second release must be a no-op
	if got := u.active.Load(); got != 0 {
		t.Fatalf("active = %d after double Release, want 0", got)
	}
}

// TestReleaseNilIsNoop covers the documented "safe to call with nil" contract:
// Handle defers Release unconditionally, while the upstream pointer is only
// meaningful after a successful Next.
func TestReleaseNilIsNoop(t *testing.T) {
	p := newPool("p", []string{"a"}, "round_robin", newRegistry())
	p.Release(nil) // must not panic
	if got := p.servers[0].active.Load(); got != 0 {
		t.Fatalf("active = %d after Release(nil), want 0", got)
	}
}

// TestPoolNextReservesExactlyOnce checks that Next bumps the counter least_conn
// reads, and that Release gives it back.
func TestPoolNextReservesExactlyOnce(t *testing.T) {
	p := newPool("p", []string{"a", "b"}, "round_robin", newRegistry())
	u := p.Next()
	if got := u.active.Load(); got != 1 {
		t.Fatalf("active = %d after Next, want 1", got)
	}
	p.Release(u)
	if got := u.active.Load(); got != 0 {
		t.Fatalf("active = %d after Release, want 0", got)
	}
}

// TestNewPoolRejectsEmptyServerList covers the fail-fast precondition: an empty
// list would make balancer selection panic deep inside the data path.
func TestNewPoolRejectsEmptyServerList(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("newPool with an empty server list did not panic")
		}
	}()
	newPool("p", nil, "round_robin", newRegistry())
}
