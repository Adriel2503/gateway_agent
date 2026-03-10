# Plan: Modulo Sender dentro del Gateway

## Flujo actual vs nuevo

```
ACTUAL:  n8n → Gateway → Agent → Gateway → n8n → WhatsApp
NUEVO:   n8n → Gateway → Agent → Gateway → WhatsApp (async, goroutine)
                                    ↓
                              n8n recibe {status: "processing"}
                              (ya NO recibe el reply del agente)
```

### Diagrama de secuencia

```
n8n ---POST---> Gateway
                  |
                  |---> Agent (espera ~25s, sync)
                  |<--- reply
                  |
n8n <--200 OK--- Gateway  ←  {status:"processing", session_id, agent_used}
                  |
                  |---> WhatsApp API (background goroutine, ~2s)
                  |<--- ok/fail (solo log + metrica)
```

---

## Estructura del modulo

```
internal/sender/
├── sender.go       ← Sender interface, Router, SendRequest
├── official.go     ← OfficialSender (WhatsApp Cloud API)
└── baileys.go      ← BaileysSender (Baileys API)
```

Patron: interfaz + implementaciones concretas + router que selecciona por `source`.
Desacoplado del gateway — extraible a servicio independiente en el futuro.

---

## Pasos de implementacion

### Paso 1: Crear `internal/sender/sender.go`

**Interface Sender**: cada canal implementa `Send` y `Name`.

```go
type Sender interface {
    Send(ctx context.Context, req SendRequest) error
    Name() string
}

type SendRequest struct {
    Phone     string // telefono destino del usuario
    Message   string // reply del agente (o fallback)
    SessionID int
    IdEmpresa int
    Source    string // "whatsapp_cloud_api" o "baileys"
}
```

**Router**: mapea `source` → sender concreto. Ejecuta en goroutine.

```go
type Router struct {
    senders map[string]Sender
    timeout time.Duration
    wg      sync.WaitGroup // rastreo de goroutines para graceful shutdown
}

func NewRouter(timeout time.Duration) *Router
func (r *Router) Register(source string, s Sender)
func (r *Router) SendAsync(req SendRequest)
func (r *Router) Wait()  // bloquea hasta que todas las goroutines terminen
```

**SendAsync** (fire-and-forget con rastreo):
1. Busca sender por `req.Source` en el mapa
2. Si no existe → log warn + metrica `gateway_sender_total{source, status="unknown_source"}`, return
3. `r.wg.Add(1)` — registra la goroutine antes de lanzarla
4. Crea `context.Background()` con timeout = `SENDER_TIMEOUT` (no usa ctx del request HTTP)
5. Lanza goroutine:
   - `defer r.wg.Done()`
   - Llama `sender.Send(ctx, req)`
   - Si ok → log info + metrica `{source, status="ok"}`
   - Si error → log warn + metrica `{source, status="error"}`

**Wait**: bloquea hasta que todas las goroutines pendientes terminen. Se llama desde main.go durante graceful shutdown.

**Por que context.Background()**: el context del request HTTP se cancela al escribir la respuesta a n8n. La goroutine del sender sigue ejecutando despues, necesita su propio context.

---

### Paso 2: Crear `internal/sender/official.go`

```go
type OfficialSender struct {
    url    string       // SENDER_OFFICIAL_URL
    client *http.Client // timeout = SENDER_TIMEOUT
}

func NewOfficialSender(url string, timeout time.Duration) *OfficialSender
func (s *OfficialSender) Send(ctx context.Context, req SendRequest) error
func (s *OfficialSender) Name() string { return "whatsapp_cloud_api" }
```

`Send` hace `POST` al URL con body JSON:
```json
{
    "phone": "51999888777",
    "message": "Hola, tu cita fue agendada...",
    "session_id": 123,
    "id_empresa": 1
}
```

El backend (backnet) se encarga de tokens, phone_number_id, etc.
El sender solo hace POST y verifica HTTP 2xx.

---

### Paso 3: Crear `internal/sender/baileys.go`

```go
type BaileysSender struct {
    url    string       // SENDER_BAILEYS_URL
    client *http.Client
}

func NewBaileysSender(url string, timeout time.Duration) *BaileysSender
func (s *BaileysSender) Send(ctx context.Context, req SendRequest) error
func (s *BaileysSender) Name() string { return "baileys" }
```

Mismo payload JSON que official. URL diferente.
Si la API de baileys necesita campos distintos en el futuro, solo se modifica este archivo.

---

### Paso 4: Agregar config — `internal/config/config.go`

Agregar 3 campos al struct `Config`:

