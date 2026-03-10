package config

import (
	"fmt"
	"os"

	"github.com/ilyakaznacheev/cleanenv"
	"github.com/joho/godotenv"
)

// Config holds gateway server configuration from environment.
// Agent-specific configuration (URLs, enabled flags) lives in agent.Registry.
type Config struct {
	HTTPPort    int    `env:"GATEWAY_HTTP_PORT" env-default:"8000"`
	CORSOrigins string `env:"CORS_ALLOWED_ORIGINS" env-default:"*"`

	// LogLevel: debug, info, warn, error. En desarrollo usar debug; en produccion info.
	LogLevel string `env:"LOG_LEVEL" env-default:"info"`

	// HTTP server timeouts (seconds). Protect against slowloris and hung connections.
	ReadHeaderTimeoutSec int `env:"GATEWAY_READ_HEADER_TIMEOUT_SEC" env-default:"10"` // max time to read request headers
	ReadTimeoutSec       int `env:"GATEWAY_READ_TIMEOUT_SEC" env-default:"40"`         // max time to read full request (headers + body)
	WriteTimeoutSec      int `env:"GATEWAY_WRITE_TIMEOUT_SEC" env-default:"35"`        // max time to write response; must be > AGENT_TIMEOUT + 5s buffer
	IdleTimeoutSec       int `env:"GATEWAY_IDLE_TIMEOUT_SEC" env-default:"60"`         // max idle time between requests (keep-alive); 0 = disabled

	AgentTimeoutSec int `env:"AGENT_TIMEOUT" env-default:"25"` // must be < GATEWAY_WRITE_TIMEOUT_SEC - 5s
}

// Load reads configuration from environment (and optional .env file).
// In dev: godotenv loads .env into OS env. In Docker: env_file already injects vars.
// godotenv does NOT overwrite existing env vars — real env always wins.
func Load() (*Config, error) {
	if err := godotenv.Load(); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("config: loading .env: %w", err)
		}
	}
	var c Config
	if err := cleanenv.ReadEnv(&c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}
