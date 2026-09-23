package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

//go:embed web/index.html
var indexHTML []byte

const (
	maxAuthorLen = 40
	maxTextLen   = 280
	messagesPage = 20
)

type server struct {
	cfg     Config
	store   Store
	started time.Time
	exit    func(code int) // os.Exit, подменяется в тестах

	served       atomic.Int64 // запросов обслужено этим подом (без проб)
	shuttingDown atomic.Bool  // получили SIGTERM
	unreadyUntil atomic.Int64 // unix-nano: до этого момента readiness отвечает 503
	sick         atomic.Bool  // liveness «сломан» — kubelet перезапустит контейнер
}

func newServer(cfg Config, store Store, exit func(int)) *server {
	return &server{cfg: cfg, store: store, started: time.Now(), exit: exit}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /api/hello", s.handleHello)
	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("DELETE /api/stats", s.handleResetStats)
	mux.HandleFunc("GET /api/messages", s.handleListMessages)
	mux.HandleFunc("POST /api/messages", s.handleAddMessage)
	mux.HandleFunc("GET /api/burn", s.handleBurn)

	mux.HandleFunc("POST /api/chaos/crash", s.handleCrash)
	mux.HandleFunc("POST /api/chaos/sick", s.handleSick)
	mux.HandleFunc("POST /api/chaos/unready", s.handleUnready)

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	return s.middleware(mux)
}

// middleware: логирование, подсчёт запросов и Connection: close.
//
// Зачем Connection: close. kube-proxy балансирует СОЕДИНЕНИЯ, а не запросы.
// Браузер держит keep-alive, и без этого заголовка все запросы со страницы
// улетали бы в один и тот же под. Для демо балансировки закрываем соединение
// после каждого ответа. В проде так делать не нужно.
func (s *server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Connection", "close")
		w.Header().Set("X-Pod-Name", s.cfg.PodName)
		s.served.Add(1)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)

		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).Round(time.Microsecond).String(),
			"remote", r.RemoteAddr,
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ---------- основное API ----------

type podInfo struct {
	Pod       string    `json:"pod"`
	Namespace string    `json:"namespace"`
	PodIP     string    `json:"podIP"`
	Node      string    `json:"node"`
	Version   string    `json:"version"`
	Color     string    `json:"color"`
	Greeting  string    `json:"greeting"`
	Storage   string    `json:"storage"`
	StartedAt time.Time `json:"startedAt"`
	Uptime    string    `json:"uptime"`
	Served    int64     `json:"served"`
}

func (s *server) info() podInfo {
	return podInfo{
		Pod:       s.cfg.PodName,
		Namespace: s.cfg.PodNamespace,
		PodIP:     s.cfg.PodIP,
		Node:      s.cfg.NodeName,
		Version:   s.cfg.Version,
		Color:     s.cfg.Color,
		Greeting:  s.cfg.Greeting,
		Storage:   s.store.Kind(),
		StartedAt: s.started,
		Uptime:    time.Since(s.started).Round(time.Second).String(),
		Served:    s.served.Load(),
	}
}

type helloResponse struct {
	podInfo
	PodVisits   int64  `json:"podVisits"`
	TotalVisits int64  `json:"totalVisits"`
	Error       string `json:"error,omitempty"`
}

// GET / — браузеру отдаём UI, а curl/wget получают одну строку текста,
// чтобы было удобно гонять `while true; do curl ...; done`.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
		return
	}

	resp, status := s.hello(r.Context())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)

	visits := fmt.Sprintf("визиты: %d этим подом / %d всего (%s)", resp.PodVisits, resp.TotalVisits, resp.Storage)
	if resp.Error != "" {
		visits = "хранилище недоступно: " + resp.Error
	}
	fmt.Fprintf(w, "%s от %s | node: %s | %s | %s\n",
		resp.Greeting, resp.Pod, resp.Node, resp.Version, visits)
}

// GET /api/hello — засчитывает визит и рассказывает, какой под ответил.
func (s *server) handleHello(w http.ResponseWriter, r *http.Request) {
	resp, status := s.hello(r.Context())
	writeJSON(w, status, resp)
}

func (s *server) hello(ctx context.Context) (helloResponse, int) {
	resp := helloResponse{podInfo: s.info()}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	podVisits, total, err := s.store.RecordVisit(ctx, s.cfg.PodName)
	if err != nil {
		slog.Error("record visit failed", "err", err)
		resp.Error = err.Error()
		return resp, http.StatusServiceUnavailable
	}
	resp.PodVisits, resp.TotalVisits = podVisits, total
	return resp, http.StatusOK
}

