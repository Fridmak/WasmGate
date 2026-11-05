package observe

import (
	"fmt"
	"io"
	"runtime"
	"sync/atomic"
	"time"
)

// UpstreamStat is a read-only view of one backend's live in-flight connection
// count. The proxy package produces it from its registry (which survives
// reloads); the metrics endpoint consumes it.
type UpstreamStat struct {
	Server string
	Active int64
}

// Metrics is the proxy's own set of counters and gauges.
//
// Design constraint: the data path must not slow down. Every method here is a
// handful of atomic adds called ONCE PER CONNECTION (never per chunk), lock-free
// and allocation-free, so the cost is a few nanoseconds against a connection
// setup measured in microseconds. Bytes are not counted per chunk: each relay
// direction returns the total from io.Copy and adds it once when it finishes.
//
// All methods are safe on a nil *Metrics (they become no-ops), so callers that
// do not need metrics — tests, embeddings — pass nil and pay nothing. Rendering
// (WritePrometheus, Snapshot) is a cold path used by the admin endpoint and may
// allocate.
type Metrics struct {
	startedAt time.Time

	accepted      atomic.Int64 // total client connections accepted
	closed        atomic.Int64 // total connection handlers finished
	active        atomic.Int64 // connections currently being proxied
	rejectedLimit atomic.Int64 // connections refused by max_conns
	rejectedRate  atomic.Int64 // connections refused by per-IP rate_limit
	dialFailed    atomic.Int64 // upstream dials that failed
	bytes         atomic.Int64 // bytes relayed, both directions combined

	reloads       atomic.Uint64 // successful configuration reloads
	configVersion atomic.Uint64 // version of the current configuration snapshot
}

// NewMetrics returns an empty Metrics with its uptime clock started.
func NewMetrics() *Metrics {
	return &Metrics{startedAt: time.Now()}
}

// Accept records an accepted client connection.
func (m *Metrics) Accept() {
	if m == nil {
		return
	}
	m.accepted.Add(1)
	m.active.Add(1)
}

// Close records a finished connection handler. Every accepted connection is
// eventually closed (rejected ones too), so active returns to zero.
func (m *Metrics) Close() {
	if m == nil {
		return
	}
	m.closed.Add(1)
	m.active.Add(-1)
}

// RejectLimit records a connection refused because max_conns was reached.
func (m *Metrics) RejectLimit() {
	if m == nil {
		return
	}
	m.rejectedLimit.Add(1)
}

// RejectRate records a connection refused by the per-IP rate limiter.
func (m *Metrics) RejectRate() {
	if m == nil {
		return
	}
	m.rejectedRate.Add(1)
}

// DialFail records a failed dial to an upstream.
func (m *Metrics) DialFail() {
	if m == nil {
		return
	}
	m.dialFailed.Add(1)
}

// AddBytes adds relayed bytes to the total. Called once per finished relay
// direction with that direction's io.Copy total, not per chunk.
func (m *Metrics) AddBytes(n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.bytes.Add(n)
}

// SetVersion records the configuration version without counting a reload (used
// at startup).
func (m *Metrics) SetVersion(v uint64) {
	if m == nil {
		return
	}
	m.configVersion.Store(v)
}

// Reload records a successful configuration reload and its new version.
func (m *Metrics) Reload(v uint64) {
	if m == nil {
		return
	}
	m.reloads.Add(1)
	m.configVersion.Store(v)
}

// MetricsSnapshot is a consistent-ish read of every metric at one point in
// time ("consistent-ish": the counters are read one by one, so a concurrent
// update can land between reads — fine for reporting, never used for control
// flow).
type MetricsSnapshot struct {
	ConnectionsAccepted      int64
	ConnectionsClosed        int64
	ConnectionsActive        int64
	ConnectionsLimitRejected int64
	ConnectionsRateRejected  int64
	UpstreamDialFailures     int64
	RelayedBytes             int64
	Reloads                  uint64
	ConfigVersion            uint64
	Uptime                   time.Duration
}