```go
// Sender URLs (vacias = sender deshabilitado para ese canal)
SenderOfficialURL string `env:"SENDER_OFFICIAL_URL" env-default:""`
SenderBaileysURL  string `env:"SENDER_BAILEYS_URL"  env-default:""`
SenderTimeoutSec  int    `env:"SENDER_TIMEOUT"       env-default:"10"`
```

Agregar a `.env.example`:
```env
# --- Sender (envio de respuestas a WhatsApp) ---
SENDER_OFFICIAL_URL=http://localhost:9001/api/send
SENDER_BAILEYS_URL=http://localhost:9002/api/send
SENDER_TIMEOUT=10
```

---

### Paso 5: Actualizar `ChatRequest` — `handler/chat.go`

n8n envia `source` y `phone` a nivel root del JSON:

```go
type ChatRequest struct {
    Message   string     `json:"message"`
    SessionID int        `json:"session_id"`
    Source    string     `json:"source"`    // NUEVO: "whatsapp_cloud_api" o "baileys"
    Phone     string     `json:"phone"`     // NUEVO: telefono del usuario
    Config    ChatConfig `json:"config"`
}
```

**Request de n8n actualizado**:
```json
{
    "message": "Quiero agendar una cita",
    "session_id": 3796,
    "source": "whatsapp_cloud_api",
    "phone": "51999888777",
    "config": {
        "nombre_bot": "MaravIA",
        "id_empresa": 1,
        "modalidad": "Citas",
        ...
    }
}
```

---

### Paso 6: Cambiar `ChatResponse` — `handler/chat.go`

La respuesta a n8n **ya NO incluye el reply**:

```go
// ANTES
type ChatResponse struct {
    Reply     string  `json:"reply"`
    SessionID int     `json:"session_id"`
    AgentUsed *string `json:"agent_used,omitempty"`
    URL       *string `json:"url"`
}

// AHORA
type ChatResponse struct {
    Status    string  `json:"status"`              // "processing" o "fallback"
    SessionID int     `json:"session_id"`
    AgentUsed *string `json:"agent_used,omitempty"`
}
```

Valores de `status`:
| Status | Significado |
|--------|------------|
| `"processing"` | Agente respondio ok, reply enviandose a WhatsApp en background |
| `"fallback"` | Agente fallo, se envia mensaje fallback al usuario |

Errores de validacion siguen siendo HTTP 400 con `{"detail": "..."}`.

---

### Paso 7: Modificar `ServeHTTP` — `handler/chat.go`

Flujo nuevo:

```
1. Validar request (igual que ahora)
2. Llamar agente con timeout (sync, igual que ahora)
3. Si error agente → reply = mensaje fallback, status = "fallback"
4. Si ok → reply = respuesta del agente, status = "processing"
5. Dispatch sender en background (si source y phone presentes)
6. Responder a n8n con {status, session_id, agent_used}
```

Codigo clave:

```go
// Despues de obtener reply (ok o fallback)
if h.Sender != nil && req.Source != "" && req.Phone != "" {
    h.Sender.SendAsync(sender.SendRequest{
        Phone:     req.Phone,
        Message:   reply,
        SessionID: req.SessionID,
        IdEmpresa: req.Config.IdEmpresa,
        Source:    req.Source,
    })
}

writeJSON(w, http.StatusOK, ChatResponse{
    Status:    status,
    SessionID: req.SessionID,
    AgentUsed: &agent,
})
```

---

### Paso 8: Agregar Sender a `ChatHandler`

```go
type ChatHandler struct {
    Caller       AgentCaller
    Router       agent.RouteFunc
    AgentTimeout time.Duration
    Metrics      MetricsRecorder // se mantiene — metricas del agente son independientes del sender
    Sender       *sender.Router  // nil = no enviar
}
```

- `Metrics` se conserva: mide latencia y status de la llamada al agente (proxy), independiente de las metricas del sender.
- Si `Sender` es nil, el gateway responde a n8n normalmente sin enviar nada a WhatsApp.

---

### Paso 9: Metricas — `metrics/metrics.go`

```go
SenderTotal = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Name: "gateway_sender_total",
        Help: "Total send attempts by source and status",
    },
    []string{"source", "status"}, // status: "ok", "error", "unknown_source"
)
```

Dashboard: `rate(gateway_sender_total{status="error"}[5m])` para alertar.

---

### Paso 10: Wiring — `cmd/gateway/main.go`

