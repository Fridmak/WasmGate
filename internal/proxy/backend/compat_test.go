package backend

// These aliases keep the moved backend tests focused on balancing semantics;
// production callers use the exported backend API directly.
type upstream = Upstream

var sinkUpstream *Upstream

type testSource struct{}

func (testSource) Upstream(addr string) *Upstream { return NewUpstream(addr) }

func newRegistry() Source { return testSource{} }

func newPool(name string, servers []string, balance string, source Source) *Pool {
	return NewPool(name, servers, balance, source)
}
