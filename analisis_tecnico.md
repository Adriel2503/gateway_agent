# Auditoría Técnica — MaravIA Gateway

> **Rol del auditor:** Senior Go Engineer & Backend Architect, especializado en API Gateways de alta concurrencia, resiliencia, observabilidad y optimización de CPU/memoria/red.
>
> **Fecha:** 2026-02-22
>
> **Versión auditada:** commit `60d6056` (branch `main`)

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

- Una **race condition de timeouts** que provoca goroutines zombies bajo carga.
- **Transporte HTTP suboptimizado** (sin `ResponseHeaderTimeout`, sin `MaxConnsPerHost`).
- **Ausencia de backpressure** (sin límite de concurrencia por agente).
- **CORS mal configurado** (`Allow-Credentials: true` con wildcard).
- **Sin autenticación** de requests entrantes.
- **Gaps en observabilidad** (sin correlation ID, sin métricas de circuit breaker state, sin tracing).

**Score actual: 6.2 / 10.** Con las correcciones críticas y medias listadas puede llegar a **8.5+**.

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
| Config | `internal/config/config.go` | Env vars → struct, lookup de URLs/flags |
| Handler | `internal/handler/chat.go` | Decode, validate, orchestrate, respond |
| Health | `internal/handler/health.go` | Health check compuesto del gateway + agentes |
| Proxy | `internal/proxy/agents.go` | HTTP call al agente + circuit breaker |
| Middleware | `internal/middleware/` | CORS, logging |
| Metrics | `internal/metrics/metrics.go` | Prometheus counters + histogramas |

**Evaluación de capas:** La separación es correcta. El único punto de acoplamiento menor es que `ChatHandler` depende del tipo concreto `*proxy.Invoker` en lugar de una interfaz — corrección sencilla pero que facilitaría testing unitario.

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

#### C1 — Race condition WriteTimeout vs AgentTimeout: goroutines zombies

**Archivos:** `cmd/gateway/main.go` + `internal/proxy/agents.go`

**Descripción:** El servidor tiene `WriteTimeout=30s` y el cliente de agentes tiene `AGENT_TIMEOUT=30s`. Ambos iguales es el **peor escenario posible**.

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

#### C2 — http.Transport sin `ResponseHeaderTimeout` ni `MaxConnsPerHost`

**Archivo:** `internal/proxy/agents.go:30-38`

**Código actual:**

```go
Transport: &http.Transport{
    MaxIdleConns:        50,
    MaxIdleConnsPerHost: 10,
    IdleConnTimeout:     90 * time.Second,
}
```

**Problema 1 — Sin `ResponseHeaderTimeout`:** Un agente puede aceptar la conexión TCP, enviar el status line `HTTP/1.1 200 OK`, y luego **nunca enviar los headers de respuesta**. El goroutine queda bloqueado hasta que `http.Client.Timeout` expire (30s). Durante ese tiempo: socket abierto, goroutine ocupada, y el agente malgastó una conexión.

**Problema 2 — Sin `MaxConnsPerHost`:** Bajo alta concurrencia, Go puede abrir **conexiones TCP ilimitadas** hacia el mismo agente. 200 requests simultáneas = 200 conexiones TCP al mismo host. Esto puede saturar el agente Python (FastAPI/Flask con workers limitados) antes de que el circuit breaker tenga tiempo de reaccionar.

**Problema 3 — Sin `DialContext` con timeout:** Si el agente está caído pero el host responde al TCP SYN con RST, la conexión falla rápido. Pero si el host no responde (firewall drop), la conexión espera el timeout de TCP del OS (~2 minutos) en lugar del timeout configurado.

**Fix — Transport completo y correctamente configurado:**

```go
import "net"

func newTransport(cfg *config.Config) *http.Transport {
    return &http.Transport{
        // Dialer TCP con timeout explícito
        DialContext: (&net.Dialer{
            Timeout:   5 * time.Second,  // falla rápido si el agente no responde TCP
            KeepAlive: 30 * time.Second, // mantiene conexiones vivas entre requests
        }).DialContext,

        // Límites de conexiones
        MaxConnsPerHost:     25,  // máx. conexiones activas por host (backpressure TCP)
        MaxIdleConnsPerHost: 10,  // conexiones idle en el pool por host
        MaxIdleConns:        50,  // total idle en el pool global
        IdleConnTimeout:     90 * time.Second,

        // Timeouts granulares a nivel de transporte
        TLSHandshakeTimeout:   5 * time.Second,
        ResponseHeaderTimeout: 20 * time.Second, // CRÍTICO: el agente debe enviar headers en 20s
        ExpectContinueTimeout: 1 * time.Second,

        // HTTP/1.1 (los agentes Python probablemente no soportan HTTP/2)
        ForceAttemptHTTP2: false,
        DisableKeepAlives: false, // keepalive = reutilizar conexiones TCP (siempre activado)
    }
}
```

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

