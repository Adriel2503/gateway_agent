package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gateway/internal/agent"
	"gateway/internal/config"
	"gateway/internal/handler"
	"gateway/internal/metrics"
	"gateway/internal/middleware"
	"gateway/internal/proxy"

	"github.com/go-chi/chi/v5"
)

func main() {
	// Cargar config primero para poder usar LOG_LEVEL al crear el logger.
	cfg, err := config.Load()
	if err != nil {
		// Sin logger aun; usar uno minimo para reportar el error.
		bootstrap := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
		bootstrap.Error("load config", "err", err)
		os.Exit(1)
	}

	level := parseLogLevel(cfg.LogLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				// Formato legible sin nanosegundos: "2026-02-20 23:56:28"
				a.Value = slog.StringValue(a.Value.Time().UTC().Format(time.DateTime))
			}
			return a
		},
	}))
	slog.SetDefault(logger)

	reg, err := agent.NewRegistryFromEnv()
	if err != nil {
		slog.Error("agent registry", "err", err)
		os.Exit(1)
	}

	agentTimeout := time.Duration(cfg.AgentTimeoutSec) * time.Second
	invoker := proxy.NewInvoker(agentTimeout, reg)

	chatHandler := &handler.ChatHandler{
		Caller:       invoker,
		Router:       agent.ModalidadToAgent,
		AgentTimeout: agentTimeout,
		Metrics:      metrics.NewRecorder(),
	}
	healthHandler := handler.NewHealthHandler(reg)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.CORS(cfg.CORSOrigins))

	r.Post("/api/agent/chat", chatHandler.ServeHTTP)
	r.Get("/health", healthHandler.ServeHTTP)
	r.Handle("/metrics", handler.MetricsHandler())

	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"MaravIA Gateway","status":"running","endpoints":{"/api/agent/chat":"POST","/health":"GET","/metrics":"GET"}}`))
	})

	const defaultPort = 8000
	port := cfg.HTTPPort
	if port <= 0 {
		port = defaultPort
	}
	addr := ":" + strconv.Itoa(port)

	// Timeouts: evitan slowloris y conexiones colgadas (valores desde config/env).
	readHeaderTimeout := time.Duration(cfg.ReadHeaderTimeoutSec) * time.Second
	readTimeout := time.Duration(cfg.ReadTimeoutSec) * time.Second
	writeTimeout := time.Duration(cfg.WriteTimeoutSec) * time.Second
	var idleTimeout time.Duration
	if cfg.IdleTimeoutSec > 0 {
		idleTimeout = time.Duration(cfg.IdleTimeoutSec) * time.Second
	}

	logStartup(cfg, reg, addr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigChan:
		slog.Info("signal received", "signal", sig.String())
	case err := <-errCh:
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}

	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), agentTimeout+5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown", "err", err)
		os.Exit(1)
	}
	slog.Info("stopped")
}

// logStartup imprime un banner con la config relevante del gateway al arrancar.
func logStartup(cfg *config.Config, reg *agent.Registry, addr string) {
	sep := "============================================================"
	dash := "------------------------------------------------------------"
	slog.Info(sep)
	slog.Info("  INICIANDO GATEWAY - MaravIA")
	slog.Info(sep)
	slog.Info(fmt.Sprintf("  Host         : %s", addr))
	slog.Info(fmt.Sprintf("  Go version   : %s", runtime.Version()))
	slog.Info(fmt.Sprintf("  Log level    : %s", cfg.LogLevel))
	slog.Info(fmt.Sprintf("  CORS origins : %s", cfg.CORSOrigins))
	slog.Info(dash)
	slog.Info("  Timeouts HTTP del servidor")
	slog.Info(fmt.Sprintf("    ReadHeader  : %ds", cfg.ReadHeaderTimeoutSec))
	slog.Info(fmt.Sprintf("    Read        : %ds", cfg.ReadTimeoutSec))
	slog.Info(fmt.Sprintf("    Write       : %ds", cfg.WriteTimeoutSec))
	slog.Info(fmt.Sprintf("    Idle        : %ds", cfg.IdleTimeoutSec))
	slog.Info(fmt.Sprintf("  Timeout agentes : %ds", cfg.AgentTimeoutSec))
	slog.Info(dash)
	slog.Info("  Agentes (puntos de conexion)")
	for _, a := range reg.All() {
		status := "habilitado"
		if !a.Enabled {
			status = "DESHABILITADO"
		}
		slog.Info(fmt.Sprintf("    %-18s [%s] %s", a.Key, status, a.URL))
	}
	slog.Info(dash)
	slog.Info("  Endpoints")
	slog.Info("    POST /api/agent/chat")
	slog.Info("    GET  /health")
	slog.Info("    GET  /metrics")
	slog.Info(sep)
}

// parseLogLevel convierte el string de env (debug, info, warn, error) a slog.Level. Valor desconocido -> info.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
