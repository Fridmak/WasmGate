package observe

import (
	"bytes"
	"strings"
	"testing"
)

// Tests for metrics.go: counter arithmetic, nil-safety and the Prometheus
// rendering, followed by the benchmark that prices the per-connection work.

func TestMetricsLifecycle(t *testing.T) {
	m := NewMetrics()

	m.Accept()
	m.Accept()
	m.Accept()
	m.AddBytes(1024)
	m.Close()
	m.Close()

	s := m.Snapshot()
	if s.ConnectionsAccepted != 3 || s.ConnectionsClosed != 2 || s.ConnectionsActive != 1 {
		t.Fatalf("connection counters: %+v", s)
	}
	if s.RelayedBytes != 1024 {
		t.Fatalf("bytes: %d, want 1024", s.RelayedBytes)
	}
}

func TestMetricsReloadAndVersion(t *testing.T) {
	m := NewMetrics()
	m.SetVersion(1)
	if s := m.Snapshot(); s.Reloads != 0 || s.ConfigVersion != 1 {
		t.Fatalf("after SetVersion: %+v", s)
	}
	m.Reload(5)
	if s := m.Snapshot(); s.Reloads != 1 || s.ConfigVersion != 5 {
		t.Fatalf("after Reload: %+v", s)
	}
}

// TestMetricsNilSafe: every mutator must tolerate a nil Metrics (tests and
// embeddings pass nil to disable metrics), and Snapshot must render zeros.
func TestMetricsNilSafe(t *testing.T) {
	var m *Metrics
	m.Accept()
	m.Close()
	m.RejectLimit()
	m.RejectRate()
	m.RejectPlugin()
	m.DialFail()
	m.AddBytes(1)
	m.SetVersion(2)
	m.Reload(3)

	if s := m.Snapshot(); s != (MetricsSnapshot{}) {
		t.Fatalf("nil Metrics snapshot: %+v, want zero", s)
	}
	var buf bytes.Buffer
	m.WritePrometheus(&buf, []UpstreamStat{{Server: "127.0.0.1:9001", Active: 2}})
	if !strings.Contains(buf.String(), "wasmgate_connections_accepted_total 0") {
		t.Fatalf("nil Metrics did not render zeros:\n%s", buf.String())
	}
}

// TestWritePrometheus checks the exposition format: the line for every metric
// the dashboards will query, plus the per-upstream gauge with its label.
func TestWritePrometheus(t *testing.T) {
	m := NewMetrics()
	m.Accept()
	m.AddBytes(42)

	var buf bytes.Buffer
	m.WritePrometheus(&buf, []UpstreamStat{
		{Server: "127.0.0.1:9001", Active: 2},
		{Server: "127.0.0.1:9002", Active: 0},
	})
	out := buf.String()

	for _, want := range []string{
		"# TYPE wasmgate_connections_accepted_total counter",
		"wasmgate_connections_accepted_total 1",
		"# TYPE wasmgate_connections_active gauge",
		"wasmgate_connections_active 1",
		"# TYPE wasmgate_relayed_bytes_total counter",
		"wasmgate_relayed_bytes_total 42",
		`wasmgate_upstream_active{server="127.0.0.1:9001"} 2`,
		`wasmgate_upstream_active{server="127.0.0.1:9002"} 0`,
		"# TYPE go_goroutines gauge",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestWritePrometheusEscapesLabel(t *testing.T) {
	var buf bytes.Buffer
	NewMetrics().WritePrometheus(&buf, []UpstreamStat{{Server: "a\"b\\c\nd:1", Active: 1}})
	want := `wasmgate_upstream_active{server="a\"b\\c\nd:1"} 1`
	if !strings.Contains(buf.String(), want) {
		t.Fatalf("label not escaped once: want %s in\n%s", want, buf.String())
	}
}

// BenchmarkMetricsConnection measures exactly what the data path adds per
// connection: accept bookkeeping, one bytes add per finished relay direction
// (two here), and close bookkeeping. Everything else in Metrics is cold path.
func BenchmarkMetricsConnection(b *testing.B) {
	m := NewMetrics()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.Accept()
		m.AddBytes(4096)
		m.AddBytes(1024)
		m.Close()
	}
}

// BenchmarkMetricsConnectionParallel is the same work when all P are finishing
// connections at once, so the counters' cache lines are genuinely shared — the
// number a flood of short connections actually pays.
func BenchmarkMetricsConnectionParallel(b *testing.B) {
	m := NewMetrics()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.Accept()
			m.AddBytes(4096)
			m.Close()
		}
	})
}