// Snapshot returns the current metric values. Zero value for a nil Metrics.
func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	return MetricsSnapshot{
		ConnectionsAccepted:      m.accepted.Load(),
		ConnectionsClosed:        m.closed.Load(),
		ConnectionsActive:        m.active.Load(),
		ConnectionsLimitRejected: m.rejectedLimit.Load(),
		ConnectionsRateRejected:  m.rejectedRate.Load(),
		UpstreamDialFailures:     m.dialFailed.Load(),
		RelayedBytes:             m.bytes.Load(),
		Reloads:                  m.reloads.Load(),
		ConfigVersion:            m.configVersion.Load(),
		Uptime:                   time.Since(m.startedAt),
	}
}

// WritePrometheus renders the metrics in the Prometheus text exposition format
// (version 0.0.4), so any scraper or Grafana can consume the endpoint as-is.
// Cold path; allocates freely. A nil Metrics renders all zeros.
func (m *Metrics) WritePrometheus(w io.Writer, upstreams []UpstreamStat) {
	s := m.Snapshot()

	// The helpers take the value as `any` so counters (int64) and gauges built
	// from uint64 fields render without a narrowing conversion gosec would
	// flag; the values here cannot approach the overflow boundary.
	counter := func(name, help string, v any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %v\n", name, help, name, name, v)
	}
	gauge := func(name, help string, v any) {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %v\n", name, help, name, name, v)
	}

	counter("wasmgate_connections_accepted_total", "Client connections accepted.", s.ConnectionsAccepted)
	counter("wasmgate_connections_closed_total", "Connection handlers finished.", s.ConnectionsClosed)
	gauge("wasmgate_connections_active", "Connections currently being proxied.", s.ConnectionsActive)
	counter("wasmgate_connections_limit_rejected_total", "Connections refused because max_conns was reached.", s.ConnectionsLimitRejected)
	counter("wasmgate_connections_rate_rejected_total", "Connections refused by the per-IP rate limiter.", s.ConnectionsRateRejected)
	counter("wasmgate_upstream_dial_failures_total", "Upstream dials that failed.", s.UpstreamDialFailures)
	counter("wasmgate_relayed_bytes_total", "Bytes relayed in both directions.", s.RelayedBytes)
	counter("wasmgate_reloads_total", "Successful configuration reloads.", s.Reloads)
	gauge("wasmgate_config_version", "Version of the current configuration snapshot.", s.ConfigVersion)
	fmt.Fprintf(w, "# HELP wasmgate_uptime_seconds %s\n# TYPE wasmgate_uptime_seconds gauge\nwasmgate_uptime_seconds %.3f\n", "Seconds since process start.", s.Uptime.Seconds())

	if len(upstreams) > 0 {
		fmt.Fprintf(w, "# HELP wasmgate_upstream_active %s\n# TYPE wasmgate_upstream_active gauge\n", "In-flight connections per upstream server.")
		for _, u := range upstreams {
			fmt.Fprintf(w, "wasmgate_upstream_active{server=\"%s\"} %d\n", escapeLabel(u.Server), u.Active)
		}
	}

	// A minimal set of runtime gauges — enough to spot goroutine leaks and
	// memory growth from a dashboard without a full client_golang dependency.
	// ReadMemStats stops the world briefly; acceptable at scrape cadence.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	gauge("go_goroutines", "Number of goroutines.", int64(runtime.NumGoroutine()))
	gauge("go_mem_heap_alloc_bytes", "Bytes of allocated heap objects.", ms.HeapAlloc)
}

// escapeLabel makes s safe inside a Prometheus label value: only the quote,
// backslash and newline characters need escaping in the text format.
func escapeLabel(s string) string {
	r := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"', '\\':
			r = append(r, '\\', s[i])
		case '\n':
			r = append(r, '\\', 'n')
		default:
			r = append(r, s[i])
		}
	}
	return string(r)
}
