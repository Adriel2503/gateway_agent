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
	Reply string  `json:"reply"`
	URL   *string `json:"url"`
}

// agentResult holds reply and optional url from the agent for circuit breaker.
type agentResult struct {
	Reply string
	URL   *string
}

// Invoker calls agent HTTP endpoints with optional circuit breaker.
type Invoker struct {
	cfg    *config.Config
	client *http.Client
	cbs    map[string]*gobreaker.CircuitBreaker[agentResult]
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
	cbs := make(map[string]*gobreaker.CircuitBreaker[agentResult], len(agents))
	for _, name := range agents {
		name := name
		cbs[name] = gobreaker.NewCircuitBreaker[agentResult](gobreaker.Settings{
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

// InvokeAgent calls the agent by name with the given payload. Returns reply, optional url, or error.
func (inv *Invoker) InvokeAgent(ctx context.Context, agent string, message string, sessionID int, contextMap map[string]interface{}) (reply string, url *string, err error) {
	if !inv.cfg.AgentEnabled(agent) {
		return "", nil, fmt.Errorf("agent %s is disabled", agent)
	}
	agentURL := inv.cfg.AgentURL(agent)
	if agentURL == "" {
		return "", nil, fmt.Errorf("no URL configured for agent %s", agent)
	}

	cb, ok := inv.cbs[agent]
	if !ok {
		return "", nil, fmt.Errorf("unknown agent: %s", agent)
	}

	res, err := cb.Execute(func() (agentResult, error) {
		return inv.doHTTP(ctx, agentURL, message, sessionID, contextMap)
	})
	if err != nil {
		return "", nil, err
	}
	return res.Reply, res.URL, nil
}

func (inv *Invoker) doHTTP(ctx context.Context, agentURL string, message string, sessionID int, contextMap map[string]interface{}) (agentResult, error) {
	body := AgentRequest{
		Message:   message,
		SessionID: sessionID,
		Context:   contextMap,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return agentResult{}, fmt.Errorf("marshal request: %w", err)
	}

	slog.Debug("→ enviando a agente",
		"url", agentURL,
		"session_id", sessionID,
		"message_preview", msgPreview(message, 80),
		"context_keys", contextKeys(contextMap),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentURL, bytes.NewReader(raw))
	if err != nil {
		return agentResult{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	start := time.Now()
	resp, err := inv.client.Do(req)
	if err != nil {
		slog.Warn("← agente no respondió", "url", agentURL, "session_id", sessionID, "err", err, "duration_ms", time.Since(start).Milliseconds())
		return agentResult{}, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("← agente respondió con error", "url", agentURL, "session_id", sessionID, "status", resp.StatusCode, "duration_ms", time.Since(start).Milliseconds())
		return agentResult{}, fmt.Errorf("agent returned status %d", resp.StatusCode)
	}

	var out AgentResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return agentResult{}, fmt.Errorf("decode response: %w", err)
	}

	url := out.URL
	if url != nil && *url == "" {
		url = nil
	}

	slog.Debug("← respuesta agente",
		"url", agentURL,
		"session_id", sessionID,
		"duration_ms", time.Since(start).Milliseconds(),
		"reply_preview", msgPreview(out.Reply, 80),
	)

	return agentResult{Reply: out.Reply, URL: url}, nil
}

// msgPreview trunca el string a maxLen caracteres para logs.
func msgPreview(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "…"
}

// contextKeys devuelve las claves del mapa de contexto (útil para logs de debug sin exponer valores).
func contextKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