#### M1 — Sin backpressure: goroutines acumulables bajo carga

**Descripción:** No hay límite de concurrencia hacia los agentes. Ante un pico de 200 requests simultáneas:

- 200 goroutines activas, cada una esperando respuesta del agente (~8 KB por goroutine = ~1.6 MB mínimo, más buffers).
- Sin `MaxConnsPerHost` (ver C2), Go abre 200 conexiones TCP al agente.
- El agente Python se satura → latencias crecientes → más goroutines acumuladas → **cascada de fallos**.

El circuit breaker solo actúa después de 5 fallos consecutivos. Mientras tanto, los goroutines se acumulan.

**Fix — semáforo por agente (backpressure limpio):**

```go
// internal/proxy/agents.go

type Invoker struct {
    cfg    *config.Config
    client *http.Client
    cbs    map[string]*gobreaker.CircuitBreaker[agentResult]
    sems   map[string]chan struct{} // limitador de concurrencia por agente
}

// En NewInvoker, inicializar semáforos:
sems := make(map[string]chan struct{}, len(agents))
for _, name := range agents {
    sems[name] = make(chan struct{}, 20) // máx. 20 requests concurrentes por agente
}

// En InvokeAgent, antes de llamar al circuit breaker:
sem, ok := inv.sems[agent]
if ok {
    select {
    case sem <- struct{}{}: // adquirir slot
        defer func() { <-sem }() // liberar al terminar
    default:
        // Backpressure: agente saturado, rechazar inmediatamente
        return "", nil, fmt.Errorf("agent %s: demasiadas requests concurrentes (backpressure)", agent)
    }
}
```

En el handler, mapear este error a **HTTP 503** (no al fallback genérico):

```go
if errors.Is(err, ErrBackpressure) {
    w.WriteHeader(http.StatusServiceUnavailable)
    writeJSON(w, http.StatusServiceUnavailable, map[string]string{
        "detail": "Servicio temporalmente saturado. Intenta en un momento.",
    })
    return
}
```

---

#### M2 — Circuit breaker tarda demasiado en abrir (150 segundos de fallos lentos)

**Configuración actual:**

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

---

#### M3 — Sin retries para errores de red transitorios

**Descripción:** Un flap de red transitorio o restart del agente Python causa fallo inmediato sin ningún reintento. Para este caso (chatbot con `session_id`), un retry es seguro porque el agente puede manejar mensajes repetidos.

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

#### M4 — Sin X-Request-ID ni correlation ID

**Descripción:** No se genera ni propaga ningún ID de correlación. Es imposible correlacionar un log del gateway con el log correspondiente del agente Python. Si n8n envía un `X-Request-ID`, se descarta silenciosamente.

**Fix — middleware de correlation ID:**

```go
// internal/middleware/request_id.go

type contextKey string
const RequestIDKey contextKey = "request_id"

func RequestID(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        id := r.Header.Get("X-Request-ID")
        if id == "" {
            // Generar un ID único si n8n no lo envía
            id = fmt.Sprintf("%d-%d", time.Now().UnixNano(), rand.Int63())
        }
        ctx := context.WithValue(r.Context(), RequestIDKey, id)
        w.Header().Set("X-Request-ID", id) // devolver al cliente para trazabilidad
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}

func GetRequestID(ctx context.Context) string {
    if id, ok := ctx.Value(RequestIDKey).(string); ok {
        return id
    }
    return ""
}
```

Agregar al router antes de CORS:

```go
r.Use(middleware.RequestID)
r.Use(middleware.Logger)
r.Use(middleware.CORS(cfg.CORSOrigins))
```

Propagar al agente en `doHTTP`:

