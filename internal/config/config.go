package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/ilyakaznacheev/cleanenv"
)

// Config holds gateway configuration from environment.
type Config struct {
	HTTPPort    int    `env:"GATEWAY_HTTP_PORT" env-default:"8000"`
	CORSOrigins string `env:"CORS_ALLOWED_ORIGINS" env-default:"*"`

	// LogLevel: debug, info, warn, error. En desarrollo usar debug; en producción info.
	LogLevel string `env:"LOG_LEVEL" env-default:"info"`

	// HTTP server timeouts (seconds). Protect against slowloris and hung connections.
	ReadHeaderTimeoutSec int `env:"GATEWAY_READ_HEADER_TIMEOUT_SEC" env-default:"10"` // max time to read request headers
	ReadTimeoutSec       int `env:"GATEWAY_READ_TIMEOUT_SEC" env-default:"30"`         // max time to read full request (headers + body)
	WriteTimeoutSec      int `env:"GATEWAY_WRITE_TIMEOUT_SEC" env-default:"30"`        // max time to write response
	IdleTimeoutSec      int `env:"GATEWAY_IDLE_TIMEOUT_SEC" env-default:"60"`         // max idle time between requests (keep-alive); 0 = disabled

	// URLs de los agentes (puntos de conexión). POST con message, session_id, context; respuesta JSON: reply.
	AgentVentaURL      string `env:"AGENT_VENTA_URL" env-default:"http://localhost:8001/api/chat"`
	AgentCitaURL       string `env:"AGENT_CITA_URL" env-default:"http://localhost:8002/api/chat"`
	AgentReservaURL    string `env:"AGENT_RESERVA_URL" env-default:"http://localhost:8003/api/chat"`
	AgentCitasVentasURL string `env:"AGENT_CITAS_VENTAS_URL" env-default:"http://localhost:8004/api/chat"`

	AgentVentaEnabled      bool `env:"AGENT_VENTA_ENABLED" env-default:"true"`
	AgentCitaEnabled       bool `env:"AGENT_CITA_ENABLED" env-default:"true"`
	AgentReservaEnabled    bool `env:"AGENT_RESERVA_ENABLED" env-default:"true"`
	AgentCitasVentasEnabled bool `env:"AGENT_CITAS_VENTAS_ENABLED" env-default:"true"`

	AgentTimeoutSec int `env:"AGENT_TIMEOUT" env-default:"30"`
}

// Load reads configuration from environment (and optional .env file).
func Load() (*Config, error) {
	_ = cleanenv.ReadEnv(nil) // optional .env via GODOTENV
	var c Config
	if err := cleanenv.ReadEnv(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	// Parse booleans from string env (n8n/docker often send "1"/"0")
	c.AgentVentaEnabled = parseBoolEnv("AGENT_VENTA_ENABLED", c.AgentVentaEnabled)
	c.AgentCitaEnabled = parseBoolEnv("AGENT_CITA_ENABLED", c.AgentCitaEnabled)
	c.AgentReservaEnabled = parseBoolEnv("AGENT_RESERVA_ENABLED", c.AgentReservaEnabled)
	c.AgentCitasVentasEnabled = parseBoolEnv("AGENT_CITAS_VENTAS_ENABLED", c.AgentCitasVentasEnabled)
	return &c, nil
}

func parseBoolEnv(key string, defaultVal bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "1" || v == "true" || v == "yes" {
		return true
	}
	if v == "0" || v == "false" || v == "no" {
		return false
	}
	b, _ := strconv.ParseBool(v)
	return b
}

// AgentURL returns the URL del agente (punto de conexión) para "venta", "cita", "reserva", "citas_ventas".
func (c *Config) AgentURL(agent string) string {
	switch agent {
	case "venta":
		return c.AgentVentaURL
	case "cita":
		return c.AgentCitaURL
	case "reserva":
		return c.AgentReservaURL
	case "citas_ventas":
		return c.AgentCitasVentasURL
	default:
		return ""
	}
}

// AgentEnabled returns whether the agent is enabled.
func (c *Config) AgentEnabled(agent string) bool {
	switch agent {
	case "venta":
		return c.AgentVentaEnabled
	case "cita":
		return c.AgentCitaEnabled
	case "reserva":
		return c.AgentReservaEnabled
	case "citas_ventas":
		return c.AgentCitasVentasEnabled
	default:
		return false
	}
}

// AgentHealthURL returns the health check URL for the agent (scheme+host+/health). Empty if AgentURL is invalid.
func (c *Config) AgentHealthURL(agent string) string {
	base := c.AgentURL(agent)
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	u.Path = "/health"
	u.RawQuery = ""
	return u.String()
}
