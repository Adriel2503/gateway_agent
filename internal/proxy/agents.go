package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"gateway/internal/agent"
	"gateway/internal/domain"
	"gateway/internal/middleware"

	"github.com/sony/gobreaker/v2"
)

// maxConcurrentPerAgent matches MaxConnsPerHost in the Transport.
const maxConcurrentPerAgent = 25

// AgentRequest is the body sent to the agent HTTP endpoint.
type AgentRequest struct {
	Message   string                 `json:"message"`
	SessionID int                    `json:"session_id"`
	IdEmpresa int                    `json:"id_empresa"`
	Config    map[string]interface{} `json:"config"`
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

// Invoker calls agent HTTP endpoints with circuit breaker and backpressure.
type Invoker struct {
	registry *agent.Registry
	client   *http.Client
	cbs      map[string]*gobreaker.CircuitBreaker[agentResult]
	sems     map[string]chan struct{} // M1: backpressure per agent
}

// NewInvoker creates an invoker with shared HTTP client and per-agent circuit breakers.
func NewInvoker(agentTimeout time.Duration, registry *agent.Registry) *Invoker {
	client := &http.Client{
		Timeout: agentTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxConnsPerHost:       25,
			MaxIdleConnsPerHost:   10,
			MaxIdleConns:          50,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     false,
			DisableKeepAlives:     false,
		},
	}

	agents := registry.Keys()
	cbs := make(map[string]*gobreaker.CircuitBreaker[agentResult], len(agents))
	sems := make(map[string]chan struct{}, len(agents))
	for _, name := range agents {
		sems[name] = make(chan struct{}, maxConcurrentPerAgent)
		cbs[name] = gobreaker.NewCircuitBreaker[agentResult](gobreaker.Settings{
			Name:        name,
			MaxRequests: 3,
			Interval:    60 * time.Second,
			Timeout:     30 * time.Second,
			ReadyToTrip: func(counts gobreaker.Counts) bool {
				return counts.ConsecutiveFailures >= 3
			},
			OnStateChange: func(name string, from, to gobreaker.State) {
				slog.Warn("circuit_breaker", "agent", name, "from", from.String(), "to", to.String())
			},
		})
	}
	return &Invoker{registry: registry, client: client, cbs: cbs, sems: sems}
}

// InvokeAgent calls the agent by name with the given payload. Returns reply, optional url, or error.
func (inv *Invoker) InvokeAgent(ctx context.Context, agent string, message string, sessionID int, idEmpresa int, configMap map[string]interface{}) (reply string, url *string, err error) {
	if !inv.registry.Enabled(agent) {
		return "", nil, fmt.Errorf("agent %s is disabled", agent)
	}
	agentURL := inv.registry.URL(agent)
	if agentURL == "" {
		return "", nil, fmt.Errorf("no URL configured for agent %s", agent)
	}

	cb, ok := inv.cbs[agent]
	if !ok {
		return "", nil, fmt.Errorf("unknown agent: %s", agent)
	}

	// M1: backpressure — non-blocking semaphore per agent.
	sem := inv.sems[agent]
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	default:
		return "", nil, fmt.Errorf("agent %s: backpressure (%d concurrent)", agent, maxConcurrentPerAgent)
	}

	// M3: retry inside CB so it sees the final result (1 failure, not 2).
	res, err := cb.Execute(func() (agentResult, error) {
		result, err := inv.doHTTP(ctx, agentURL, message, sessionID, idEmpresa, configMap)
		if err != nil && isRetryable(err) {
			select {
			case <-ctx.Done():
				return agentResult{}, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
			slog.Debug("retry agente", "url", agentURL, "err", err)
			return inv.doHTTP(ctx, agentURL, message, sessionID, idEmpresa, configMap)
		}
		return result, err
	})
	if err != nil {
		return "", nil, err
	}

	// Defensa en profundidad: el agente deberia siempre retornar reply,
	// pero si viene vacio lo detectamos aqui. Fuera de cb.Execute para
	// que el CB no lo cuente como fallo (el agente esta vivo, solo no genero texto).
	if strings.TrimSpace(res.Reply) == "" {
		slog.Warn("agent returned empty reply", "agent", agent, "url", agentURL)
		return "", res.URL, domain.ErrEmptyReply
	}

	return res.Reply, res.URL, nil
}

func (inv *Invoker) doHTTP(ctx context.Context, agentURL string, message string, sessionID int, idEmpresa int, configMap map[string]interface{}) (agentResult, error) {
	body := AgentRequest{
		Message:   message,
		SessionID: sessionID,
		IdEmpresa: idEmpresa,
		Config:    configMap,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return agentResult{}, fmt.Errorf("marshal request: %w", err)
	}

	slog.Debug("→ enviando a agente",
		"url", agentURL,
		"session_id", sessionID,
		"message_preview", domain.Preview(message, domain.DefaultPreviewLen),
		"config_keys", contextKeys(configMap),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentURL, bytes.NewReader(raw))
	if err != nil {
		return agentResult{}, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if rid := middleware.GetRequestID(ctx); rid != "" {
		req.Header.Set("X-Request-ID", rid)
	}

	start := time.Now()
	resp, err := inv.client.Do(req)
	if err != nil {
		slog.Warn("← agente no respondio", "url", agentURL, "session_id", sessionID, "err", err, "duration_ms", time.Since(start).Milliseconds())
		return agentResult{}, fmt.Errorf("http do: %w", err)
	}
	defer func() {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("← agente respondio con error", "url", agentURL, "session_id", sessionID, "status", resp.StatusCode, "duration_ms", time.Since(start).Milliseconds())
		return agentResult{}, fmt.Errorf("agent returned status %d", resp.StatusCode)
	}

	var out AgentResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return agentResult{}, fmt.Errorf("decode response: %w", err)
	}

	u := out.URL
	if u != nil && *u == "" {
		u = nil
	}

	slog.Debug("← respuesta agente",
		"url", agentURL,
		"session_id", sessionID,
		"duration_ms", time.Since(start).Milliseconds(),
		"reply_preview", domain.Preview(out.Reply, domain.DefaultPreviewLen),
	)

	return agentResult{Reply: out.Reply, URL: u}, nil
}

// isRetryable returns true for transient connection errors worth retrying.
// Does NOT retry timeouts (if 5s dial failed, 5.5s won't help) or context cancellation.
func isRetryable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset")
}

// contextKeys devuelve las claves del mapa de contexto (util para logs de debug sin exponer valores).
func contextKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