```go
req.Header.Set("X-Request-ID", middleware.GetRequestID(ctx))
```

Incluir en todos los logs del handler y proxy:

```go
slog.Info("→ request entrada",
    "request_id", middleware.GetRequestID(r.Context()),
    "modalidad", req.Config.Modalidad,
    // ...
)
```

---

#### M5 — config.Load() no carga el archivo .env en desarrollo local

**Archivo:** `internal/config/config.go:22`

**Código actual:**

```go
_ = cleanenv.ReadEnv(nil) // ← no hace nada útil
var c Config
if err := cleanenv.ReadEnv(&c); err != nil { ... }
```

**Problema:** `cleanenv.ReadEnv` lee variables **del entorno del proceso** (variables ya exportadas), no de un archivo `.env`. La llamada con `nil` es un no-op. En Docker Compose con `env_file: .env`, las variables ya están inyectadas en el entorno → funciona. Pero al ejecutar `go run ./cmd/gateway` directamente en desarrollo local, **el `.env` no se carga automáticamente**.

**Fix:**

```go
func Load() (*Config, error) {
    // Intentar cargar .env si existe (solo para desarrollo local; en Docker ya está en el entorno)
    if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
        // No es error crítico si .env no existe (producción sin archivo)
        slog.Debug("config: .env not found, using environment variables only")
    }
    var c Config
    if err := cleanenv.ReadEnv(&c); err != nil {
        return nil, fmt.Errorf("config: %w", err)
    }
    // ...
}
```

---

#### M6 — ModalidadToAgent: fallback silencioso a "cita"

**Archivo:** `internal/proxy/agents.go`

**Código actual:**

```go
default:
    return "cita" // ← silencioso, sin warning
```

**Problema:** Si n8n envía una modalidad incorrecta (`"ventas"` con minúscula, `"CITAS"` con mayúscula, un typo, etc.), el gateway lo rutea silenciosamente al agente de citas. En producción esto causa que conversaciones de ventas lleguen al agente de citas sin ningún aviso en los logs.

**Fix — loguear como warning y opcionalmente rechazar:**

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

#### G2 — Health check hace requests síncronos secuenciales

**Archivo:** `internal/handler/health.go`

**Problema:** Con 4 agentes caídos, cada check espera 2s (timeout) antes de pasar al siguiente. El endpoint `/health` tarda hasta **8 segundos** en responder. Cualquier load balancer con un health check timeout de 1–2s marcará el gateway como caído.

**Fix — checks paralelos con WaitGroup:**

```go
func (h *HealthHandler) checkAgents() (map[string]string, bool) {
    agentNames := []string{"venta", "cita", "reserva", "citas_ventas"}
    results := make(map[string]string, len(agentNames))
    var mu sync.Mutex
    var wg sync.WaitGroup
    allOK := true

    for _, name := range agentNames {
        wg.Add(1)
        go func(n string) {
            defer wg.Done()
            status := h.checkAgent(n)
            mu.Lock()
            results[n] = status
            if status != "ok" && status != "disabled" {
                allOK = false
            }
            mu.Unlock()
        }(name)
    }
    wg.Wait()
    return results, allOK
}
```

Con esta implementación, el timeout máximo de `/health` es **2 segundos** (un solo check en paralelo), no 8.

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

#### G6 — .env y .env.example desincronizados

**Problema:** `.env` no incluye `AGENT_CITAS_VENTAS_URL` ni `AGENT_CITAS_VENTAS_ENABLED`, pero el código sí soporta el agente `citas_ventas`. En runtime se usan los defaults del struct (`http://localhost:8004/api/chat`, `true`), pero no está documentado en `.env`.

**Fix:** Sincronizar `.env` con `.env.example`:

```env
AGENT_CITAS_VENTAS_URL=http://localhost:8004/api/chat
AGENT_CITAS_VENTAS_ENABLED=true
```

---

## 4. Riesgos de Producción