// GET /api/stats — сколько визитов обслужил каждый под.
func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"servedBy": s.cfg.PodName,
		"storage":  s.store.Kind(),
		"pods":     stats,
	})
}

func (s *server) handleResetStats(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ResetStats(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servedBy": s.cfg.PodName, "reset": true})
}

// ---------- гостевая книга ----------

func (s *server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.store.Messages(r.Context(), messagesPage)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"servedBy": s.cfg.PodName,
		"storage":  s.store.Kind(),
		"messages": msgs,
	})
}

func (s *server) handleAddMessage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Author string `json:"author"`
		Text   string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("ожидается JSON вида {\"author\": \"...\", \"text\": \"...\"}"))
		return
	}

	in.Author = strings.TrimSpace(in.Author)
	in.Text = strings.TrimSpace(in.Text)
	if in.Author == "" {
		in.Author = "аноним"
	}
	switch {
	case in.Text == "":
		writeError(w, http.StatusBadRequest, errors.New("текст сообщения пустой"))
		return
	case utf8.RuneCountInString(in.Text) > maxTextLen:
		writeError(w, http.StatusBadRequest, fmt.Errorf("текст длиннее %d символов", maxTextLen))
		return
	case utf8.RuneCountInString(in.Author) > maxAuthorLen:
		writeError(w, http.StatusBadRequest, fmt.Errorf("имя длиннее %d символов", maxAuthorLen))
		return
	}

	msg, err := s.store.AddMessage(r.Context(), Message{Author: in.Author, Text: in.Text, Pod: s.cfg.PodName})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

// ---------- нагрузка и хаос ----------

// GET /api/burn?ms=200 — честно жжёт CPU указанное время. Нужен для демо HPA.
func (s *server) handleBurn(w http.ResponseWriter, r *http.Request) {
	ms, err := strconv.Atoi(r.URL.Query().Get("ms"))
	if err != nil || ms <= 0 {
		ms = 100
	}
	ms = min(ms, 5000)

	deadline := time.Now().Add(time.Duration(ms) * time.Millisecond)
	sum := sha256.Sum256([]byte(s.cfg.PodName))
	rounds := 0
	for time.Now().Before(deadline) {
		for range 1000 {
			sum = sha256.Sum256(sum[:])
		}
		rounds++
	}
	writeJSON(w, http.StatusOK, map[string]any{"pod": s.cfg.PodName, "burnedMs": ms, "rounds": rounds})
}

// POST /api/chaos/crash — процесс падает с exit 1. Смотрим, как растёт RESTARTS.
func (s *server) handleCrash(w http.ResponseWriter, _ *http.Request) {
	slog.Warn("crash requested via API, exiting with code 1")
	writeJSON(w, http.StatusAccepted, map[string]any{"pod": s.cfg.PodName, "action": "crash"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.exit(1)
	}()
}

// POST /api/chaos/sick — процесс жив, но /healthz начинает отвечать 500.
// Через failureThreshold проб kubelet сам перезапустит контейнер.
func (s *server) handleSick(w http.ResponseWriter, _ *http.Request) {
	s.sick.Store(true)
	slog.Warn("liveness probe will fail from now on")
	writeJSON(w, http.StatusAccepted, map[string]any{"pod": s.cfg.PodName, "action": "sick"})
}

// POST /api/chaos/unready?seconds=30 — под временно выпадает из балансировки,
// но НЕ перезапускается. Смотрим на READY 0/1 и на EndpointSlice.
func (s *server) handleUnready(w http.ResponseWriter, r *http.Request) {
	sec, err := strconv.Atoi(r.URL.Query().Get("seconds"))
	if err != nil || sec <= 0 {
		sec = 30
	}
	sec = min(sec, 600)

	until := time.Now().Add(time.Duration(sec) * time.Second)
	s.unreadyUntil.Store(until.UnixNano())
	slog.Warn("readiness probe will fail", "until", until.Format(time.RFC3339))
	writeJSON(w, http.StatusAccepted, map[string]any{"pod": s.cfg.PodName, "action": "unready", "seconds": sec})
}

// ---------- пробы ----------

// /healthz — liveness: «процесс жив и не завис». Внешние зависимости сюда
// специально не проверяем, иначе падение БД перезапустило бы все поды.
func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	if s.sick.Load() {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "sick"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// /readyz — readiness: «готов принимать трафик». Под с 503 убирается из
// endpoints сервиса, но не перезапускается.
func (s *server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	switch {
	case s.shuttingDown.Load():
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "shutting down"})
	case time.Now().UnixNano() < s.unreadyUntil.Load():
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unready (chaos)"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response failed", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
