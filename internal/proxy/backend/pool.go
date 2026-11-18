package backend

import "wasmgate/internal/config"

// Pool groups upstreams under a single balancing strategy.
//
// A pool is (re)built from a configuration snapshot, but the *Upstream objects
// it points at come from the long-lived registry, so their active counters keep
// counting across reloads (see proxy/runtime). Callers hold the exact *Upstream
// they reserved, so a pool rebuilt during a reload can never decrement the wrong
// counter.
type Pool struct {
	name        string      // pool name from the config (used in logs)
	servers     []*Upstream // upstreams in config order; balancers rely on this order
	balancer    Balancer    // strategy chosen from PoolConfig.Balance
	dialRetries int         // extra dial attempts across servers in the pool
}

// Source supplies the shared upstream objects used by a pool. The production
// runtime implements it with its long-lived registry.
type Source interface {
	Upstream(addr string) *Upstream
}

// NewPool constructs a pool together with its balancer. balance is the raw
// config string; endpoint objects come from source so their counters survive
// reloads. An empty server list is a programming error and panics.
func NewPool(name string, servers []string, balance string, source Source) *Pool {
	if len(servers) == 0 {
		panic("proxy: NewPool called with an empty server list")
	}
	ups := make([]*Upstream, 0, len(servers))
	for _, s := range servers {
		ups = append(ups, source.Upstream(s))
	}
	return &Pool{name: name, servers: ups, balancer: newBalancer(balance)}
}

// Next reserves the chosen upstream. The caller MUST call Release with the
// returned pointer exactly once when the connection is done.
func (p *Pool) Next() *Upstream {
	u := p.balancer.Next(p.servers)
	u.active.Add(1)
	return u
}

// Release returns a reserved slot. Safe to call with nil.
//
// The decrement is guarded so a stray double-Release cannot drive the counter
// negative: a negative active count would make least_conn consider the upstream
// "extra free" and funnel ever more connections to it.
func (p *Pool) Release(u *Upstream) {
	if u == nil {
		return
	}
	for {
		cur := u.active.Load()
		if cur <= 0 {
			return // nothing to release (defensive)
		}
		if u.active.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// Name returns the pool name (used in logs).
func (p *Pool) Name() string { return p.name }

// SetDialRetries configures the number of additional upstream attempts.
func (p *Pool) SetDialRetries(n int) { p.dialRetries = n }

// DialRetries returns the configured number of additional attempts.
func (p *Pool) DialRetries() int { return p.dialRetries }

// BalancerName identifies the configured strategy.
func (p *Pool) BalancerName() string { return p.balancer.Name() }

// ConfigureBreaker applies one circuit-breaker policy to every endpoint.
func (p *Pool) ConfigureBreaker(cb config.CircuitBreakerConfig) {
	for _, u := range p.servers {
		u.ConfigureBreaker(cb)
	}
}
