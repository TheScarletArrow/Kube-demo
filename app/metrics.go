package main

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

// Метрики в формате Prometheus, написанные руками (без client_golang):
// формат текстовый и простой, а демке хватает счётчиков и одной гистограммы.
//
// Prometheus находит поды по аннотациям prometheus.io/scrape|port|path
// (см. deployment.yaml и k8s/extras/observability/).

var durationBuckets = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5}

type histogram struct {
	counts []int64 // по бакетам, не накопительно
	sum    float64
	count  int64
}

type metrics struct {
	mu        sync.Mutex
	requests  map[[2]string]int64 // {route, code}
	durations map[string]*histogram
	actions   map[string]int64 // хаос, scale, снимки...
}

func newMetrics() *metrics {
	return &metrics{
		requests:  map[[2]string]int64{},
		durations: map[string]*histogram{},
		actions:   map[string]int64{},
	}
}

func (m *metrics) observe(route string, code int, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.requests[[2]string{route, fmt.Sprint(code)}]++
	h, ok := m.durations[route]
	if !ok {
		h = &histogram{counts: make([]int64, len(durationBuckets))}
		m.durations[route] = h
	}
	sec := d.Seconds()
	for i, le := range durationBuckets {
		if sec <= le {
			h.counts[i]++
			break
		}
	}
	h.sum += sec
	h.count++
}

func (m *metrics) action(name string) {
	m.mu.Lock()
	m.actions[name]++
	m.mu.Unlock()
}

func labelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func sortedKeys[K comparable, V any](m map[K]V, less func(a, b K) int) []K {
	keys := make([]K, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, less)
	return keys
}

// GET /metrics
func (s *server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.write(w, s)
}

func (m *metrics) write(w io.Writer, s *server) {
	m.mu.Lock()
	defer m.mu.Unlock()

	fmt.Fprintln(w, "# HELP kube_demo_build_info Версия приложения и под, отдавший метрики.")
	fmt.Fprintln(w, "# TYPE kube_demo_build_info gauge")
	fmt.Fprintf(w, "kube_demo_build_info{version=\"%s\",pod=\"%s\",node=\"%s\",storage=\"%s\"} 1\n",
		labelValue(s.cfg.Version), labelValue(s.cfg.PodName), labelValue(s.cfg.NodeName), s.store.Kind())

	ready := 1
	if s.notReadyReason() != "" {
		ready = 0
	}
	fmt.Fprintln(w, "# HELP kube_demo_ready 1, если /readyz отвечает 200.")
	fmt.Fprintln(w, "# TYPE kube_demo_ready gauge")
	fmt.Fprintf(w, "kube_demo_ready %d\n", ready)

	fmt.Fprintln(w, "# HELP kube_demo_http_requests_total HTTP-запросы по маршрутам и кодам ответа.")
	fmt.Fprintln(w, "# TYPE kube_demo_http_requests_total counter")
	for _, k := range sortedKeys(m.requests, func(a, b [2]string) int { return strings.Compare(a[0]+a[1], b[0]+b[1]) }) {
		fmt.Fprintf(w, "kube_demo_http_requests_total{route=\"%s\",code=\"%s\"} %d\n", labelValue(k[0]), k[1], m.requests[k])
	}

	fmt.Fprintln(w, "# HELP kube_demo_http_request_duration_seconds Время обработки запроса.")
	fmt.Fprintln(w, "# TYPE kube_demo_http_request_duration_seconds histogram")
	for _, route := range sortedKeys(m.durations, strings.Compare) {
		h := m.durations[route]
		var cum int64
		for i, le := range durationBuckets {
			cum += h.counts[i]
			fmt.Fprintf(w, "kube_demo_http_request_duration_seconds_bucket{route=\"%s\",le=\"%g\"} %d\n", labelValue(route), le, cum)
		}
		fmt.Fprintf(w, "kube_demo_http_request_duration_seconds_bucket{route=\"%s\",le=\"+Inf\"} %d\n", labelValue(route), h.count)
		fmt.Fprintf(w, "kube_demo_http_request_duration_seconds_sum{route=\"%s\"} %g\n", labelValue(route), h.sum)
		fmt.Fprintf(w, "kube_demo_http_request_duration_seconds_count{route=\"%s\"} %d\n", labelValue(route), h.count)
	}

	fmt.Fprintln(w, "# HELP kube_demo_actions_total Действия из UI: хаос, scale, снимки и т.п.")
	fmt.Fprintln(w, "# TYPE kube_demo_actions_total counter")
	for _, a := range sortedKeys(m.actions, strings.Compare) {
		fmt.Fprintf(w, "kube_demo_actions_total{action=\"%s\"} %d\n", labelValue(a), m.actions[a])
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintln(w, "# TYPE go_goroutines gauge")
	fmt.Fprintf(w, "go_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintln(w, "# TYPE go_memstats_heap_alloc_bytes gauge")
	fmt.Fprintf(w, "go_memstats_heap_alloc_bytes %d\n", ms.HeapAlloc)
	fmt.Fprintln(w, "# TYPE go_memstats_sys_bytes gauge")
	fmt.Fprintf(w, "go_memstats_sys_bytes %d\n", ms.Sys)
	fmt.Fprintln(w, "# TYPE process_start_time_seconds gauge")
	fmt.Fprintf(w, "process_start_time_seconds %d\n", s.started.Unix())
}