```go
// Sender router (solo si hay URLs configuradas)
var senderRouter *sender.Router
if cfg.SenderOfficialURL != "" || cfg.SenderBaileysURL != "" {
    senderTimeout := time.Duration(cfg.SenderTimeoutSec) * time.Second
    senderRouter = sender.NewRouter(senderTimeout)
    if cfg.SenderOfficialURL != "" {
        senderRouter.Register("whatsapp_cloud_api",
            sender.NewOfficialSender(cfg.SenderOfficialURL, senderTimeout))
    }
    if cfg.SenderBaileysURL != "" {
        senderRouter.Register("baileys",
            sender.NewBaileysSender(cfg.SenderBaileysURL, senderTimeout))
    }
}

chatHandler := &handler.ChatHandler{
    Caller:       invoker,
    Router:       agent.ModalidadToAgent,
    AgentTimeout: agentTimeout,
    Metrics:      metrics.NewRecorder(),
    Sender:       senderRouter,
}
```

Agregar al startup banner los senders configurados.

---

## Resumen de archivos

| Archivo | Accion | Detalle |
|---------|--------|---------|
| `internal/sender/sender.go` | **CREAR** | Interface Sender, Router, SendRequest, SendAsync |
| `internal/sender/official.go` | **CREAR** | OfficialSender (WhatsApp Cloud API) |
| `internal/sender/baileys.go` | **CREAR** | BaileysSender (Baileys API) |
| `internal/config/config.go` | MODIFICAR | +3 campos: SenderOfficialURL, SenderBaileysURL, SenderTimeoutSec |
| `internal/handler/chat.go` | MODIFICAR | +Source/Phone en request, nueva ChatResponse, sender dispatch |
| `internal/metrics/metrics.go` | MODIFICAR | +SenderTotal counter |
| `cmd/gateway/main.go` | MODIFICAR | Crear sender router, pasar a ChatHandler, startup log |
| `.env.example` | MODIFICAR | +SENDER_OFFICIAL_URL, SENDER_BAILEYS_URL, SENDER_TIMEOUT |

---

## Consideraciones importantes

### Backward compatibility
- **Sin SENDER_*_URL en env**: senderRouter es nil, gateway no envia a WhatsApp
- **Cambio breaking en la respuesta**: `ChatResponse` cambia de `{reply, session_id, agent_used, url}` a `{status, session_id, agent_used}`. n8n debe actualizarse para no esperar el reply.

### Graceful shutdown (obligatorio desde dia 1)
- Las goroutines del sender pueden estar ejecutando cuando llega SIGTERM
- Si no se espera, se pierden mensajes de WhatsApp que ya tienen reply listo
- El Router usa `sync.WaitGroup` para rastrear goroutines activas
- En main.go, el shutdown primero espera al sender y luego cierra el server:

```go
// 1. Dejar de aceptar requests nuevos
slog.Info("shutting down")

// 2. Esperar goroutines del sender (mensajes en vuelo a WhatsApp)
if senderRouter != nil {
    slog.Info("waiting for pending sends")
    senderRouter.Wait()
}

// 3. Shutdown del HTTP server (espera requests in-flight del proxy)
ctx, cancel := context.WithTimeout(context.Background(), agentTimeout+5*time.Second)
defer cancel()
srv.Shutdown(ctx)
```

- El SENDER_TIMEOUT (10s) garantiza que las goroutines terminan antes del shutdown timeout del server (30s)
- No se necesita timeout extra: cada goroutine ya tiene su context con SENDER_TIMEOUT

### Error handling del sender
- Si el POST al API de WhatsApp falla → log warn + metrica error
- NO afecta la respuesta a n8n (ya fue enviada)
- NO reintenta (por ahora). Reintentos se agregan despues si se necesita

### Por que context.Background() y no el request context
- El request context se cancela cuando se escribe la respuesta HTTP a n8n
- El sender ejecuta DESPUES de esa respuesta
- Necesita su propio context con su propio timeout (SENDER_TIMEOUT)

### Escalabilidad futura
- Si se necesita extraer a servicio: crear `cmd/sender/main.go`, mover `internal/sender/` a su propio modulo Go, exponer como HTTP endpoint
- El codigo del sender no tiene dependencias en handler, proxy, ni config del gateway
- Solo depende de `metrics` (que se puede replicar)

---

## Fase 2: Gateway asincrono — webhook directo (roadmap, NO implementar aun)

### Cambio de paradigma

En Fase 1 el gateway sigue siendo **semi-sincrono**: el handler espera al agente (~25s)
y solo el sender es async. En Fase 2 el gateway se vuelve **100% asincrono**.

```
FASE 1 (semi-sync):
  n8n ──POST──> Gateway ──sync wait ~25s──> Agent
                  ↓                            ↓
                  ← {status:"processing"} ←────┘
                  ↓ goroutine
                Sender → WhatsApp

FASE 2 (full async):
  WhatsApp ──webhook──> Gateway: 200 OK (inmediato, solo ACK)
                          ↓ goroutine (pipeline completo en background)
                        Agent (~25s)
                          ↓
                        Sender → WhatsApp
```

