package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestServer(t *testing.T) (*server, http.Handler) {
	t.Helper()
	cfg := Config{
		PodName:  "kube-demo-abc-12345",
		NodeName: "worker-1",
		Version:  "1.0.0",
		Color:    "royalblue",
		Greeting: "Привет",
	}
	srv := newServer(cfg, newMemoryStore(), func(int) {})
	return srv, srv.routes()
}

func do(t *testing.T, h http.Handler, method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestIndexNegotiatesContent(t *testing.T) {
	_, h := newTestServer(t)

	html := do(t, h, "GET", "/", "", "Accept", "text/html,application/xhtml+xml")
	if ct := html.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("browser should get HTML, got %q", ct)
	}

	text := do(t, h, "GET", "/", "", "Accept", "*/*")
	if ct := text.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("curl should get text, got %q", ct)
	}
	if !strings.Contains(text.Body.String(), "kube-demo-abc-12345") {
		t.Fatalf("text response should contain pod name: %q", text.Body.String())
	}
	if text.Header().Get("Connection") != "close" {
		t.Fatal("expected Connection: close for per-request balancing")
	}
}

func TestHelloCountsVisits(t *testing.T) {
	_, h := newTestServer(t)

	for want := int64(1); want <= 3; want++ {
		rec := do(t, h, "GET", "/api/hello", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		got := decode[helloResponse](t, rec)
		if got.PodVisits != want || got.TotalVisits != want {
			t.Fatalf("visit %d: got pod=%d total=%d", want, got.PodVisits, got.TotalVisits)
		}
		if got.Pod != "kube-demo-abc-12345" || got.Storage != "memory" {
			t.Fatalf("unexpected info: %+v", got.podInfo)
		}
	}

	stats := decode[struct{ Pods []PodStat }](t, do(t, h, "GET", "/api/stats", ""))
	if len(stats.Pods) != 1 || stats.Pods[0].Visits != 3 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	do(t, h, "DELETE", "/api/stats", "")
	stats = decode[struct{ Pods []PodStat }](t, do(t, h, "GET", "/api/stats", ""))
	if len(stats.Pods) != 0 {
		t.Fatalf("stats should be empty after reset: %+v", stats)
	}
}

func TestMessages(t *testing.T) {
	_, h := newTestServer(t)

	for _, tc := range []struct {
		body string
		code int
	}{
		{`{"author":"Антон","text":"Работает!"}`, http.StatusCreated},
		{`{"text":"без имени"}`, http.StatusCreated},
		{`{"author":"x","text":"   "}`, http.StatusBadRequest},
		{`{"text":"` + strings.Repeat("я", maxTextLen+1) + `"}`, http.StatusBadRequest},
		{`не json`, http.StatusBadRequest},
	} {
		if rec := do(t, h, "POST", "/api/messages", tc.body); rec.Code != tc.code {
			t.Errorf("POST %.30s: got %d, want %d (%s)", tc.body, rec.Code, tc.code, rec.Body)
		}
	}

	list := decode[struct{ Messages []Message }](t, do(t, h, "GET", "/api/messages", ""))
	if len(list.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(list.Messages))
	}
	if newest := list.Messages[0]; newest.Author != "аноним" || newest.Pod != "kube-demo-abc-12345" {
		t.Fatalf("unexpected newest message: %+v", newest)
	}
}

func TestProbesAndChaos(t *testing.T) {
	srv, h := newTestServer(t)

	if rec := do(t, h, "GET", "/healthz", ""); rec.Code != http.StatusOK {
		t.Fatalf("healthz: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/readyz", ""); rec.Code != http.StatusOK {
		t.Fatalf("readyz: %d", rec.Code)
	}

	do(t, h, "POST", "/api/chaos/unready?seconds=60", "")
	if rec := do(t, h, "GET", "/readyz", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz after unready: %d", rec.Code)
	}
	srv.unreadyUntil.Store(time.Now().Add(-time.Second).UnixNano())
	if rec := do(t, h, "GET", "/readyz", ""); rec.Code != http.StatusOK {
		t.Fatalf("readyz should recover: %d", rec.Code)
	}

	do(t, h, "POST", "/api/chaos/sick", "")
	if rec := do(t, h, "GET", "/healthz", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("healthz after sick: %d", rec.Code)
	}

	srv.shuttingDown.Store(true)
	if rec := do(t, h, "GET", "/readyz", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz while shutting down: %d", rec.Code)
	}
}

func TestCrashCallsExit(t *testing.T) {
	exited := make(chan int, 1)
	srv := newServer(Config{PodName: "p"}, newMemoryStore(), func(code int) { exited <- code })

	if rec := do(t, srv.routes(), "POST", "/api/chaos/crash", ""); rec.Code != http.StatusAccepted {
		t.Fatalf("crash: %d", rec.Code)
	}
	select {
	case code := <-exited:
		if code != 1 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exit was not called")
	}
}

func TestChaosRequiresPost(t *testing.T) {
	_, h := newTestServer(t)
	if rec := do(t, h, "GET", "/api/chaos/crash", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET crash should be 405, got %d", rec.Code)
	}
}

func TestProbesAreNotCounted(t *testing.T) {
	srv, h := newTestServer(t)
	do(t, h, "GET", "/healthz", "")
	do(t, h, "GET", "/readyz", "")
	do(t, h, "GET", "/api/hello", "")
	if got := srv.served.Load(); got != 1 {
		t.Fatalf("served = %d, want 1", got)
	}
}
