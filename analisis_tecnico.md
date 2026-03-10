# Auditoría Técnica — MaravIA Gateway

> **Rol del auditor:** Senior Go Engineer & Backend Architect, especializado en API Gateways de alta concurrencia, resiliencia, observabilidad y optimización de CPU/memoria/red.
>
> **Fecha:** 2026-02-22 (actualizado 2026-03-10)
>
> **Versión auditada:** commit `60d6056` → actualizado a `f9140c7` (branch `main`)

---

## Índice

1. [Resumen Ejecutivo](#1-resumen-ejecutivo)
2. [Arquitectura Actual](#2-arquitectura-actual)
3. [Hallazgos y Problemas](#3-hallazgos-y-problemas)
   - [🔴 Crítico](#-crítico)
   - [🟡 Medio](#-medio)
   - [🟢 Mejora](#-mejora)
4. [Riesgos de Producción](#4-riesgos-de-producción)
5. [Optimización de Performance](#5-optimización-de-performance)
6. [Resiliencia](#6-resiliencia)
7. [Observabilidad](#7-observabilidad)
8. [Checklist de Producción](#8-checklist-de-producción)
9. [Score de Madurez](#9-score-de-madurez)

---

## 1. Resumen Ejecutivo

El gateway está bien estructurado para un proyecto inicial: layout estándar Go, cliente HTTP compartido, circuit breaker por agente, métricas Prometheus, timeouts de servidor configurables y cierre graceful. **La base es sólida.**

Sin embargo, hay problemas de producción reales:

- ~~Una **race condition de timeouts** que provoca goroutines zombies bajo carga.~~ ✅ Resuelto (C1)
- ~~**Transporte HTTP suboptimizado** (sin `ResponseHeaderTimeout`, sin `MaxConnsPerHost`).~~ ✅ Resuelto (C2)
- **Ausencia de backpressure** (sin límite de concurrencia por agente).
- **CORS mal configurado** (`Allow-Credentials: true` con wildcard).
- **Sin autenticación** de requests entrantes.
- **Gaps en observabilidad** (sin correlation ID, sin métricas de circuit breaker state, sin tracing).

**Refactorización SOLID aplicada (2026-03-10):**
- ✅ OCP: Registry dinámico desde env vars (`AGENT_*_URL`) — agregar agente = solo env var
- ✅ DIP: Interfaces definidas en el consumidor (`AgentCaller`, `AgentLister`, `RouteFunc`)
- ✅ ISP: Config slim (solo servidor) + Registry separado (solo agentes)
- ✅ SRP: Paquetes `domain/` (FlexBool, FlexInt, Preview) y `agent/` (Registry, Routing)
- ✅ Health checks paralelos con `sync.WaitGroup`

**Score actual: 7.0 / 10** (era 6.2). Con las correcciones medias y mejoras restantes puede llegar a **8.5+**.

---

## 2. Arquitectura Actual

### Flujo de datos

```
n8n
 │
 ▼  POST /api/agent/chat
┌─────────────────────────────────────────┐
│            chi Router                   │
│  middleware: Logger → CORS              │
├─────────────────────────────────────────┤
│  ChatHandler                            │
│   ├─ MaxBytesReader (512 KB limit)      │
│   ├─ json.Decode + validación           │
│   ├─ ModalidadToAgent(modalidad)        │
│   └─ Invoker.InvokeAgent(ctx, agent)   │
├─────────────────────────────────────────┤
│  Invoker                                │
│   ├─ AgentEnabled check                 │
│   ├─ CircuitBreaker[agentResult].Execute│
│   └─ shared http.Client → doHTTP       │
└─────────────────────────────────────────┘
           │
           ├──CB──> Agente Venta   :8001/api/chat
           ├──CB──> Agente Cita    :8002/api/chat
           ├──CB──> Agente Reserva :8003/api/chat
           └──CB──> Agente CitasV  :8004/api/chat
```

### Capas identificadas

| Capa | Archivo | Responsabilidad |
|---|---|---|
| Entry point | `cmd/gateway/main.go` | Router, server setup, graceful shutdown |
| Config | `internal/config/config.go` | Env vars → struct (solo servidor HTTP, sin agentes) |
| Agent Registry | `internal/agent/registry.go` | Registro dinámico de agentes desde `AGENT_*_URL` env vars |
| Agent Routing | `internal/agent/routing.go` | Mapeo modalidad → agente (`RouteFunc`) |
| Domain | `internal/domain/flex.go` | Tipos compartidos: `FlexBool`, `FlexInt`, `Preview()` |
| Handler | `internal/handler/chat.go` | Decode, validate, orchestrate, respond (usa interfaz `AgentCaller`) |
| Health | `internal/handler/health.go` | Health check paralelo (`sync.WaitGroup`, usa interfaz `AgentLister`) |
| Proxy | `internal/proxy/agents.go` | HTTP client al agente + circuit breaker (implementa `AgentCaller`) |
| Middleware | `internal/middleware/` | CORS, logging |
| Metrics | `internal/metrics/metrics.go` | Prometheus counters + histogramas |

**Evaluación de capas:** ~~El único punto de acoplamiento menor es que `ChatHandler` depende del tipo concreto `*proxy.Invoker` en lugar de una interfaz.~~ ✅ Resuelto — `ChatHandler` ahora depende de la interfaz `AgentCaller` (definida en el consumidor, convención Go). Dependency Inversion aplicado correctamente.

### Stack tecnológico

| Componente | Tecnología |
|---|---|
| Router | Chi v5 |
| Config | cleanenv (env → struct) |
| Logging | `log/slog` stdlib (JSON estructurado) |
| Métricas | `prometheus/client_golang` |
| Circuit Breaker | `sony/gobreaker v2` (por agente) |
| HTTP Client | `net/http.Client` (pool compartido) |

---

## 3. Hallazgos y Problemas

---

### 🔴 Crítico

---

#### C1 — ✅ RESUELTO — Race condition WriteTimeout vs AgentTimeout: goroutines zombies

**Archivos:** `cmd/gateway/main.go` + `internal/handler/chat.go`

**Estado:** Resuelto en refactor 2026-03-10. Se agregó `context.WithTimeout` explícito en `ChatHandler.ServeHTTP` con `AgentTimeout` (25s). Timeouts ajustados: `AGENT_TIMEOUT=25s`, `WRITE_TIMEOUT=35s`, `READ_TIMEOUT=40s`. Buffer de 10s entre agente y write timeout.

**Descripción original:** El servidor tenía `WriteTimeout=30s` y el cliente de agentes tenía `AGENT_TIMEOUT=30s`. Ambos iguales es el **peor escenario posible**.

**Secuencia de fallo (escenario 1 — agente lento):**

```
t=0s    n8n envía request
t=0s    handler goroutine inicia → llama doHTTP(ctx = r.Context())
t=30s   WriteTimeout dispara → servidor cancela el write deadline de la conexión
        n8n recibe "connection reset" o "504 gateway timeout"
t=30s   r.Context() NO se cancela (WriteTimeout NO cancela el request context)
t=30s   El agente responde justo en ese momento → doHTTP recibe respuesta
t=30s   handler intenta json.Encode(w) → "write: broken pipe"
        Goroutine retorna, pero recursos del agente fueron desperdiciados
```

**Secuencia de fallo (escenario 2 — agente colgado):**

```
t=0s    handler inicia, llama doHTTP
t=30s   WriteTimeout dispara → n8n ya desconectado
t=30s   Agente aún sin responder
t=30s   Goroutine SIGUE VIVA esperando al agente
t=30s+  http.Client.Timeout dispara (desde que se llamó Do()) → doHTTP retorna error
t=30s+  handler intenta escribir fallback → "write: broken pipe"
        Goroutine zombi durante varios segundos adicionales post-WriteTimeout
```

**Causa raíz:** `r.Context()` en `net/http` de Go **no se cancela automáticamente cuando `WriteTimeout` dispara**. Solo se cancela cuando:
1. El cliente cierra la conexión activamente.
2. `http.Server.Shutdown()` es llamado.

Como el handler pasa `r.Context()` directamente a `http.NewRequestWithContext(ctx, ...)`, no hay ningún mecanismo que aborte la llamada al agente cuando el cliente (n8n) ya desconectó.

**Fix — agregar contexto acotado explícitamente en el handler:**

```go
// internal/handler/chat.go — en ServeHTTP, antes de InvokeAgent
agentTimeout := time.Duration(h.Invoker.AgentTimeoutSec()) * time.Second
agentCtx, cancel := context.WithTimeout(r.Context(), agentTimeout)
defer cancel()

reply, url, err := h.Invoker.InvokeAgent(agentCtx, agent, req.Message, req.SessionID, contextForAgent)
```

**Fix — ajustar valores de config para garantizar buffer:**

```env
# Regla: AGENT_TIMEOUT + ~5s buffer < GATEWAY_WRITE_TIMEOUT_SEC
AGENT_TIMEOUT=25
GATEWAY_WRITE_TIMEOUT_SEC=35
GATEWAY_READ_TIMEOUT_SEC=40
```

Esto garantiza que si el agente tarda demasiado, el contexto lo cancela y el handler tiene 5–10 segundos para escribir la respuesta de fallback **antes** de que `WriteTimeout` dispare.

---

#### C2 — ✅ RESUELTO — http.Transport sin `ResponseHeaderTimeout` ni `MaxConnsPerHost`

**Archivo:** `internal/proxy/agents.go`

**Estado:** Resuelto en refactor 2026-03-10. Transport completamente configurado con todos los campos recomendados:
- `DialContext` con Timeout=5s y KeepAlive=30s
- `MaxConnsPerHost=25`, `MaxIdleConnsPerHost=10`, `MaxIdleConns=50`
- `ResponseHeaderTimeout=20s`, `TLSHandshakeTimeout=5s`
- `ForceAttemptHTTP2=false`, `DisableKeepAlives=false`

<details>
<summary>Descripción original del problema (click para expandir)</summary>

**Código que tenía:**

```go
Transport: &http.Transport{
    MaxIdleConns:        50,
    MaxIdleConnsPerHost: 10,
    IdleConnTimeout:     90 * time.Second,
}
```

**Problema 1 — Sin `ResponseHeaderTimeout`:** Un agente puede aceptar la conexión TCP, enviar el status line `HTTP/1.1 200 OK`, y luego **nunca enviar los headers de respuesta**. El goroutine queda bloqueado hasta que `http.Client.Timeout` expire (30s).

**Problema 2 — Sin `MaxConnsPerHost`:** Bajo alta concurrencia, Go puede abrir **conexiones TCP ilimitadas** hacia el mismo agente. 200 requests simultáneas = 200 conexiones TCP al mismo host.

**Problema 3 — Sin `DialContext` con timeout:** Si el host no responde (firewall drop), la conexión espera el timeout de TCP del OS (~2 minutos).

</details>

---

#### C3 — CORS: `Allow-Credentials: true` siempre activo, incluso con `Origin: *`

**Archivo:** `internal/middleware/cors.go:20-22`

**Código actual:**

```go
// Se envía siempre, sin importar si el origin es wildcard o específico
w.Header().Set("Access-Control-Allow-Credentials", "true")
```

**Problema 1 — Violación del estándar CORS:** Cuando `CORS_ALLOWED_ORIGINS=*` (el default actual del `.env`), la combinación `Access-Control-Allow-Origin: *` + `Access-Control-Allow-Credentials: true` es **inválida según la especificación CORS**. Los browsers modernos rechazan silenciosamente estas respuestas — cualquier llamada desde un browser fallará sin mensaje de error claro.

**Problema 2 — Riesgo en producción con origins específicos:** Cuando en producción se configure `CORS_ALLOWED_ORIGINS=https://app.maravia.pe`, el header `Allow-Credentials: true` habilita que ese origen haga requests **credencializados** (con cookies, authorization headers) sin ninguna validación de autenticidad del request. Esto amplía la superficie de ataque si el gateway no tiene autenticación propia.

**Fix:**

```go
func CORS(origins string) func(http.Handler) http.Handler {
    allowed := strings.Split(origins, ",")
    for i := range allowed {
        allowed[i] = strings.TrimSpace(allowed[i])
    }
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            origin := r.Header.Get("Origin")
            allowWildcard := false

            if origin != "" {
                for _, o := range allowed {
                    if o == "*" {
                        w.Header().Set("Access-Control-Allow-Origin", "*")
                        allowWildcard = true
                        break
                    } else if o == origin {
                        w.Header().Set("Access-Control-Allow-Origin", origin)
                        w.Header().Set("Vary", "Origin") // necesario para caches
                        break
                    }
                }
            }

            // Allow-Credentials solo cuando el origin es específico (nunca con wildcard)
            if !allowWildcard {
                w.Header().Set("Access-Control-Allow-Credentials", "true")
            }

            w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
            w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Accept, X-Request-ID")

            if r.Method == http.MethodOptions {
                w.WriteHeader(http.StatusNoContent)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

---

### 🟡 Medio

---

#### M1 — ✅ RESUELTO — Sin backpressure: goroutines acumulables bajo carga

**Archivo:** `internal/proxy/agents.go`

**Estado:** Resuelto 2026-03-10. Semáforo `chan struct{}` por agente, capacidad 25 (= `MaxConnsPerHost`), non-blocking. Si los 25 slots están ocupados → error inmediato "backpressure" → handler devuelve fallback.

**Problema original:** Sin límite de concurrencia hacia los agentes. 200 requests simultáneas = 200 goroutines acumulándose sin control.

---

#### M2 — ✅ RESUELTO — Circuit breaker tarda demasiado en abrir (150 segundos de fallos lentos)

**Archivo:** `internal/proxy/agents.go`

**Estado:** Resuelto 2026-03-10. `ConsecutiveFailures` bajado de 5→3, `Timeout` de 60s→30s, `OnStateChange` log level subido a Warn. Tiempo máximo de exposición: 75s (era 125s).

<details>
<summary>Configuración que tenía (click para expandir)</summary>

```go
ReadyToTrip: func(counts gobreaker.Counts) bool {
    return counts.ConsecutiveFailures >= 5 // ← umbral alto
},
Timeout:  60 * time.Second, // tiempo en estado abierto antes de probar
Interval: 60 * time.Second,
```

**Problema:** Con `ConsecutiveFailures >= 5` y `AGENT_TIMEOUT=30s`:

```
Fallo 1 → espera 30s → error
Fallo 2 → espera 30s → error
Fallo 3 → espera 30s → error
Fallo 4 → espera 30s → error
Fallo 5 → espera 30s → error → BREAKER ABRE
Total: 150 segundos de requests fallando lentamente hacia n8n
```

Durante esos 150 segundos, n8n recibe timeouts o errores, y las goroutines se acumulan.

**Fix:**

```go
cbs[name] = gobreaker.NewCircuitBreaker[agentResult](gobreaker.Settings{
    Name:        name,
    MaxRequests: 3,
    Interval:    60 * time.Second,
    Timeout:     30 * time.Second, // probar recuperación cada 30s (era 60s)
    ReadyToTrip: func(counts gobreaker.Counts) bool {
        return counts.ConsecutiveFailures >= 3 // abrir más rápido (era 5)
    },
    OnStateChange: func(name string, from, to gobreaker.State) {
        slog.Warn("circuit_breaker_state_change",
            "agent", name,
            "from", from.String(),
            "to", to.String(),
        )
        // Actualizar métrica Prometheus (ver sección Observabilidad)
        metrics.CircuitBreakerState.WithLabelValues(name).Set(float64(to))
    },
})
```

Con `AgentTimeout=25s` y umbral=3: máximo **75 segundos** antes de que el breaker abra (vs 150s actuales).

</details>

---

#### M3 — ✅ RESUELTO — Sin retries para errores de red transitorios

**Archivo:** `internal/proxy/agents.go`

**Estado:** Resuelto 2026-03-10. 1 retry dentro de `cb.Execute()` para `connection refused` y `connection reset`. Backoff 500ms. No retryable: timeouts, context cancelado, HTTP responses. El CB ve el resultado final (1 fallo, no 2).

**Problema original:** Un restart del agente Python causaba fallo inmediato sin ningún reintento.

**Fix — 1 retry con backoff mínimo para errores de red:**

```go
func isRetryableError(err error) bool {
    // Solo reintentar errores de conexión/red, no errores de aplicación
    var netErr net.Error
    if errors.As(err, &netErr) {
        return netErr.Timeout() || !netErr.Temporary()
    }
    return strings.Contains(err.Error(), "connection refused") ||
           strings.Contains(err.Error(), "connection reset")
}

func (inv *Invoker) doHTTPWithRetry(ctx context.Context, agentURL string, ...) (agentResult, error) {
    var lastErr error
    for attempt := 0; attempt < 2; attempt++ {
        if attempt > 0 {
            // Backoff mínimo, respetando el contexto padre
            select {
            case <-ctx.Done():
                return agentResult{}, ctx.Err()
            case <-time.After(500 * time.Millisecond):
            }
            slog.Debug("retrying agent call", "url", agentURL, "attempt", attempt+1)
        }
        res, err := inv.doHTTP(ctx, agentURL, ...)
        if err == nil {
            return res, nil
        }
        if isRetryableError(err) {
            lastErr = err
            continue
        }
        return agentResult{}, err // error no retriable → fallo inmediato
    }
    return agentResult{}, fmt.Errorf("after %d attempts: %w", 2, lastErr)
}
```

---

#### M4 — ✅ RESUELTO — Sin X-Request-ID ni correlation ID

**Archivos:** `internal/middleware/request_id.go` (nuevo), `middleware/logger.go`, `handler/chat.go`, `proxy/agents.go`, `cmd/gateway/main.go`

**Estado:** Resuelto 2026-03-10. Middleware `RequestID` genera 16-char hex ID (o usa el de n8n si viene). Se almacena en context, se incluye en todos los logs (logger, handler ×4), se propaga al agente via header `X-Request-ID`, y se devuelve en el response header.

**Problema original:** Imposible correlacionar logs del gateway con logs del agente Python.

---

#### M5 — ✅ RESUELTO — config.Load() no carga el archivo .env en desarrollo local

**Archivo:** `internal/config/config.go`

**Estado:** Resuelto 2026-03-10. `godotenv.Load()` carga `.env` al OS env si existe. No sobreescribe vars existentes (env vars reales siempre ganan). Si `.env` no existe (producción) se ignora silenciosamente.

**Problema original:** `cleanenv.ReadEnv(nil)` era un no-op. En dev local con `go run` el `.env` no se cargaba.

---

#### M6 — ✅ RESUELTO — ModalidadToAgent: fallback silencioso a "cita"

**Archivo:** `internal/agent/routing.go`

**Estado:** Resuelto 2026-03-10. `slog.Warn` con `modalidad_recibida`, `modalidad_normalizada` y `fallback` antes de retornar el agente por defecto. Las modalidades válidas (citas, ventas, reservas, citas y ventas) nunca disparan el warning.

**Problema original:** Modalidad desconocida se ruteaba a "cita" sin aviso en logs.

```go
func ModalidadToAgent(modalidad string) string {
    m := strings.ToLower(strings.TrimSpace(modalidad))
    switch m {
    case "citas":
        return "cita"
    case "ventas":
        return "venta"
    case "reservas":
        return "reserva"
    case "citas y ventas":
        return "citas_ventas"
    default:
        slog.Warn("modalidad desconocida, usando fallback",
            "modalidad_recibida", modalidad,
            "modalidad_normalizada", m,
            "fallback", "cita",
        )
        return "cita"
    }
}
```

Alternativa más estricta: retornar un error y responder HTTP 400 al cliente, forzando a n8n a configurar correctamente el campo.

---

#### M7 — logStartup usa fmt.Sprintf en slog: rompe el structured logging

**Archivo:** `cmd/gateway/main.go`

**Código actual:**

```go
slog.Info(fmt.Sprintf("  Host         : %s", addr))
slog.Info(fmt.Sprintf("  Go version   : %s", runtime.Version()))
```

**Problema:** Esto produce logs con un único campo de mensaje string, imposibles de parsear por herramientas como Grafana Loki, Datadog, Elastic o cualquier sistema de log estructurado. El propósito completo de `slog` es mantener key-value pairs separados.

**Fix:**

```go
slog.Info("gateway_startup",
    "addr", addr,
    "go_version", runtime.Version(),
    "log_level", cfg.LogLevel,
    "cors_origins", cfg.CORSOrigins,
    "agent_timeout_sec", cfg.AgentTimeoutSec,
    "read_header_timeout_sec", cfg.ReadHeaderTimeoutSec,
    "write_timeout_sec", cfg.WriteTimeoutSec,
    "agents_enabled", enabledAgents(cfg),
)
```

---

### 🟢 Mejora

---

#### G1 — Métricas Prometheus insuficientes para diagnóstico de producción

**Estado actual:** solo `gateway_requests_total` y `gateway_request_duration_seconds`.

**Faltan:**

```go
// internal/metrics/metrics.go — adiciones

// Requests en vuelo en este momento (gauge)
var InFlightRequests = promauto.NewGaugeVec(
    prometheus.GaugeOpts{
        Name: "gateway_inflight_requests",
        Help: "Requests activos en vuelo por agente",
    },
    []string{"agent"},
)

// Estado del circuit breaker por agente (0=closed, 1=open, 2=half-open)
var CircuitBreakerState = promauto.NewGaugeVec(
    prometheus.GaugeOpts{
        Name: "gateway_circuit_breaker_state",
        Help: "Estado del circuit breaker por agente (0=closed, 1=open, 2=half-open)",
    },
    []string{"agent"},
)

// HTTP status del upstream por agente
var UpstreamStatusTotal = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Name: "gateway_upstream_http_status_total",
        Help: "Respuestas HTTP del agente upstream por código de status",
    },
    []string{"agent", "status_code"},
)

// Tipo de error del agente
var AgentErrorTotal = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Name: "gateway_agent_error_total",
        Help: "Errores por tipo: timeout, connection_refused, circuit_open, decode_error",
    },
    []string{"agent", "error_type"},
)
```

**Uso en InvokeAgent:**

```go
metrics.InFlightRequests.WithLabelValues(agent).Inc()
defer metrics.InFlightRequests.WithLabelValues(agent).Dec()
```

---

#### G2 — ✅ RESUELTO — Health check hace requests síncronos secuenciales

**Archivo:** `internal/handler/health.go`

**Estado:** Resuelto en refactor 2026-03-10. Health checks ahora ejecutan en paralelo con `sync.WaitGroup` + `sync.Mutex`. Usa interfaz `AgentLister` (que `Registry` implementa) en vez de lista hardcodeada. Timeout máximo de `/health` = 2s (un solo check en paralelo), no 8s.

**Problema original:** Con 4 agentes caídos, cada check esperaba 2s (timeout) antes de pasar al siguiente → hasta 8 segundos para responder `/health`.

---

#### G3 — Graceful shutdown timeout insuficiente para requests en vuelo

**Archivo:** `cmd/gateway/main.go`

**Código actual:**

```go
ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
```

**Problema:** Con `AGENT_TIMEOUT=30s`, si al momento del shutdown hay goroutines esperando respuesta del agente, `srv.Shutdown(10s)` forzará el cierre después de 10 segundos. Las requests en vuelo se cortarán abruptamente, dejando a n8n sin respuesta.

**Fix:** El shutdown timeout debe ser al menos `AGENT_TIMEOUT + un buffer`:

```go
shutdownTimeout := time.Duration(cfg.AgentTimeoutSec+10) * time.Second
ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
```

---

#### G4 — Sin autenticación de requests entrantes

**Descripción:** Cualquier proceso que conozca la IP:puerto del gateway puede enviar requests arbitrarios a `/api/agent/chat`. Si el gateway está expuesto a internet (incluso detrás de un proxy), esto es un vector de abuso y costos.

**Fix mínimo — middleware de API key:**

```go
// internal/middleware/auth.go
func APIKey(validKey string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if validKey == "" {
                // Sin configurar → pasar (permite desactivar en dev)
                next.ServeHTTP(w, r)
                return
            }
            key := r.Header.Get("X-API-Key")
            if key == "" {
                key = r.URL.Query().Get("api_key") // fallback para n8n query param
            }
            if key != validKey {
                http.Error(w, `{"detail":"unauthorized"}`, http.StatusUnauthorized)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

Agregar variable de entorno:

```env
GATEWAY_API_KEY=tu-clave-secreta-aqui
```

---

#### G5 — responseWriter wrapper no implementa http.Flusher

**Archivo:** `internal/middleware/logger.go`

**Código actual:**

```go
type responseWriter struct {
    http.ResponseWriter
    status int
}
```

**Problema:** Si algún handler usa `http.Flusher` para streaming, la type assertion `w.(http.Flusher)` falla silenciosamente porque el wrapper no implementa esa interfaz. Aunque no es un problema hoy (respuestas síncronas), es una deuda técnica.

**Fix:**

```go
func (w *responseWriter) Flush() {
    if f, ok := w.ResponseWriter.(http.Flusher); ok {
        f.Flush()
    }
}
```

---

#### G6 — ✅ RESUELTO — .env y .env.example desincronizados

**Estado:** Resuelto en refactor 2026-03-10. Ya no existen campos hardcodeados de agentes en el Config struct. El Registry escanea dinámicamente `AGENT_*_URL` del entorno — `.env.example` documenta el patrón y los agentes actuales. No hay posibilidad de desincronización porque el código no tiene lista fija de agentes.

**Problema original:** `.env` no incluía `AGENT_CITAS_VENTAS_URL` ni `AGENT_CITAS_VENTAS_ENABLED`, pero el código sí soportaba el agente `citas_ventas`.

---

## 4. Riesgos de Producción

| # | Riesgo | Escenario que lo activa | Impacto | Estado |
|---|---|---|---|---|
| R1 | ~~**Goroutines zombies**~~ | ~~Agente lento (≥30s), WriteTimeout dispara~~ | ~~Memory leak gradual~~ | ✅ Resuelto (C1) |
| R2 | ~~**Cascada de fallos**~~ | ~~1 agente lento satura el pool~~ | ~~Degradación total~~ | ✅ Resuelto (M1 semáforo) |
| R3 | ~~**TCP socket exhaustion**~~ | ~~Alta carga sin `MaxConnsPerHost`~~ | ~~Agente saturado~~ | ✅ Resuelto (C2) |
| R4 | ~~**Breaker tarda en abrir**~~ | ~~5 fallos × 25s = 125s~~ | ~~n8n timeouts en cascada~~ | ✅ Resuelto (M2: 3 fallos, 30s) |
| R5 | ~~**Health check lento**~~ | ~~4 agentes caídos → 8s para `/health`~~ | ~~LB marca gateway como muerto~~ | ✅ Resuelto (G2) |
| R6 | **CORS bug** | Browser hace requests (UI futura) con wildcard + credentials | Requests silenciosamente rechazadas | Pendiente (C3) |
| R7 | **Sin autenticación** | Endpoint accesible desde red no confiable | Abuso, costos de agentes, spam | Pendiente (G4) |
| R8 | ~~**Modalidad silenciosa**~~ | ~~n8n envía modalidad incorrecta~~ | ~~Sin aviso en logs~~ | ✅ Resuelto (M6 warning) |
| R9 | **Shutdown brusco** | Requests en vuelo durante deploy/restart | n8n recibe error en mitad de conversación | Pendiente (G3) |

### Qué falla primero bajo carga

```
Escenario: tráfico creciente de n8n

1. Primero: Agente Python se satura (workers limitados, I/O bound con LLM)
   → Latencias suben de 3s → 15s → 25s

2. Segundo: Goroutines del gateway se acumulan (cada request bloqueada hasta 25s)
   → Pero context.WithTimeout cancela a 25s exactos ✅ (C1 resuelto)
   → Goroutines no se vuelven zombies

3. Tercero: MaxConnsPerHost=25 limita conexiones TCP por agente ✅ (C2 resuelto)
   → Ya no hay socket exhaustion ilimitado

4. ⚠️ PENDIENTE: Sin backpressure (semáforo), el gateway no rechaza requests nuevas
   → Goroutines se acumulan hasta MaxConnsPerHost, pero sin semáforo explícito

5. ⚠️ PENDIENTE: El circuit breaker abre TARDE (5 fallos × 25s = 125s)
```

---

## 5. Optimización de Performance

### http.Transport — Configuración Completa Recomendada

```go
// internal/proxy/agents.go

import "net"

func newTransport() *http.Transport {
    return &http.Transport{
        // Dialer TCP con timeout explícito
        DialContext: (&net.Dialer{
            Timeout:   5 * time.Second,  // falla rápido si el agente no responde TCP
            KeepAlive: 30 * time.Second, // mantiene conexiones vivas entre requests
        }).DialContext,

        // Límites de conexiones (ajustar según carga esperada)
        MaxConnsPerHost:     25,  // máx. activas por host — previene saturar el agente
        MaxIdleConnsPerHost: 10,  // pool de conexiones reutilizables por host
        MaxIdleConns:        50,  // total de conexiones idle en todo el pool
        IdleConnTimeout:     90 * time.Second,

        // Timeouts granulares
        TLSHandshakeTimeout:   5 * time.Second,
        ResponseHeaderTimeout: 20 * time.Second, // el agente debe enviar headers en 20s
        ExpectContinueTimeout: 1 * time.Second,

        // HTTP/1.1 con keepalive (adecuado para agentes Python)
        ForceAttemptHTTP2: false,
        DisableKeepAlives: false, // reutilizar conexiones TCP = menos overhead
    }
}
```

### Timeouts end-to-end correctamente encadenados

```
n8n ──────────────────────────────────────────────── Gateway ─────── Agente
     ReadHeaderTimeout(10s)   WriteTimeout(35s)            AgentTimeout(25s)
     ◄──────────────────►     ◄──────────────────►         ◄──────────────►
                              ◄── ResponseHeaderTimeout(20s) ──►
                              ◄── DialTimeout(5s) ──►

Regla de oro:
  DialTimeout(5s)                          < ResponseHeaderTimeout(20s)
  ResponseHeaderTimeout(20s)               < AgentTimeout(25s)
  AgentTimeout(25s) + buffer(5-10s)        < WriteTimeout(35s)
  WriteTimeout(35s)                        < ReadTimeout(40s)
  ReadTimeout(40s)                        <= IdleTimeout(60s)
```

### ¿HTTP/1.1, HTTP/2, gRPC o WebSocket?

| Protocolo | Ventaja | Cuándo usarlo |
|---|---|---|
| **HTTP/1.1 + keepalive** (actual) | Simple, universal, compatible con FastAPI/Flask | Ahora — mantener |
| **HTTP/2** | Multiplexing (varios streams por TCP), header compression | Cuando los agentes lo soporten y haya >20 req/s por agente |
| **gRPC** | Contrato fuerte (protobuf), streaming bidireccional, mejor performance | Si se rediseñan los agentes en Go o si se requiere streaming real |
| **WebSocket** | Bidireccional, eventos push al usuario | Solo si el usuario final necesita respuestas en streaming real-time |

**Recomendación:** Quedarse en HTTP/1.1 con el Transport mejorado. gRPC es el roadmap ideal para cuando los agentes sean propios y controlados.

### ¿Se necesita una cola (Redis/NATS)?

**No, para el caso de uso actual.** El gateway es un proxy síncrono: n8n espera la respuesta del agente. Una cola añadiría latencia y complejidad sin beneficio neto.

Lo que **sí se necesita** (y es más simple): el **semáforo de concurrencia por agente** (ver M1) para backpressure.

**Cuándo reconsiderar una cola:**
- Si n8n no puede esperar 25–30 segundos y se requiere procesamiento asíncrono con webhook de vuelta.
- Si se requiere durabilidad de mensajes (reintentos persistentes ante crash).
- Si el volumen supera los cientos de requests/segundo y se quiere desacoplar completamente producción de consumo.

### Respuestas grandes: buffers en memoria

```go
// Actual: lee todo el body del agente en memoria con json.Decode
// OK para respuestas de chatbot (<10 KB típicamente)

// Preventivo: agregar LimitReader antes del Decode para proteger contra agentes que devuelvan cuerpos enormes
limitedBody := io.LimitReader(resp.Body, 10*1024*1024) // 10 MB máximo
if err := json.NewDecoder(limitedBody).Decode(&out); err != nil {
    return agentResult{}, fmt.Errorf("decode response: %w", err)
}
```

Para un chatbot de texto esto es más que suficiente. Streaming real (`io.Copy` directo) solo sería necesario si los agentes devuelven archivos o respuestas multi-MB.

---

## 6. Resiliencia

### Tabla de estado actual vs recomendado

| Mecanismo | Estado actual | Acción |
|---|---|---|
| HTTP server ReadHeaderTimeout | ✅ 10s | Mantener |
| HTTP server ReadTimeout | ✅ 40s | ✅ Ajustado (era 30s) |
| HTTP server WriteTimeout | ✅ 35s | ✅ Ajustado (era 30s) |
| HTTP server IdleTimeout | ✅ 60s | Mantener |
| http.Client.Timeout | ✅ 25s | ✅ Ajustado (era 30s) |
| context.WithTimeout en handler | ✅ 25s | ✅ Resuelto (C1) |
| ResponseHeaderTimeout | ✅ 20s | ✅ Resuelto (C2) |
| DialTimeout | ✅ 5s | ✅ Resuelto (C2) |
| MaxConnsPerHost | ✅ 25 | ✅ Resuelto (C2) |
| Circuit breaker por agente | ✅ gobreaker | ✅ Resuelto (M2) — umbral 3, timeout 30s |
| Backpressure / semáforo | ✅ chan struct{} cap 25 | ✅ Resuelto (M1) — non-blocking, per agent |
| Retry con backoff | ✅ 1 retry, 500ms | ✅ Resuelto (M3) — connection refused/reset |
| Rate limiting de entrada | ❌ Falta | Considerar golang.org/x/time/rate |
| Body drain antes de Close | ⚠️ Implícito | Agregar io.Copy(io.Discard, resp.Body) en rutas de error |
| Graceful shutdown | ✅ 10s | Pendiente: Aumentar a AgentTimeout+10s (G3) |

### Flujo de estados del circuit breaker (corregido)

```
Closed ─(3 fallos consecutivos)──► Open ─(30s)──► Half-Open
  ▲                                                    │
  │                                              (éxito en probe)
  │◄─────────────────────────────────────────────────────┘

  Half-Open ─(fallo en probe)──► Open
```

### Body drain y keepalive

Para que HTTP keepalive funcione correctamente (reutilización de conexiones TCP), el body de la respuesta del upstream debe ser completamente drenado antes de cerrarlo:

```go
// En rutas donde se detecta un error ANTES de leer el body completo:
defer func() {
    _, _ = io.Copy(io.Discard, resp.Body) // drenar para permitir keepalive
    resp.Body.Close()
}()
```

Cuando `json.Decode` lee el body completamente (caso normal), esto ya se cumple. El `defer` explícito protege las rutas de error donde se retorna antes de completar la lectura.

---

## 7. Observabilidad

### Estado actual

| Componente | Estado | Detalle |
|---|---|---|
| Logging estructurado | ✅ slog JSON | Bueno, con previews de mensajes |
| Métricas Prometheus | ⚠️ Básico | Solo requests_total y duration; faltan inflight, circuit state, upstream status |
| Health check compuesto | ✅ Paralelo | ✅ Resuelto (G2) — WaitGroup + Mutex, max 2s |
| Correlation ID / X-Request-ID | ✅ Implementado | ✅ Resuelto (M4) — middleware + propagación al agente + logs |
| OpenTelemetry tracing | ❌ Falta | Sin spans distribuidos |
| Log de startup estructurado | ⚠️ Strings crudos | fmt.Sprintf en slog (ver M7) |

### Métricas Prometheus a agregar

```go
// internal/metrics/metrics.go

var InFlightRequests = promauto.NewGaugeVec(
    prometheus.GaugeOpts{
        Name: "gateway_inflight_requests",
        Help: "Requests activos en vuelo por agente",
    },
    []string{"agent"},
)

var CircuitBreakerState = promauto.NewGaugeVec(
    prometheus.GaugeOpts{
        Name: "gateway_circuit_breaker_state",
        Help: "Estado del circuit breaker (0=closed, 1=open, 2=half-open)",
    },
    []string{"agent"},
)

var UpstreamStatusTotal = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Name: "gateway_upstream_http_status_total",
        Help: "Respuestas HTTP del agente upstream por código de status",
    },
    []string{"agent", "status_code"},
)

var AgentErrorTotal = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Name: "gateway_agent_error_total",
        Help: "Errores por tipo: timeout, connection_refused, circuit_open, decode_error, backpressure",
    },
    []string{"agent", "error_type"},
)
```

**Uso en InvokeAgent:**

```go
metrics.InFlightRequests.WithLabelValues(agent).Inc()
defer metrics.InFlightRequests.WithLabelValues(agent).Dec()
```

**Uso en OnStateChange del circuit breaker:**

```go
OnStateChange: func(name string, from, to gobreaker.State) {
    slog.Warn("circuit_breaker_state_change",
        "agent", name,
        "from", from.String(),
        "to", to.String(),
    )
    metrics.CircuitBreakerState.WithLabelValues(name).Set(float64(to))
},
```

### OpenTelemetry (roadmap)

Para un gateway de producción con múltiples agentes, el tracing distribuido es el mayor salto de observabilidad. Prioridad: baja ahora, alta cuando haya más de 2 agentes activos simultáneamente.

```go
// Ejemplo básico con go.opentelemetry.io/otel
ctx, span := otel.Tracer("gateway").Start(ctx, "invoke_agent",
    trace.WithAttributes(
        attribute.String("agent.name", agent),
        attribute.Int("session.id", sessionID),
        attribute.String("modalidad", modalidad),
    ),
)
defer span.End()

// Propagar contexto al agente via HTTP headers
otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
```

### Alertas Prometheus recomendadas

```yaml
# Ejemplo de alertas para Grafana/Alertmanager

- alert: GatewayCircuitBreakerOpen
  expr: gateway_circuit_breaker_state == 1
  for: 30s
  annotations:
    summary: "Circuit breaker abierto para agente {{ $labels.agent }}"

- alert: GatewayHighErrorRate
  expr: rate(gateway_requests_total{status="error"}[5m]) / rate(gateway_requests_total[5m]) > 0.1
  for: 2m
  annotations:
    summary: "Tasa de errores >10% en los últimos 5 minutos"

- alert: GatewayHighLatency
  expr: histogram_quantile(0.95, gateway_request_duration_seconds_bucket) > 20
  for: 2m
  annotations:
    summary: "P95 de latencia supera 20 segundos"

- alert: GatewayInFlightSaturation
  expr: gateway_inflight_requests > 15
  for: 1m
  annotations:
    summary: "Más de 15 requests en vuelo para agente {{ $labels.agent }}"
```

---

## 8. Checklist de Producción

### Crítico — Antes de ir a producción

```
[x] C1: Agregar context.WithTimeout en ChatHandler ✅ (resuelto 2026-03-10)
[x] C2: Mejorar http.Transport (ResponseHeaderTimeout, MaxConnsPerHost, DialContext, TLS) ✅ (resuelto 2026-03-10)
[ ] C3: Corregir CORS (no Allow-Credentials con wildcard)
[x] M1: Implementar semáforo de concurrencia por agente ✅ (resuelto 2026-03-10 — chan struct{} cap 25)
[x] M2: Bajar circuit breaker ✅ (resuelto 2026-03-10 — ConsecutiveFailures=3, Timeout=30s, Warn)
[x] G6: Sincronizar .env con .env.example ✅ (resuelto — registry dinámico)
[ ] Ajustar LOG_LEVEL=info en .env de producción
[ ] Restringir CORS_ALLOWED_ORIGINS a dominios reales en producción
[x] Ajustar timeouts: AGENT_TIMEOUT=25, GATEWAY_WRITE_TIMEOUT_SEC=35 ✅ (resuelto 2026-03-10)
```

### Importante — Primera semana en producción

```
[x] M3: Agregar 1 retry para errores de red transitorios ✅ (resuelto 2026-03-10 — dentro de CB, 500ms backoff)
[x] M4: Implementar middleware X-Request-ID ✅ (resuelto 2026-03-10 — genera/propaga/logea)
[x] M6: Warning log en ModalidadToAgent ✅ (resuelto 2026-03-10 — slog.Warn en fallback)
[x] G2: Paralelizar health checks ✅ (resuelto 2026-03-10 — WaitGroup + Mutex)
[ ] G3: Aumentar shutdown timeout a AgentTimeout+10s
[ ] G4: Implementar autenticación mínima (API key header)
[ ] G1: Agregar métricas: inflight_requests, circuit_breaker_state, upstream_status
[ ] compose.yaml: agregar healthcheck y resource limits (memory, cpu)
```

### Mejoras — Sprint posterior

```
[x] M5: Cargar .env en desarrollo ✅ (resuelto 2026-03-10 — godotenv.Load())
[ ] M7: Refactorizar logStartup a structured logging (sin fmt.Sprintf)
[ ] G5: Implementar http.Flusher en responseWriter
[ ] G1: Agregar métricas de error_type (timeout/connection/circuit/decode)
[ ] Rate limiting de entrada (golang.org/x/time/rate por IP o por cliente)
[ ] OpenTelemetry tracing con propagación al agente
[ ] Alertas Prometheus (circuit breaker open, alta latencia, alta tasa de error)
[x] ChatHandler: usar interfaz en lugar de *proxy.Invoker ✅ (resuelto — interfaz AgentCaller)
[ ] Tests de integración con agente mock
[ ] io.LimitReader en decode de respuesta del agente (10 MB máximo)
```

---

## 9. Score de Madurez

| Dimensión | Score | Fortalezas | Gaps principales |
|---|---|---|---|
| Arquitectura y separación de capas | 9/10 | Interfaces (DIP), Registry dinámico (OCP), domain pkg (SRP) | — |
| Alta concurrencia y backpressure | 8/10 | Semáforo per-agent cap 25 ✅, MaxConnsPerHost=25 ✅ | — |
| Uso de net/http y Transport | 9/10 | Transport completo: DialContext, ResponseHeaderTimeout, MaxConnsPerHost, TLS | — |
| Gestión de recursos (pool, keepalive) | 8/10 | Semáforo + MaxConnsPerHost + DialTimeout + pool tuneado | — |
| Resiliencia (timeouts, retry, breaker) | 9/10 | context.WithTimeout ✅, retry ✅, CB tuneado (3/30s) ✅ | — |
| Observabilidad (logs, métricas, tracing) | 7/10 | slog JSON ✅, X-Request-ID ✅, health paralelo ✅, Prometheus básico ✅ | Sin circuit state metrics, sin tracing |
| Seguridad | 5/10 | Body limit ✅, timeouts ✅ | CORS bug ❌ (C3), sin auth ❌ (G4) |
| Correctitud del código | 9/10 | FlexBool/FlexInt, context.WithTimeout, interfaces, SOLID, .env loading | — |
| **TOTAL ACTUAL** | **8.0 / 10** | Resiliencia completa, observabilidad con correlation ID | Gaps de seguridad (C3, G4) |

### Progreso y proyección

| Fase | Fixes aplicados | Score |
|---|---|---|
| ~~Auditoría inicial (2026-02-22)~~ | Ninguno | ~~6.2 / 10~~ |
| ~~Refactor SOLID (2026-03-10)~~ | C1, C2, G2, G6 + interfaces + registry + domain | ~~7.0 / 10~~ |
| **Resiliencia + Observabilidad (2026-03-10)** | M1, M2, M3, M4, M5, M6 | **8.0 / 10** ← actual |
| Seguridad (C3, G4) | CORS fix, auth API key | ~8.5 / 10 |
| Mejoras (G1, G3, G5, M7, tracing, tests) | Métricas completas, shutdown, OTel, testing | ~9.0 / 10 |

---

> **Conclusión (actualizada 2026-03-10):** De los 16 hallazgos originales, **12 están resueltos**: C1, C2 (críticos), M1-M6 (medios), G2, G6 (mejoras), más la refactorización SOLID (interfaces, registry, domain). El gateway tiene resiliencia completa (semáforo, CB tuneado, retry, timeouts encadenados), observabilidad con X-Request-ID de punta a punta, y .env loading para desarrollo. **Pendientes: 4 items** — C3 (CORS bug), G1 (métricas avanzadas), G3 (shutdown timeout), G4 (auth), G5 (Flusher), M7 (startup log estructurado). El gap más relevante es seguridad (C3 + G4).
