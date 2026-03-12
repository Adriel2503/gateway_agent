package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gateway/internal/agent"
	"gateway/internal/domain"
	"gateway/internal/middleware"
)

// MaxRequestBodyBytes es el limite de tamano del body para POST /api/agent/chat (mitiga DoS por bodies enormes).
const MaxRequestBodyBytes = 512 * 1024 // 512 KB

const fallbackReply = "No pude conectar con el agente. Intenta de nuevo en un momento."
const emptyReplyMsg = "El agente especializado no pudo generar una respuesta. Intenta de nuevo."

// AgentCaller invokes a downstream agent.
type AgentCaller interface {
	InvokeAgent(ctx context.Context, agent, message string, sessionID int, configMap map[string]interface{}) (reply string, url *string, err error)
}

// MetricsRecorder records request metrics.
type MetricsRecorder interface {
	Record(agent, status string, duration time.Duration)
}

// ---------------------------------------------------------------------------
// Structs de request / response
// ---------------------------------------------------------------------------

// ChatRequest matches the orquestador contract from n8n.
type ChatRequest struct {
	Message   string     `json:"message"`
	SessionID int        `json:"session_id"`
	Config    ChatConfig `json:"config"`
}

// ChatConfig is the config object inside ChatRequest.
// Los campos opcionales usan FlexBool/FlexInt para tolerar string, numero o bool de n8n.
type ChatConfig struct {
	NombreBot     string `json:"nombre_bot"`
	IdEmpresa     int    `json:"id_empresa"`
	Modalidad     string `json:"modalidad"`
	FraseSaludo   string `json:"frase_saludo"`
	ArchivoSaludo string `json:"archivo_saludo"`
	Personalidad  string `json:"personalidad"`
	FraseDes      string `json:"frase_des"`
	FraseNoSabe   string `json:"frase_no_sabe"`
	CorreoUsuario string `json:"correo_usuario,omitempty"`
	// Campos opcionales que n8n puede enviar como string, numero o bool
	DuracionCitaMinutos domain.FlexInt  `json:"duracion_cita_minutos"`
	Slots               domain.FlexInt  `json:"slots"`
	AgendarUsuario      domain.FlexBool `json:"agendar_usuario"`
	AgendarSucursal     domain.FlexBool `json:"agendar_sucursal"`
	IdProspecto         domain.FlexInt  `json:"id_prospecto"`
	UsuarioID           domain.FlexInt  `json:"usuario_id"`
	IdChatbot           domain.FlexInt  `json:"id_chatbot"`
}

// ChatResponse matches the orquestador response to n8n.
type ChatResponse struct {
	Reply     string  `json:"reply"`
	SessionID int     `json:"session_id"`
	AgentUsed *string `json:"agent_used,omitempty"`
	URL       *string `json:"url"`
}

// ChatHandler handles POST /api/agent/chat.
type ChatHandler struct {
	Caller       AgentCaller
	Router       agent.RouteFunc // maps modalidad → agent key
	AgentTimeout time.Duration
	Metrics      MetricsRecorder
}

// ServeHTTP implements http.Handler.
func (h *ChatHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Limitar tamano del body por peticion para evitar DoS (bodies de MB/GB).
	body := http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)
	defer body.Close()

	var req ChatRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"detail": "Body demasiado grande (max. 512 KB)"})
			return
		}
		slog.Debug("chat decode error", "err", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "JSON invalido"})
		return
	}

	// Validation (same as orquestador)
	if strings.TrimSpace(req.Message) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "El campo 'message' no puede estar vacio"})
		return
	}
	if req.SessionID < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "El campo 'session_id' debe ser un entero no negativo"})
		return
	}
	if req.Config.IdEmpresa <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "El campo 'config.id_empresa' debe ser un numero mayor a 0"})
		return
	}

	agent := h.Router(req.Config.Modalidad)
	configMap := configToMap(req.Config)

	// Log de entrada: que llega al gateway y a donde se deriva.
	rid := middleware.GetRequestID(r.Context())
	slog.Info("→ request entrada",
		"request_id", rid,
		"modalidad", req.Config.Modalidad,
		"agent", agent,
		"session_id", req.SessionID,
		"id_empresa", req.Config.IdEmpresa,
		"id_chatbot", req.Config.IdChatbot.Value,
		"message_preview", domain.Preview(req.Message, domain.DefaultPreviewLen),
	)

	agentCtx, cancel := context.WithTimeout(r.Context(), h.AgentTimeout)
	defer cancel()

	start := time.Now()
	reply, url, err := h.Caller.InvokeAgent(agentCtx, agent, req.Message, req.SessionID, configMap)
	elapsed := time.Since(start)

	status := "ok"
	if err != nil {
		status = "error"
	}
	h.Metrics.Record(agent, status, elapsed)

	if err != nil {
		slog.Warn("agent invoke failed", "request_id", rid, "agent", agent, "session_id", req.SessionID, "err", err, "duration_ms", elapsed.Milliseconds())
		fallback := fallbackReply
		if errors.Is(err, domain.ErrEmptyReply) {
			fallback = emptyReplyMsg
		}
		slog.Info("← respuesta n8n (fallback)",
			"request_id", rid,
			"agent", agent,
			"session_id", req.SessionID,
			"status", "fallback",
			"reply_preview", domain.Preview(fallback, domain.DefaultPreviewLen),
		)
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(ChatResponse{
			Reply:     fallback,
			SessionID: req.SessionID,
			AgentUsed: &agent,
			URL:       nil,
		})
		return
	}

	slog.Info("← respuesta n8n (ok)",
		"request_id", rid,
		"agent", agent,
		"session_id", req.SessionID,
		"duration_ms", elapsed.Milliseconds(),
		"reply_preview", domain.Preview(reply, domain.DefaultPreviewLen),
	)
	resp := ChatResponse{
		Reply:     reply,
		SessionID: req.SessionID,
		AgentUsed: &agent,
		URL:       url,
	}
	writeJSON(w, http.StatusOK, resp)
}

func configToMap(c ChatConfig) map[string]interface{} {
	m := map[string]interface{}{
		"nombre_bot":     c.NombreBot,
		"id_empresa":     c.IdEmpresa,
		"frase_saludo":   c.FraseSaludo,
		"archivo_saludo": c.ArchivoSaludo,
		"personalidad":   c.Personalidad,
		"frase_des":      c.FraseDes,
		"frase_no_sabe":  c.FraseNoSabe,
		"modalidad":      c.Modalidad,
		"correo_usuario": c.CorreoUsuario,
	}
	if c.DuracionCitaMinutos.Valid {
		m["duracion_cita_minutos"] = c.DuracionCitaMinutos.Value
	}
	if c.Slots.Valid {
		m["slots"] = c.Slots.Value
	}
	if c.AgendarUsuario.Valid {
		m["agendar_usuario"] = c.AgendarUsuario.Value
	}
	if c.AgendarSucursal.Valid {
		m["agendar_sucursal"] = c.AgendarSucursal.Value
	}
	if c.IdProspecto.Valid {
		m["id_prospecto"] = c.IdProspecto.Value
	}
	if c.UsuarioID.Valid {
		m["usuario_id"] = c.UsuarioID.Value
	}
	if c.IdChatbot.Valid {
		m["id_chatbot"] = c.IdChatbot.Value
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