| # | Riesgo | Escenario que lo activa | Impacto | Probabilidad |
|---|---|---|---|---|
| R1 | **Goroutines zombies** | Agente lento (≥30s), WriteTimeout dispara, goroutine sigue viva | Memory leak gradual, OOM | Alta |
| R2 | **Cascada de fallos** | 1 agente lento satura el pool → latencias globales suben | Degradación total del servicio | Media |
| R3 | **TCP socket exhaustion** | Alta carga sin `MaxConnsPerHost` | Agente saturado, `connection refused` | Media |
| R4 | **Breaker tarda en abrir** | 5 fallos × 30s = 150s de requests lentas antes de apertura | n8n timeouts en cascada | Alta |
| R5 | **Health check lento** | 4 agentes caídos → 8s para responder `/health` | LB/orchestrator marca gateway como muerto | Media |
| R6 | **CORS bug** | Browser hace requests (UI futura) con wildcard + credentials | Requests silenciosamente rechazadas por browser | Baja-Media |
| R7 | **Sin autenticación** | Endpoint accesible desde red no confiable | Abuso, costos de agentes, spam | Depende de red |
| R8 | **Modalidad silenciosa** | n8n envía modalidad incorrecta | Ventas ruteadas a Citas sin aviso | Media |
| R9 | **Shutdown brusco** | Requests en vuelo durante deploy/restart | n8n recibe error en mitad de conversación | Media |

### Qué falla primero bajo carga

```
Escenario: tráfico creciente de n8n

1. Primero: Agente Python se satura (workers limitados, I/O bound con LLM)
   → Latencias suben de 3s → 15s → 30s

2. Segundo: Goroutines del gateway se acumulan (cada request bloqueada 30s)
   → Memoria del gateway sube
   → CPU sube por GC pressure

3. Tercero: Sin MaxConnsPerHost, el pool TCP del agente se agota
   → "connection refused" o "too many open files"

4. Cuarto: Sin backpressure (semáforo), el gateway no rechaza requests nuevas
   → Más goroutines → más memoria → OOM o crash

5. El circuit breaker abre TARDE (150s) → durante ese tiempo todo está degradado
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

| Mecanismo | Estado actual | Acción recomendada |
|---|---|---|
| HTTP server ReadHeaderTimeout | ✅ 10s | Mantener |
| HTTP server ReadTimeout | ✅ 30s | Subir a 40s |
| HTTP server WriteTimeout | ✅ 30s | Subir a 35s |
| HTTP server IdleTimeout | ✅ 60s | Mantener |
| http.Client.Timeout | ✅ 30s | Bajar a 25s (< WriteTimeout) |
| context.WithTimeout en handler | ❌ Falta | Agregar (C1 — crítico) |
| ResponseHeaderTimeout | ❌ Falta | Agregar 20s en Transport (C2) |
| DialTimeout | ❌ Falta | Agregar 5s en DialContext (C2) |
| MaxConnsPerHost | ❌ Falta | Agregar 25 (C2) |
| Circuit breaker por agente | ✅ gobreaker | Bajar umbral a 3, timeout a 30s (M2) |
| Backpressure / semáforo | ❌ Falta | Implementar (M1) |
| Retry con backoff | ❌ Falta | 1 retry para errores de red (M3) |
| Rate limiting de entrada | ❌ Falta | Considerar golang.org/x/time/rate |
| Body drain antes de Close | ⚠️ Implícito | Agregar io.Copy(io.Discard, resp.Body) en rutas de error |
| Graceful shutdown | ✅ 10s | Aumentar a AgentTimeout+10s (G3) |

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
| Health check compuesto | ✅ Implementado | Verificar paralelismo (ver G2) |
| Correlation ID / X-Request-ID | ❌ Falta | Imposible trazar requests cross-service |
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
[ ] C1: Agregar context.WithTimeout en ChatHandler (AGENT_TIMEOUT < WRITE_TIMEOUT - 5s)
[ ] C2: Mejorar http.Transport:
        - ResponseHeaderTimeout: 20s
        - MaxConnsPerHost: 25
        - DialContext con Timeout: 5s y KeepAlive: 30s
        - TLSHandshakeTimeout: 5s
[ ] C3: Corregir CORS (no Allow-Credentials con wildcard)
[ ] M1: Implementar semáforo de concurrencia por agente (backpressure)
[ ] M2: Bajar circuit breaker: ConsecutiveFailures=3, Timeout=30s
[ ] G6: Sincronizar .env con .env.example (AGENT_CITAS_VENTAS_URL)
[ ] Ajustar LOG_LEVEL=info en .env de producción
[ ] Restringir CORS_ALLOWED_ORIGINS a dominios reales en producción
[ ] Ajustar timeouts: AGENT_TIMEOUT=25, GATEWAY_WRITE_TIMEOUT_SEC=35
```

