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

	"gateway/internal/config"
	"gateway/internal/handler"
	"gateway/internal/middleware"
	"gateway/internal/proxy"

	"github.com/go-chi/chi/v5"
)

func main() {
	// Cargar config primero para poder usar LOG_LEVEL al crear el logger.
	cfg, err := config.Load()
	if err != nil {
		// Sin logger aún; usar uno mínimo para reportar el error.
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

	invoker := proxy.NewInvoker(cfg)
	chatHandler := &handler.ChatHandler{Invoker: invoker}
	healthHandler := handler.NewHealthHandler(cfg)

	r := chi.NewRouter()
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

	logStartup(cfg, addr)

	srv := &http.Server{
		Addr:    addr,
		Handler: r,
		// ReadHeaderTimeout: tiempo máximo para leer los headers de la petición. Mitiga slowloris.
		ReadHeaderTimeout: readHeaderTimeout,
		// ReadTimeout: tiempo máximo para leer toda la petición (headers + body). Cierra si el cliente tarda demasiado.
		ReadTimeout: readTimeout,
		// WriteTimeout: tiempo máximo para escribir la respuesta. Cierra si el cliente no consume a tiempo.
		WriteTimeout: writeTimeout,
		// IdleTimeout: tiempo máximo que una conexión keep-alive puede estar idle entre peticiones. 0 = sin límite.
		IdleTimeout: idleTimeout,
	}
	go func() {
		slog.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("shutdown", "err", err)
		os.Exit(1)
	}
	slog.Info("stopped")
}

// logStartup imprime un banner con la config relevante del gateway al arrancar.
func logStartup(cfg *config.Config, addr string) {
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
	slog.Info("  Agentes (puntos de conexión)")
	logAgent(cfg, "venta", "Ventas")
	logAgent(cfg, "cita", "Citas")
	logAgent(cfg, "reserva", "Reservas")
	logAgent(cfg, "citas_ventas", "Citas y Ventas")
	slog.Info(dash)
	slog.Info("  Modalidades → Agente")
	slog.Info("    Citas           → cita")
	slog.Info("    Ventas          → venta")
	slog.Info("    Reservas        → reserva")
	slog.Info("    Citas y Ventas  → citas_ventas")
	slog.Info("    (otro/fallback) → cita")
	slog.Info(dash)
	slog.Info("  Endpoints")
	slog.Info("    POST /api/agent/chat")
	slog.Info("    GET  /health")
	slog.Info("    GET  /metrics")
	slog.Info(sep)
}

func logAgent(cfg *config.Config, key, label string) {
	enabled := cfg.AgentEnabled(key)
	url := cfg.AgentURL(key)
	status := "habilitado"
	if !enabled {
		status = "DESHABILITADO"
	}
	slog.Info(fmt.Sprintf("    %-18s [%s] %s", label, status, url))
}

// parseLogLevel convierte el string de env (debug, info, warn, error) a slog.Level. Valor desconocido → info.
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
