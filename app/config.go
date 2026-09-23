package main

import (
	"net"
	"net/url"
	"os"
	"time"
)

// Config собирается целиком из переменных окружения — ровно так, как это
// принято в Kubernetes: часть приходит из ConfigMap, часть из Secret,
// а часть (имя пода, нода, IP) — через Downward API.
type Config struct {
	Port string

	PodName      string
	PodNamespace string
	PodIP        string
	NodeName     string

	Version  string
	Color    string
	Greeting string

	// Storage: "memory" или "postgres". Если не задано — postgres при наличии DB_HOST.
	Storage string
	DB      DBConfig

	// ShutdownDelay — сколько ждать после SIGTERM, прежде чем перестать принимать
	// запросы. За это время Kubernetes успевает убрать под из endpoints сервиса.
	ShutdownDelay time.Duration
}

type DBConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Name     string
	SSLMode  string
}

func (c DBConfig) URL() string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.User, c.Password),
		Host:   net.JoinHostPort(c.Host, c.Port),
		Path:   "/" + c.Name,
	}
	u.RawQuery = url.Values{"sslmode": {c.SSLMode}}.Encode()
	return u.String()
}

func loadConfig() Config {
	hostname, _ := os.Hostname()

	cfg := Config{
		Port: env("PORT", "8080"),

		PodName:      env("POD_NAME", hostname),
		PodNamespace: env("POD_NAMESPACE", "-"),
		PodIP:        env("POD_IP", "-"),
		NodeName:     env("NODE_NAME", "-"),

		Version:  env("APP_VERSION", buildVersion),
		Color:    env("APP_COLOR", "royalblue"),
		Greeting: env("GREETING", "Привет"),

		DB: DBConfig{
			Host:     os.Getenv("DB_HOST"),
			Port:     env("DB_PORT", "5432"),
			User:     env("DB_USER", "demo"),
			Password: os.Getenv("DB_PASSWORD"),
			Name:     env("DB_NAME", "demo"),
			SSLMode:  env("DB_SSLMODE", "disable"),
		},

		ShutdownDelay: envDuration("SHUTDOWN_DELAY", 5*time.Second),
	}

	cfg.Storage = os.Getenv("STORAGE")
	if cfg.Storage == "" {
		cfg.Storage = "memory"
		if cfg.DB.Host != "" {
			cfg.Storage = "postgres"
		}
	}
	return cfg
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}
