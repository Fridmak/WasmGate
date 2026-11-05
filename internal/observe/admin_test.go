package observe

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Tests for admin.go: the observability endpoints over a real bound listener.

// discardLogger keeps admin request logging out of the test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestAdminEndpoints(t *testing.T) {
	m := NewMetrics()
	m.Accept()
	upstreams := func() []UpstreamStat {
		return []UpstreamStat{{Server: "127.0.0.1:9001", Active: 1}}
	}
	admin, err := NewAdmin("127.0.0.1:0", m, upstreams, discardLogger())
	if err != nil {
		t.Fatalf("admin listen: %v", err)
	}
	go func() { _ = admin.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = admin.Shutdown(ctx)
	})
	base := "http://" + admin.Addr().String()

	t.Run("metrics", func(t *testing.T) {
		body := getBody(t, base+"/metrics")
		for _, want := range []string{
			"wasmgate_connections_accepted_total 1",
			`wasmgate_upstream_active{server="127.0.0.1:9001"} 1`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("/metrics missing %q:\n%s", want, body)
			}
		}
	})

	t.Run("healthz", func(t *testing.T) {
		var h struct {
			Status        string `json:"status"`
			ConfigVersion uint64 `json:"config_version"`
		}
		if err := json.Unmarshal([]byte(getBody(t, base+"/healthz")), &h); err != nil {
			t.Fatalf("decode healthz: %v", err)
		}
		if h.Status != "ok" {
			t.Errorf("healthz status = %q, want ok", h.Status)
		}
	})

	t.Run("pprof index", func(t *testing.T) {
		// The index page lists the available profiles; a 200 with profile links
		// means the handlers are wired up.
		body := getBody(t, base+"/debug/pprof/")
		if !strings.Contains(body, "goroutine") {
			t.Errorf("/debug/pprof/ did not list profiles:\n%s", body)
		}
	})

	t.Run("reload", func(t *testing.T) {
		admin.SetReload(func(context.Context) (uint64, error) { return 7, nil })
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/reload", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /reload: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /reload: status %d", resp.StatusCode)
		}
		var out struct {
			Status        string `json:"status"`
			ConfigVersion uint64 `json:"config_version"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode reload: %v", err)
		}
		if out.Status != "reloaded" || out.ConfigVersion != 7 {
			t.Fatalf("reload response = %+v", out)
		}
	})
}

// getBody fetches url with a timeout and returns the response body, closing it.
func getBody(t *testing.T, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
