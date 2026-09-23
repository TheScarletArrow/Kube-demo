package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Режим агента для DaemonSet: `kube-demo agent`. Тот же образ, другая команда.
// На каждой ноде работает ровно один агент и рассказывает о своей ноде.
//
// hostPath не нужен: /proc/loadavg, /proc/uptime, /proc/meminfo и версия ядра
// не изолируются namespace'ами, так что контейнер видит значения самой ноды.

type nodeInfo struct {
	Node         string     `json:"node"`
	Agent        string     `json:"agent"`
	Kernel       string     `json:"kernel"`
	CPUs         int        `json:"cpus"`
	Load         [3]float64 `json:"load"`
	MemTotalMi   int64      `json:"memTotalMi"`
	MemAvailMi   int64      `json:"memAvailableMi"`
	UptimeSecond int64      `json:"uptimeSeconds"`
}

func collectNodeInfo() nodeInfo {
	hostname, _ := os.Hostname()
	info := nodeInfo{
		Node:  env("NODE_NAME", hostname),
		Agent: env("POD_NAME", hostname),
		CPUs:  runtime.NumCPU(),
	}
	info.Kernel, _ = readFirstLine("/proc/sys/kernel/osrelease")

	if v, ok := readFirstLine("/proc/loadavg"); ok {
		for i, f := range strings.Fields(v)[:3] {
			info.Load[i], _ = strconv.ParseFloat(f, 64)
		}
	}
	if v, ok := readFirstLine("/proc/uptime"); ok {
		up, _ := strconv.ParseFloat(strings.Fields(v)[0], 64)
		info.UptimeSecond = int64(up)
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			kb, _ := strconv.ParseInt(f[1], 10, 64)
			switch f[0] {
			case "MemTotal:":
				info.MemTotalMi = kb >> 10
			case "MemAvailable:":
				info.MemAvailMi = kb >> 10
			}
		}
	}
	return info
}

func runAgent() error {
	port := env("AGENT_PORT", "9100")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, collectNodeInfo())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	slog.Info("node agent starting", "port", port, "node", os.Getenv("NODE_NAME"))
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return srv.ListenAndServe()
}

// GET /api/nodes — опрашиваем агентов DaemonSet'а. Их адреса берём из DNS
// headless-сервиса: он возвращает IP всех подов сразу.
func (s *server) handleNodes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	ips, err := net.DefaultResolver.LookupHost(ctx, s.cfg.NodeAgentService)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable,
			fmt.Errorf("DaemonSet node-agent не найден (DNS %s): %w", s.cfg.NodeAgentService, err))
		return
	}

	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		nodes = []nodeInfo{}
		errs  []error
	)
	client := &http.Client{Timeout: 2 * time.Second}
	for _, ip := range ips {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var info nodeInfo
			err := getJSON(ctx, client, "http://"+net.JoinHostPort(ip, s.cfg.NodeAgentPort)+"/info", &info)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", ip, err))
				return
			}
			nodes = append(nodes, info)
		}()
	}
	wg.Wait()
	slices.SortFunc(nodes, func(a, b nodeInfo) int { return strings.Compare(a.Node, b.Node) })

	resp := map[string]any{"servedBy": s.cfg.PodName, "nodes": nodes}
	if len(errs) > 0 {
		resp["error"] = errors.Join(errs...).Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func getJSON(ctx context.Context, c *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
