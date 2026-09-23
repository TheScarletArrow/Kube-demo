package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrokenReleaseIsNeverReady(t *testing.T) {
	srv := newServer(Config{PodName: "p", ReleaseState: "broken-123"}, newMemoryStore(), func(int) {})
	h := srv.routes()
	if rec := do(t, h, "GET", "/readyz", ""); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "broken") {
		t.Fatalf("readyz: %d %s", rec.Code, rec.Body)
	}
	// liveness при этом в порядке: контейнер не перезапускается, просто не получает трафик.
	if rec := do(t, h, "GET", "/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
}

func TestMetrics(t *testing.T) {
	srv, h := newTestServer(t)
	do(t, h, "GET", "/api/hello", "")
	do(t, h, "GET", "/api/hello", "")
	do(t, h, "GET", "/nope", "")
	do(t, h, "POST", "/api/chaos/unready?seconds=1", "")
	srv.cfg.Version = `v"1`

	rec := do(t, h, "GET", "/metrics", "")
	body := rec.Body.String()
	for _, want := range []string{
		`kube_demo_http_requests_total{route="GET /api/hello",code="200"} 2`,
		`kube_demo_http_requests_total{route="unmatched",code="404"} 1`,
		`kube_demo_http_request_duration_seconds_bucket{route="GET /api/hello",le="+Inf"} 2`,
		`kube_demo_http_request_duration_seconds_count{route="GET /api/hello"} 2`,
		`kube_demo_actions_total{action="unready"} 1`,
		`kube_demo_ready 0`,
		`version="v\"1"`,
		"# TYPE kube_demo_http_request_duration_seconds histogram",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics should contain %s\n%s", want, body)
		}
	}
	if strings.Contains(body, `route="GET /metrics"`) {
		t.Error("scrapes of /metrics itself should not be counted")
	}
}

func TestAccessLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	srv, _ := newTestServer(t)
	var err error
	if srv.access, err = openAccessLog(path); err != nil {
		t.Fatal(err)
	}
	h := srv.routes()
	do(t, h, "GET", "/api/hello", "")
	do(t, h, "GET", "/healthz", "") // пробы в access-лог не пишем
	srv.access.Close()

	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "GET /api/hello 200") {
		t.Fatalf("access log: %q", b)
	}
}

func TestCgroupParsing(t *testing.T) {
	for in, want := range map[string]string{"50000 100000": "500m", "max 100000": "нет", "150000 100000": "1500m"} {
		if got := cpuFromQuota(strings.Fields(in)); got != want {
			t.Errorf("cpu %q: got %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"67108864": "64Mi", "max": "нет", "9223372036854771712": "нет"} {
		if got := memFromBytes(in); got != want {
			t.Errorf("mem %q: got %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]int{"250m": 250, "1": 1000, "1.5": 1500} {
		if got, err := parseMilliCPU(in); err != nil || got != want {
			t.Errorf("parseMilliCPU(%q) = %d, %v", in, got, err)
		}
	}
}

func TestNodesWithoutDaemonSet(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.cfg.NodeAgentService = "node-agent.invalid"
	if rec := do(t, srv.routes(), "GET", "/api/nodes", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", rec.Code)
	}
}

func TestNodesQueriesAgents(t *testing.T) {
	// Настоящий агент на httptest; «DNS headless-сервиса» — сам IP, LookupHost вернёт его же.
	agentMux := http.NewServeMux()
	agentMux.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		info := collectNodeInfo()
		info.Node = "kind-worker"
		writeJSON(w, http.StatusOK, info)
	})
	agent := httptest.NewServer(agentMux)
	defer agent.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(agent.URL, "http://"))

	srv, _ := newTestServer(t)
	srv.cfg.NodeAgentService, srv.cfg.NodeAgentPort = host, port

	rec := do(t, srv.routes(), "GET", "/api/nodes", "")
	got := decode[struct{ Nodes []nodeInfo }](t, rec)
	if rec.Code != http.StatusOK || len(got.Nodes) != 1 || got.Nodes[0].Node != "kind-worker" ||
		got.Nodes[0].CPUs == 0 || got.Nodes[0].Kernel == "" || got.Nodes[0].MemTotalMi == 0 {
		t.Fatalf("nodes: %d %+v", rec.Code, got)
	}
}