### Importante — Primera semana en producción

```
[ ] M3: Agregar 1 retry con backoff mínimo para errores de red transitorios
[ ] M4: Implementar middleware X-Request-ID y propagarlo al agente
[ ] M6: Agregar warning log en ModalidadToAgent para modalidades desconocidas
[ ] G2: Paralelizar health checks (tiempo máx. de /health = 2s, no 8s)
[ ] G3: Aumentar shutdown timeout a AgentTimeout+10s
[ ] G4: Implementar autenticación mínima (API key header)
[ ] G1: Agregar métricas: inflight_requests, circuit_breaker_state, upstream_status
[ ] compose.yaml: agregar healthcheck y resource limits (memory, cpu)
```

### Mejoras — Sprint posterior

```
[ ] M5: Cargar .env explícitamente en desarrollo (godotenv.Load())
[ ] M7: Refactorizar logStartup a structured logging (sin fmt.Sprintf)
[ ] G5: Implementar http.Flusher en responseWriter
[ ] G1: Agregar métricas de error_type (timeout/connection/circuit/decode)
[ ] Rate limiting de entrada (golang.org/x/time/rate por IP o por cliente)
[ ] OpenTelemetry tracing con propagación al agente
[ ] Alertas Prometheus (circuit breaker open, alta latencia, alta tasa de error)
[ ] ChatHandler: usar interfaz en lugar de *proxy.Invoker (facilita testing)
[ ] Tests de integración con agente mock
[ ] io.LimitReader en decode de respuesta del agente (10 MB máximo)
```

---

## 9. Score de Madurez

| Dimensión | Score | Fortalezas | Gaps principales |
|---|---|---|---|
| Arquitectura y separación de capas | 8/10 | Layout estándar Go, capas claras | ChatHandler acoplado a tipo concreto |
| Alta concurrencia y backpressure | 4/10 | Goroutines nativas de Go | Sin semáforo, sin MaxConnsPerHost, goroutines zombies |
| Uso de net/http y Transport | 6/10 | Cliente compartido ✅ | Transport incompleto (sin ResponseHeaderTimeout, sin DialTimeout) |
| Gestión de recursos (pool, keepalive) | 5/10 | MaxIdleConns configurado | Sin MaxConnsPerHost, sin límite activo |
| Resiliencia (timeouts, retry, breaker) | 6/10 | Circuit breaker por agente ✅ | Race condition timeout ❌, sin retry ❌, breaker lento ❌ |
| Observabilidad (logs, métricas, tracing) | 5/10 | slog JSON ✅, Prometheus básico ✅ | Sin X-Request-ID, sin circuit state metrics, sin tracing |
| Seguridad | 5/10 | Body limit ✅, timeouts ✅ | CORS bug ❌, sin auth ❌, wildcard CORS en producción ❌ |
| Correctitud del código | 7/10 | FlexBool/FlexInt bien resueltos, ctx propagado | WriteTimeout race condition |
| **TOTAL ACTUAL** | **6.2 / 10** | Base sólida para MVP | Gaps reales para producción bajo carga |

### Proyección tras aplicar fixes

| Fase | Fixes aplicados | Score esperado |
|---|---|---|
| Ahora (MVP) | Ninguno | 6.2 / 10 |
| Críticos (C1, C2, C3, M1, M2) | Race condition, Transport, CORS, backpressure, breaker | ~7.8 / 10 |
| Importantes (M3-M6, G2-G4, G6) | Retry, correlación, auth, health paralelo | ~8.5 / 10 |
| Mejoras (G1, G5, tracing, tests) | Métricas completas, OTel, testing | ~9.0 / 10 |

---

> **Conclusión:** El código es limpio, idiomático en Go y bien estructurado para un proyecto de tamaño pequeño-mediano. Los problemas identificados no son de diseño fundamental, sino de configuración y mecanismos de resiliencia que se agregan iterativamente. Los dos fixes más urgentes son **C1** (goroutines zombies por race de timeouts) y **C2** (Transport incompleto), ya que son los únicos que pueden causar degradación catastrófica bajo carga real. El resto del sistema seguirá funcional sin ellos, pero con riesgo creciente a medida que escale el tráfico.
