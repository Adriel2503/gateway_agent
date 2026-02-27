package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gateway/internal/metrics"
	"gateway/internal/proxy"
)

// MaxRequestBodyBytes es el límite de tamaño del body para POST /api/agent/chat (mitiga DoS por bodies enormes).
const MaxRequestBodyBytes = 512 * 1024 // 512 KB

// ---------------------------------------------------------------------------
// Tipos flexibles: n8n puede enviar bool/int/string indistintamente.
// ---------------------------------------------------------------------------

// FlexBool acepta JSON bool, número (0/1) o string ("0","1","true","false").
type FlexBool struct {
	Valid bool
	Value bool
}

func (f *FlexBool) UnmarshalJSON(data []byte) error {
	s := string(data)
	if s == "null" {
		f.Valid = false
		return nil
	}
	// bool nativo
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		f.Valid, f.Value = true, b
		return nil
	}
	// número
	var n float64
	if err := json.Unmarshal(data, &n); err == nil {
		f.Valid, f.Value = true, n != 0
		return nil
	}
	// string
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		str = strings.ToLower(strings.TrimSpace(str))
		f.Valid = true
		f.Value = str == "1" || str == "true" || str == "yes"
		return nil
	}
	return fmt.Errorf("FlexBool: cannot parse %s", s)
}

// FlexInt acepta JSON número o string numérico ("15", "3796").
type FlexInt struct {
	Valid bool
	Value int
}

func (f *FlexInt) UnmarshalJSON(data []byte) error {
	s := string(data)
	if s == "null" {
		f.Valid = false
		return nil
	}
	// número nativo
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		f.Valid, f.Value = true, n
		return nil
	}
	// float (por si viene 30.0)
	var fl float64
	if err := json.Unmarshal(data, &fl); err == nil {
		f.Valid, f.Value = true, int(fl)
		return nil
	}
	// string numérico
	var str string
	if err := json.Unmarshal(data, &str); err == nil {
		str = strings.TrimSpace(str)
		if v, err := strconv.Atoi(str); err == nil {
			f.Valid, f.Value = true, v
			return nil
		}
		if v, err := strconv.ParseFloat(str, 64); err == nil {
			f.Valid, f.Value = true, int(v)
			return nil
		}
	}
	return fmt.Errorf("FlexInt: cannot parse %s", s)
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
// Los campos opcionales usan FlexBool/FlexInt para tolerar string, número o bool de n8n.
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
	// Campos opcionales que n8n puede enviar como string, número o bool
	DuracionCitaMinutos FlexInt  `json:"duracion_cita_minutos"`
	Slots               FlexInt  `json:"slots"`
	AgendarUsuario      FlexBool `json:"agendar_usuario"`
	AgendarSucursal     FlexBool `json:"agendar_sucursal"`
	IdProspecto         FlexInt  `json:"id_prospecto"`
	UsuarioID           FlexInt  `json:"usuario_id"`
	IdChatbot           FlexInt  `json:"id_chatbot"`
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
	if strings.TrimSpace(req.Message) == "" {
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
	configMap := configToMap(req.Config)
	contextForAgent := map[string]interface{}{"config": configMap}

	// Log de entrada: qué llega al gateway y a dónde se deriva.
	slog.Info("→ request entrada",
		"modalidad", req.Config.Modalidad,
		"agent", agent,
		"session_id", req.SessionID,
		"id_empresa", req.Config.IdEmpresa,
		"id_chatbot", req.Config.IdChatbot.Value,
		"message_preview", preview(req.Message, 80),
	)

	agentCtx, cancel := context.WithTimeout(r.Context(), h.Invoker.AgentTimeout())
	defer cancel()

	start := time.Now()
	reply, url, err := h.Invoker.InvokeAgent(agentCtx, agent, req.Message, req.SessionID, contextForAgent)
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
			URL:       nil,
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

// preview trunca el string a maxLen caracteres y agrega "…" si fue recortado.
func preview(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "…"
}
