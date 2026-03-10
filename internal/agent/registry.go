package agent

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
)

// AgentInfo holds the configuration for one downstream agent.
type AgentInfo struct {
	Key       string // e.g. "venta", "cita"
	URL       string // e.g. "http://localhost:8001/api/chat"
	Enabled   bool
	HealthURL string // derived: scheme+host+"/health"
}

// Registry holds all known agents, keyed by agent name.
// Populated from environment variables at startup.
type Registry struct {
	agents map[string]AgentInfo
}

// NewRegistryFromEnv scans os.Environ() for AGENT_*_URL entries and builds the registry.
// Each AGENT_<KEY>_URL defines an agent; AGENT_<KEY>_ENABLED controls whether it is active (default true).
func NewRegistryFromEnv() (*Registry, error) {
	agents := make(map[string]AgentInfo)

	for _, env := range os.Environ() {
		k, v, ok := strings.Cut(env, "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(k, "AGENT_") || !strings.HasSuffix(k, "_URL") {
			continue
		}

		// AGENT_CITAS_VENTAS_URL → "CITAS_VENTAS"
		middle := k[len("AGENT_") : len(k)-len("_URL")]
		if middle == "" {
			continue
		}
		agentKey := strings.ToLower(middle)
		agentURL := strings.TrimSpace(v)
		if agentURL == "" {
			continue
		}

		enabled := parseBoolEnv(fmt.Sprintf("AGENT_%s_ENABLED", middle), true)
		healthURL := deriveHealthURL(agentURL)

		agents[agentKey] = AgentInfo{
			Key:       agentKey,
			URL:       agentURL,
			Enabled:   enabled,
			HealthURL: healthURL,
		}
	}

	if len(agents) == 0 {
		return nil, fmt.Errorf("agent registry: no AGENT_*_URL variables found in environment")
	}

	return &Registry{agents: agents}, nil
}

// Get returns the AgentInfo for the given key, and whether it exists.
func (r *Registry) Get(key string) (AgentInfo, bool) {
	a, ok := r.agents[key]
	return a, ok
}

// All returns all agents sorted alphabetically by key.
func (r *Registry) All() []AgentInfo {
	out := make([]AgentInfo, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Keys returns all agent keys sorted alphabetically.
func (r *Registry) Keys() []string {
	keys := make([]string, 0, len(r.agents))
	for k := range r.agents {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Enabled returns whether the agent is enabled. False if unknown.
func (r *Registry) Enabled(key string) bool {
	a, ok := r.agents[key]
	return ok && a.Enabled
}

// URL returns the agent endpoint URL. Empty if unknown.
func (r *Registry) URL(key string) string {
	a, ok := r.agents[key]
	if !ok {
		return ""
	}
	return a.URL
}

// HealthURL returns the agent health check URL. Empty if unknown.
func (r *Registry) HealthURL(key string) string {
	a, ok := r.agents[key]
	if !ok {
		return ""
	}
	return a.HealthURL
}

// deriveHealthURL parses the agent URL and replaces its path with /health.
func deriveHealthURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	u.Path = "/health"
	u.RawQuery = ""
	return u.String()
}

// parseBoolEnv reads an env var as boolean. Supports "1"/"0"/"true"/"false"/"yes"/"no".
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
	return defaultVal
}
