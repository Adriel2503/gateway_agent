package agent

import (
	"strings"
)

// RouteFunc maps an n8n modalidad string to an agent key.
// Returns empty string if not found.
type RouteFunc func(modalidad string) (agentKey string)

var modalidadMap = map[string]string{
	"citas":          "cita",
	"ventas":         "venta",
	"reservas":       "reserva",
	"citas y ventas": "citas_ventas",
}

// ModalidadToAgent maps n8n modalidad (valores fijos) a clave de agente.
// Devuelve "" si la modalidad no se reconoce.
func ModalidadToAgent(modalidad string) string {
	m := strings.ToLower(strings.TrimSpace(modalidad))
	if agent, ok := modalidadMap[m]; ok {
		return agent
	}
	return ""
}
