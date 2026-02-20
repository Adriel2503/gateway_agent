package handler

import (
	"encoding/json"
	"net/http"
	"time"

	"gateway/internal/config"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const healthCheckTimeout = 2 * time.Second

// HealthHandler handles GET /health. Si cfg != nil, hace un GET a cada agente habilitado
// en su URL de health (base + /health) y devuelve status "ok" o "degraded" según alcance.
type HealthHandler struct {
	Cfg *config.Config
	// client con timeout corto para no bloquear el health
	client *http.Client
}

// NewHealthHandler returns a health handler that checks gateway + agents when Cfg is set.
func NewHealthHandler(cfg *config.Config) *HealthHandler {
	return &HealthHandler{
		Cfg: cfg,
		client: &http.Client{
			Timeout: healthCheckTimeout,
		},
	}
}

// ServeHTTP implements http.Handler.
func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/health" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	// Sin config (no debería pasar): solo proceso vivo.
	if h.Cfg == nil {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"service": "gateway",
		})
		return
	}

	agents := map[string]string{}
	allOK := true
	for _, name := range []string{"venta", "cita", "reserva", "citas_ventas"} {
		if !h.Cfg.AgentEnabled(name) {
			agents[name] = "disabled"
			continue
		}
		healthURL := h.Cfg.AgentHealthURL(name)
		if healthURL == "" {
			agents[name] = "no_url"
			allOK = false
			continue
		}
		resp, err := h.client.Get(healthURL)
		if err != nil {
			agents[name] = "unreachable"
			allOK = false
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			agents[name] = "ok"
		} else {
			agents[name] = "unreachable"
			allOK = false
		}
	}

	status := "ok"
	code := http.StatusOK
	if !allOK {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  status,
		"service": "gateway",
		"agents":  agents,
	})
}

// MetricsHandler returns Prometheus metrics (GET /metrics).
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}
