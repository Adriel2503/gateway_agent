package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"gateway/internal/agent"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const healthCheckTimeout = 2 * time.Second

// AgentLister provides the list of agents for health checks.
type AgentLister interface {
	All() []agent.AgentInfo
}

// HealthHandler handles GET /health.
type HealthHandler struct {
	agents AgentLister
	client *http.Client
}

// NewHealthHandler returns a health handler that checks all registered agents.
func NewHealthHandler(agents AgentLister) *HealthHandler {
	return &HealthHandler{
		agents: agents,
		client: &http.Client{
			Timeout: healthCheckTimeout,
		},
	}
}

// ServeHTTP implements http.Handler.
func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	agentInfos := h.agents.All()
	agentStatuses := make(map[string]string, len(agentInfos))
	allOK := true

	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, a := range agentInfos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := checkAgent(h.client, a)
			mu.Lock()
			agentStatuses[a.Key] = s
			if s != "ok" && s != "disabled" {
				allOK = false
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

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
		"agents":  agentStatuses,
	})
}

// checkAgent verifica la salud de un agente individual.
func checkAgent(client *http.Client, a agent.AgentInfo) string {
	if !a.Enabled {
		return "disabled"
	}
	if a.HealthURL == "" {
		return "no_url"
	}
	resp, err := client.Get(a.HealthURL)
	if err != nil {
		return "unreachable"
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "ok"
	}
	return "unreachable"
}

// MetricsHandler returns Prometheus metrics (GET /metrics).
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}
