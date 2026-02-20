package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"gateway/internal/metrics"
	"gateway/internal/proxy"
)

// MaxRequestBodyBytes es el límite de tamaño del body para POST /api/agent/chat (mitiga DoS por bodies enormes).
const MaxRequestBodyBytes = 512 * 1024 // 512 KB

// ChatRequest matches the orquestador contract from n8n.
type ChatRequest struct {
	Message   string     `json:"message"`
	SessionID int        `json:"session_id"`
	Config    ChatConfig `json:"config"`
}

// ChatConfig is the config object inside ChatRequest.
type ChatConfig struct {
	NombreBot    string `json:"nombre_bot"`
	IdEmpresa    int    `json:"id_empresa"`
	RolBot       string `json:"rol_bot"`
	TipoBot      string `json:"tipo_bot"`
	Objetivo     string `json:"objetivo_principal"`
	Modalidad    string `json:"modalidad"`
	FraseSaludo  string `json:"frase_saludo"`
	FraseDes     string `json:"frase_des"`
	FraseEsc     string `json:"frase_esc"`
	Personalidad string `json:"personalidad"`
	// Optional fields n8n may send (forwarded to agents)
	DuracionCitaMinutos *int   `json:"duracion_cita_minutos,omitempty"`
	Slots               *int   `json:"slots,omitempty"`
	AgendarUsuario      *bool  `json:"agendar_usuario,omitempty"`
	AgendarSucursal     *bool  `json:"agendar_sucursal,omitempty"`
	UsuarioID           *int   `json:"usuario_id,omitempty"`
	CorreoUsuario      string `json:"correo_usuario,omitempty"`
}

// ChatResponse matches the orquestador response to n8n.
type ChatResponse struct {
	Reply     string  `json:"reply"`
	SessionID int     `json:"session_id"`
	AgentUsed *string `json:"agent_used,omitempty"`
	Action    string  `json:"action"`
}

// ChatHandler handles POST /api/agent/chat.
type ChatHandler struct {
	Invoker *proxy.Invoker
}

// ServeHTTP implements http.Handler.
func (h *ChatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Limitar tamaño del body por petición para evitar DoS (bodies de MB/GB).
	body := http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)
	defer body.Close()

	var req ChatRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"detail": "Body demasiado grande (máx. 512 KB)"})
			return
		}
		slog.Debug("chat decode error", "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "JSON inválido"})
		return
	}

	// Validation (same as orquestador)
	if req.Message == "" || len(req.Message) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "El campo 'message' no puede estar vacío"})
		return
	}
	if req.SessionID < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "El campo 'session_id' debe ser un entero no negativo"})
		return
	}
	if req.Config.IdEmpresa <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "El campo 'config.id_empresa' debe ser un número mayor a 0"})
		return
	}

	agent := proxy.ModalidadToAgent(req.Config.Modalidad)
	contextMap := configToMap(req.Config)

	// Log de entrada: qué llega al gateway y a dónde se deriva.
	slog.Info("→ request entrada",
		"modalidad", req.Config.Modalidad,
		"agent", agent,
		"session_id", req.SessionID,
		"id_empresa", req.Config.IdEmpresa,
		"message_preview", preview(req.Message, 80),
	)

	start := time.Now()
	reply, err := h.Invoker.InvokeAgent(r.Context(), agent, req.Message, req.SessionID, contextMap)
	elapsed := time.Since(start)

	if err != nil {
		metrics.RequestsTotal.WithLabelValues(agent, "error").Inc()
		metrics.RequestDurationSeconds.WithLabelValues(agent).Observe(elapsed.Seconds())
		slog.Warn("agent invoke failed", "agent", agent, "session_id", req.SessionID, "err", err, "duration_ms", elapsed.Milliseconds())
		fallback := "No pude conectar con el agente. Intenta de nuevo en un momento."
		slog.Info("← respuesta n8n (fallback)",
			"agent", agent,
			"session_id", req.SessionID,
			"status", "fallback",
			"reply_preview", preview(fallback, 80),
		)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ChatResponse{
			Reply:     fallback,
			SessionID: req.SessionID,
			AgentUsed: &agent,
			Action:    "delegate",
		})
		return
	}

	metrics.RequestsTotal.WithLabelValues(agent, "ok").Inc()
	metrics.RequestDurationSeconds.WithLabelValues(agent).Observe(elapsed.Seconds())
	slog.Info("← respuesta n8n (ok)",
		"agent", agent,
		"session_id", req.SessionID,
		"duration_ms", elapsed.Milliseconds(),
		"reply_preview", preview(reply, 80),
	)
	resp := ChatResponse{
		Reply:     reply,
		SessionID: req.SessionID,
		AgentUsed: &agent,
		Action:    "delegate",
	}
	writeJSON(w, http.StatusOK, resp)
}

func configToMap(c ChatConfig) map[string]interface{} {
	m := map[string]interface{}{
		"nombre_bot":         c.NombreBot,
		"id_empresa":         c.IdEmpresa,
		"rol_bot":            c.RolBot,
		"tipo_bot":           c.TipoBot,
		"objetivo_principal": c.Objetivo,
		"modalidad":          c.Modalidad,
		"personalidad":       c.Personalidad,
		"frase_saludo":       c.FraseSaludo,
		"frase_des":          c.FraseDes,
		"frase_esc":          c.FraseEsc,
		"correo_usuario":     c.CorreoUsuario,
	}
	if c.DuracionCitaMinutos != nil {
		m["duracion_cita_minutos"] = *c.DuracionCitaMinutos
	}
	if c.Slots != nil {
		m["slots"] = *c.Slots
	}
	if c.AgendarUsuario != nil {
		m["agendar_usuario"] = *c.AgendarUsuario
	}
	if c.AgendarSucursal != nil {
		m["agendar_sucursal"] = *c.AgendarSucursal
	}
	if c.UsuarioID != nil {
		m["usuario_id"] = *c.UsuarioID
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// preview trunca el string a maxLen caracteres y agrega "…" si fue recortado.
func preview(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "…"
}
