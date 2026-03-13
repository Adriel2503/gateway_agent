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
	InvokeAgent(ctx context.Context, agent, message string, sessionID int, idEmpresa int, configMap map[string]interface{}) (reply string, url *string, err error)
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
	IdEmpresa int        `json:"id_empresa"`
	ApiKey    string     `json:"api_key"`
	Config    ChatConfig `json:"config"`
}

// ChatConfig is the config object inside ChatRequest.
// Los campos opcionales de tipo bool usan FlexBool para tolerar string, numero o bool de n8n.
type ChatConfig struct {
	NombreBot string `json:"nombre_bot"`
	Modalidad string `json:"modalidad"`
	FraseSaludo   string `json:"frase_saludo"`
	ArchivoSaludo string `json:"archivo_saludo"`
	Personalidad  string `json:"personalidad"`
	FraseDes      string `json:"frase_des"`
	FraseNoSabe   string `json:"frase_no_sabe"`
	CorreoUsuario string `json:"correo_usuario,omitempty"`
	// Campos opcionales que n8n puede enviar como string, numero o bool
	DuracionCitaMinutos int             `json:"duracion_cita_minutos"`
	Slots               int             `json:"slots"`
	AgendarUsuario      domain.FlexBool `json:"agendar_usuario"`
	AgendarSucursal     domain.FlexBool `json:"agendar_sucursal"`
	UsuarioID           int             `json:"usuario_id"`
	IdChatbot           int             `json:"id_chatbot"`
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
	if req.SessionID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "El campo 'session_id' debe ser un entero mayor a 0"})
		return
	}
	if strings.TrimSpace(req.ApiKey) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "El campo 'api_key' no puede estar vacio"})
		return
	}
	if strings.TrimSpace(req.Config.Modalidad) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "El campo 'config.modalidad' no puede estar vacio"})
		return
	}
	if req.IdEmpresa <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "El campo 'id_empresa' debe ser un numero mayor a 0"})
		return
	}

	agent := h.Router(req.Config.Modalidad)
	if agent == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "Modalidad no reconocida: " + req.Config.Modalidad})
		return
	}
	configMap := configToMap(req.Config)

	// Log de entrada: que llega al gateway y a donde se deriva.
	rid := middleware.GetRequestID(r.Context())
	slog.Info("→ request entrada",
		"request_id", rid,
		"modalidad", req.Config.Modalidad,
		"agent", agent,
		"session_id", req.SessionID,
		"id_empresa", req.IdEmpresa,
		"id_chatbot", req.Config.IdChatbot,
		"message_preview", domain.Preview(req.Message, domain.DefaultPreviewLen),
	)

	agentCtx, cancel := context.WithTimeout(r.Context(), h.AgentTimeout)
	defer cancel()

	start := time.Now()
	reply, url, err := h.Caller.InvokeAgent(agentCtx, agent, req.Message, req.SessionID, req.IdEmpresa, configMap)
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
		writeJSON(w, http.StatusOK, ChatResponse{
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
		"frase_saludo":   c.FraseSaludo,
		"archivo_saludo": c.ArchivoSaludo,
		"personalidad":   c.Personalidad,
		"frase_des":      c.FraseDes,
		"frase_no_sabe":  c.FraseNoSabe,
		"modalidad":      c.Modalidad,
		"correo_usuario": c.CorreoUsuario,
	}
	m["duracion_cita_minutos"] = c.DuracionCitaMinutos
	m["slots"] = c.Slots
	if c.AgendarUsuario.Valid {
		m["agendar_usuario"] = c.AgendarUsuario.Value
	}
	if c.AgendarSucursal.Valid {
		m["agendar_sucursal"] = c.AgendarSucursal.Value
	}
	m["usuario_id"] = c.UsuarioID
	m["id_chatbot"] = c.IdChatbot
	return m
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
