package proxy

// pool groups upstreams under a single balancing strategy.
//
// A pool is (re)built from a configuration snapshot, but the *upstream objects
// it points at come from the long-lived registry, so their active counters keep
// counting across reloads (see state.go). Callers hold the exact *upstream they
// reserved, so a pool rebuilt during a reload can never decrement the wrong
// counter.
type pool struct {
	name        string      // pool name from the config (used in logs)
	servers     []*upstream // upstreams in config order; balancers rely on this order
	balancer    Balancer    // strategy chosen from PoolConfig.Balance
	dialRetries int         // extra dial attempts across servers in the pool
}

// newPool constructs a pool together with its balancer. balance is the raw
// config string; newBalancer maps it to a strategy. Endpoint objects are taken
// from g so their counters survive reloads.
//
// Precondition: servers must be non-empty. Config validation guarantees this,
// and an empty slice here would make balancer selection panic, so we fail fast
// on a programming error.
func newPool(name string, servers []string, balance string, g *registry) *pool {
	if len(servers) == 0 {
		panic("proxy: newPool called with an empty server list")
	}
	ups := make([]*upstream, 0, len(servers))
	for _, s := range servers {
		ups = append(ups, g.upstream(s))
	}
	return &pool{name: name, servers: ups, balancer: newBalancer(balance)}
}

// Next reserves the chosen upstream. The caller MUST call Release with the
// returned pointer exactly once when the connection is done.
func (p *pool) Next() *upstream {
	u := p.balancer.Next(p.servers)
	u.active.Add(1)
	return u
}

// Release returns a reserved slot. Safe to call with nil.
//
// The decrement is guarded so a stray double-Release cannot drive the counter
// negative: a negative active count would make least_conn consider the upstream
// "extra free" and funnel ever more connections to it.
func (p *pool) Release(u *upstream) {
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
func (p *pool) Name() string { return p.name }
