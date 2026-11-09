package observe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
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
	admin, err := NewAdmin("127.0.0.1:0", m, upstreams, discardLogger(), AdminOptions{Pprof: true})
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

func TestAdminAuthHostAndPprofDefault(t *testing.T) {
	m := NewMetrics()
	admin, err := NewAdmin("127.0.0.1:0", m, nil, discardLogger(), AdminOptions{Token: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = admin.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = admin.Shutdown(ctx)
	})
	base := "http://" + admin.Addr().String()

	// No token -> 401.
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("metrics without token: %d, want 401", resp.StatusCode)
	}

	// Correct bearer token -> 200, but pprof is still disabled by default.
	req, _ := http.NewRequest(http.MethodGet, base+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics with token: %d, want 200", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, base+"/debug/pprof/", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pprof without opt-in: %d, want 404", resp.StatusCode)
	}

	// DNS-rebinding-style Host is rejected before auth.
	req, _ = http.NewRequest(http.MethodGet, base+"/metrics", nil)
	req.Host = "evil.example"
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("bad Host: %d, want 421", resp.StatusCode)
	}

	// Bracketed IPv6 loopback without port is accepted.
	req, _ = http.NewRequest(http.MethodGet, base+"/metrics", nil)
	req.Host = "[::1]"
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Host [::1]: %d, want 200", resp.StatusCode)
	}

	// Cross-origin POST /reload is rejected with 403.
	req, _ = http.NewRequest(http.MethodPost, base+"/reload", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("Origin", "http://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin reload: %d, want 403", resp.StatusCode)
	}
}

func TestAdminReloadSingleFlightAndErrorRedaction(t *testing.T) {
	m := NewMetrics()
	admin, err := NewAdmin("127.0.0.1:0", m, nil, discardLogger(), AdminOptions{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = admin.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = admin.Shutdown(ctx)
	})
	base := "http://" + admin.Addr().String()

	// First reload blocks until released.
	started := make(chan struct{})
	release := make(chan struct{})
	var calls int
	var mu sync.Mutex
	admin.SetReload(func(context.Context) (uint64, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		close(started)
		<-release
		return 7, nil
	})

	first := make(chan int, 1)
	go func() {
		resp, err := http.Post(base+"/reload", "", nil)
		if err != nil {
			first <- -1
			return
		}
		_ = resp.Body.Close()
		first <- resp.StatusCode
	}()
	<-started

	// A concurrent reload must be rejected with 409, not queued.
	resp, err := http.Post(base+"/reload", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("concurrent reload: %d, want 409", resp.StatusCode)
	}
	close(release)
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first reload: %d, want 200", code)
	}

	// A failing reload must not leak the underlying error (paths, config
	// fragments) to the caller.
	admin.SetReload(func(context.Context) (uint64, error) {
		return 0, errors.New("secret detail: /etc/wasmgate/config.yaml")
	})
	resp, err = http.Post(base+"/reload", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("failing reload: %d, want 400", resp.StatusCode)
	}
	if strings.Contains(string(body), "secret detail") || strings.Contains(string(body), "/etc/wasmgate") {
		t.Fatalf("reload error leaked internals: %s", body)
	}
}

func TestAdminPprofGuardBlocksHeapGC(t *testing.T) {
	m := NewMetrics()
	admin, err := NewAdmin("127.0.0.1:0", m, nil, discardLogger(), AdminOptions{Pprof: true})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = admin.Serve() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = admin.Shutdown(ctx)
	})
	base := "http://" + admin.Addr().String()

	for _, path := range []string{"/debug/pprof/heap?gc=1", "/debug/pprof/heap?gc=2", "/debug/pprof/goroutine?debug=02"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("GET %s: status %d, want 400", path, resp.StatusCode)
		}
	}
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
