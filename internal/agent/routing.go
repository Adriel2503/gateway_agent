package agent

import (
	"log/slog"
	"strings"
)

// RouteFunc maps an n8n modalidad string to an agent key.
type RouteFunc func(modalidad string) (agentKey string)

var modalidadMap = map[string]string{
	"citas":          "cita",
	"ventas":         "venta",
	"reservas":       "reserva",
	"citas y ventas": "citas_ventas",
}

const fallbackAgent = "cita"

// ModalidadToAgent maps n8n modalidad (valores fijos) a clave de agente.
// Devuelve fallbackAgent ("cita") si la modalidad no se reconoce.
func ModalidadToAgent(modalidad string) string {
	m := strings.ToLower(strings.TrimSpace(modalidad))
	if agent, ok := modalidadMap[m]; ok {
		return agent
	}
	slog.Warn("modalidad desconocida, usando fallback",
		"modalidad_recibida", modalidad,
		"modalidad_normalizada", m,
		"fallback", fallbackAgent,
	)
	return fallbackAgent
}
