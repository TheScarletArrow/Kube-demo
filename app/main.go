package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Проставляется при сборке: -ldflags "-X main.buildVersion=1.2.3"
var buildVersion = "dev"

func main() {
	// JSON-логи в stdout — kubectl logs и любые лог-коллекторы скажут спасибо.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := loadConfig()

	store, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()

	srv := newServer(cfg, store, os.Exit)
	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting",
			"port", cfg.Port,
			"pod", cfg.PodName,
			"node", cfg.NodeName,
			"version", cfg.Version,
			"storage", store.Kind(),
		)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Graceful shutdown. Порядок важен:
	//  1. readiness начинает отвечать 503;
	//  2. ждём ShutdownDelay — за это время kube-proxy/ingress перестают слать
	//     нам новые запросы (удаление из endpoints происходит асинхронно);
	//  3. дожидаемся уже начатых запросов и выходим.
	// Благодаря этому rolling update проходит без единой ошибки у клиентов.
	srv.shuttingDown.Store(true)
	slog.Info("SIGTERM received, draining", "delay", cfg.ShutdownDelay.String())
	time.Sleep(cfg.ShutdownDelay)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	slog.Info("bye", "served", srv.served.Load())
	return nil
}

func openStore(cfg Config) (Store, error) {
	switch cfg.Storage {
	case "postgres":
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		slog.Info("connecting to postgres", "host", cfg.DB.Host, "db", cfg.DB.Name)
		return newPostgresStore(ctx, cfg.DB.URL())
	case "memory":
		return newMemoryStore(), nil
	default:
		return nil, errors.New("unknown STORAGE " + cfg.Storage + ", expected memory|postgres")
	}
}
