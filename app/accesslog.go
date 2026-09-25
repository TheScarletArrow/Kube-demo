package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// accessLog пишет access-лог в файл на общем emptyDir-томе.
// Его читает sidecar-контейнер access-log (tail -F) и выводит в свой stdout:
//
//	kubectl logs <pod> -c access-log
//
// Классический паттерн «приложение пишет в файл, sidecar доставляет логи».
type accessLog struct {
	mu   sync.Mutex
	f    *os.File
	size int64
}

const accessLogMaxSize = 5 << 20 // примитивная ротация: обнуляем файл, tail -F это переживает

func openAccessLog(path string) (*accessLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	st, _ := f.Stat()
	return &accessLog{f: f, size: st.Size()}, nil
}

func (a *accessLog) write(r *http.Request, status int, d time.Duration) {
	if a == nil {
		return
	}
	line := fmt.Sprintf("%s %s %s %s %d %s\n",
		time.Now().UTC().Format(time.RFC3339), r.RemoteAddr, r.Method, r.URL.RequestURI(), status, d.Round(time.Microsecond))

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.size > accessLogMaxSize {
		if err := a.f.Truncate(0); err == nil {
			a.size = 0
		}
	}
	n, err := a.f.WriteString(line)
	if err != nil {
		slog.Warn("access log write failed", "err", err)
	}
	a.size += int64(n)
}

func (a *accessLog) Close() {
	if a != nil {
		a.f.Close()
	}
}