### Que cambia arquitecturalmente

El handler del webhook **solo valida, hace ACK y dispara una goroutine**.
Ya no hay ninguna espera sincrona en el path HTTP.

```go
// Fase 2: webhook handler (conceptual)
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    msg, err := h.parser.Parse(r)  // extraer phone, message del formato del provider
    if err != nil {
        writeJSON(w, 400, ...)
        return
    }

    // ACK inmediato — WhatsApp/Baileys requieren respuesta rapida (<5s)
    w.WriteHeader(http.StatusOK)

    // Pipeline completo en background
    h.pipeline.ProcessAsync(msg)
}
```

`ProcessAsync` seria el nuevo orquestador:
```go
func (p *Pipeline) ProcessAsync(msg IncomingMessage) {
    p.wg.Add(1)
    go func() {
        defer p.wg.Done()
        ctx, cancel := context.WithTimeout(context.Background(), p.totalTimeout)
        defer cancel()

        // 1. Llamar agente (lo que hoy es sync en el handler)
        reply, url, err := p.invoker.InvokeAgent(ctx, msg.Agent, msg.Text, msg.SessionID, msg.Context)
        if err != nil {
            reply = fallbackReply
        }

        // 2. Enviar a WhatsApp (lo que hoy hace el sender)
        p.sender.Send(ctx, sender.SendRequest{
            Phone:   msg.Phone,
            Message: reply,
            ...
        })
    }()
}
```

### Que se reutiliza de Fase 1

| Componente | Reutilizable |
|---|---|
| `internal/sender/` | 100% — mismo sender, misma interfaz |
| `internal/proxy/` (Invoker, CB, semaforo) | 100% — se llama desde goroutine en vez de handler |
| `internal/agent/` (Registry) | 100% |
| `internal/metrics/` | Se extiende con metricas de webhook |
| `internal/handler/chat.go` | Se mantiene para backward compat o se depreca |

### Que hay que construir nuevo

| Componente | Trabajo | Complejidad |
|---|---|---|
| Endpoint webhook | `POST /api/webhook/whatsapp` y `/api/webhook/baileys` | Media |
| Validacion de firma | Verificar signature de Meta (HMAC-SHA256) / Baileys | Media |
| Parser de mensajes | Extraer phone, message, metadata del formato de cada provider | Media |
| Pipeline/orquestador | Goroutine que ejecuta Agent → Sender como pipeline | Baja (compone piezas existentes) |
| Mapeo modalidad | Hoy n8n envia `modalidad`; sin n8n hay que resolverlo (config por empresa, o campo en DB) | **Decisión de producto** |
| Graceful shutdown | WaitGroup del pipeline debe drenar goroutines que estan llamando agentes (~25s) + enviando (~10s) | Baja |

### Consideraciones clave

**Timeouts**: el pipeline completo puede tomar hasta ~35s (25s agente + 10s sender).
El shutdown timeout debe cubrir esto: `AGENT_TIMEOUT + SENDER_TIMEOUT + 5s`.

**Backpressure**: el semaforo por agente (25 concurrent) ya protege el proxy.
Pero ahora las goroutines se acumulan en el pipeline, no en handlers HTTP.
Considerar un semaforo global para el pipeline si el trafico crece.

**Perdida de mensajes**: si el gateway se reinicia, las goroutines in-flight se pierden.
En Fase 1 esto solo pierde el envio a WhatsApp (el reply ya llego a n8n).
En Fase 2 se pierde todo el flujo. Para trafico alto, considerar una cola persistente
(Redis, NATS) entre el webhook y el pipeline. Para MVP, goroutines + WaitGroup es suficiente.

**Idempotencia**: WhatsApp puede enviar el mismo webhook 2+ veces.
Necesita deduplicacion por message_id (in-memory set o Redis).

---

## Verificacion

1. `go build ./...` — compila sin errores
2. `go vet ./...` — sin warnings
3. **Sin sender**: `curl -X POST localhost:8000/api/agent/chat` → `{status: "processing", ...}`
4. **Con sender**: log muestra "→ sending to whatsapp_cloud_api" y "← send ok"
5. **Metrica**: `curl localhost:8000/metrics | grep gateway_sender_total`
6. **Source invalido**: log muestra warning, metrica `unknown_source`
7. **Sin phone/source**: no intenta enviar, responde normalmente

---

## Variables de entorno nuevas

```env
# Sender: envio de respuestas a WhatsApp
# Dejar vacias para deshabilitar el envio por ese canal
SENDER_OFFICIAL_URL=http://localhost:9001/api/send
SENDER_BAILEYS_URL=http://localhost:9002/api/send
SENDER_TIMEOUT=10
```
