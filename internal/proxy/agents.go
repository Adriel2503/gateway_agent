package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gateway/internal/config"

	"github.com/sony/gobreaker/v2"
)

// ModalidadToAgent maps n8n modalidad (valores fijos) a clave de agente. Comparación exacta tras normalizar.
func ModalidadToAgent(modalidad string) string {
	m := strings.ToLower(strings.TrimSpace(modalidad))
	switch m {
	case "citas":
		return "cita"
	case "ventas":
		return "venta"
	case "reservas":
		return "reserva"
	case "citas y ventas":
		return "citas_ventas"
	default:
		return "cita"
	}
}

// AgentRequest is the body sent to the agent HTTP endpoint.
type AgentRequest struct {
	Message   string                 `json:"message"`
	SessionID int                    `json:"session_id"`
	Context   map[string]interface{} `json:"context"`
}

// AgentResponse is the expected response from the agent.
type AgentResponse struct {
	Reply string `json:"reply"`
}

// Invoker calls agent HTTP endpoints with optional circuit breaker.
type Invoker struct {
	cfg    *config.Config
	client *http.Client
	cbs    map[string]*gobreaker.CircuitBreaker[string]
}

// NewInvoker creates an invoker with shared HTTP client and per-agent circuit breakers.
func NewInvoker(cfg *config.Config) *Invoker {
	client := &http.Client{
		Timeout: time.Duration(cfg.AgentTimeoutSec) * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        50,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	agents := []string{"venta", "cita", "reserva", "citas_ventas"}
	cbs := make(map[string]*gobreaker.CircuitBreaker[string], len(agents))
	for _, name := range agents {
		name := name
		cbs[name] = gobreaker.NewCircuitBreaker[string](gobreaker.Settings{
			Name:        name,
			MaxRequests: 3,
			Interval:    60 * time.Second,
			Timeout:     60 * time.Second,
			ReadyToTrip: func(counts gobreaker.Counts) bool {
				return counts.ConsecutiveFailures >= 5
			},
			OnStateChange: func(name string, from, to gobreaker.State) {
				slog.Info("circuit_breaker", "agent", name, "from", from.String(), "to", to.String())
			},
		})
	}
	return &Invoker{cfg: cfg, client: client, cbs: cbs}
}

// InvokeAgent calls the agent by name with the given payload. Returns reply or error.
func (inv *Invoker) InvokeAgent(ctx context.Context, agent string, message string, sessionID int, contextMap map[string]interface{}) (reply string, err error) {
	if !inv.cfg.AgentEnabled(agent) {
		return "", fmt.Errorf("agent %s is disabled", agent)
	}
	url := inv.cfg.AgentURL(agent)
	if url == "" {
		return "", fmt.Errorf("no URL configured for agent %s", agent)
	}

	cb, ok := inv.cbs[agent]
	if !ok {
		return "", fmt.Errorf("unknown agent: %s", agent)
	}

	return cb.Execute(func() (string, error) {
		return inv.doHTTP(ctx, url, message, sessionID, contextMap)
	})
}

func (inv *Invoker) doHTTP(ctx context.Context, url string, message string, sessionID int, contextMap map[string]interface{}) (string, error) {
	body := AgentRequest{
		Message:   message,
		SessionID: sessionID,
		Context:   contextMap,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := inv.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("agent returned status %d", resp.StatusCode)
	}

	var out AgentResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	return out.Reply, nil
}
