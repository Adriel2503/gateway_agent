# MaravIA Gateway

API Gateway en Go que recibe requests de **n8n** y enruta al agente IA especializado segun la **modalidad** del negocio. Implementa circuit breaker por agente, metricas Prometheus, health checks paralelos y registro dinamico de agentes.

## Arquitectura

```
┌──────────┐       ┌───────────────────────────┐       ┌──────────────────┐
│          │ POST  │     MaravIA Gateway       │       │  Agente Venta    │
│   n8n    │──────>│                           │──CB──>│  :8001/api/chat  │
│          │       │  /api/agent/chat          │       └──────────────────┘
└──────────┘       │                           │       ┌──────────────────┐
                   │  Routing por modalidad:   │──CB──>│  Agente Cita     │
                   │    citas    -> cita       │       │  :8002/api/chat  │
                   │    ventas   -> venta      │       └──────────────────┘
                   │    reservas -> reserva    │       ┌──────────────────┐
                   │    citas y ventas         │──CB──>│  Agente Reserva  │
                   │            -> citas_ventas│       │  :8003/api/chat  │
                   │                           │       └──────────────────┘
                   │  CB = Circuit Breaker     │       ┌──────────────────┐
                   │                           │──CB──>│  Agente Citas y  │
                   └───────────────────────────┘       │  Ventas :8004    │
                                                       └──────────────────┘
```

### Flujo de un request

```
n8n ──POST──> Gateway ──POST──> Agente Python (FastAPI + LangGraph)
                                      │
n8n <──JSON── Gateway <──JSON─────────┘
```

1. n8n envia `{message, session_id, config}` al gateway
2. Gateway lee `config.modalidad` y selecciona el agente
3. Gateway reenvia al agente con `{message, session_id, context}`
4. Agente responde `{reply, url}`
5. Gateway responde a n8n con `{reply, session_id, agent_used, url}`

## Stack

| Componente | Tecnologia |
|---|---|
| Lenguaje | Go 1.26+ |
| Router | [Chi v5](https://github.com/go-chi/chi) |
| Config | [cleanenv](https://github.com/ilyakaznacheev/cleanenv) (env -> struct) |
| Logging | `log/slog` (stdlib, JSON estructurado) |
| Metricas | [Prometheus client_golang](https://github.com/prometheus/client_golang) |
| Circuit Breaker | [gobreaker v2](https://github.com/sony/gobreaker) (por agente) |
| HTTP Client | `net/http.Client` (connection pooling, transport tuneado) |

## Inicio rapido

### Desarrollo local

```bash
cd gateway
cp .env.example .env
go mod tidy
go run ./cmd/gateway
```

### Compilar binario

```bash
go build -o gateway ./cmd/gateway
./gateway
```

### Docker

```bash
docker build -t maravia-gateway .
docker run -p 8000:8000 --env-file .env maravia-gateway

# O con Docker Compose
docker compose up --build
```

Imagen Docker: multi-stage build (golang:1.26-alpine -> alpine:3.19), binario estatico, usuario no-root (`appuser` UID 10001). ~20-30 MB.

## Estructura del proyecto

```
gateway/
├── cmd/gateway/
│   └── main.go                 # Entry point, wiring, graceful shutdown
├── internal/
│   ├── agent/                  # Registro y routing de agentes
│   │   ├── registry.go         # Registry: escanea AGENT_*_URL del env (dinamico)
│   │   └── routing.go          # ModalidadToAgent: mapea modalidad -> agente
│   ├── config/
│   │   └── config.go           # Config del servidor (puertos, timeouts, CORS)
│   ├── domain/
│   │   └── flex.go             # FlexBool, FlexInt (tipos flexibles para n8n)
│   ├── handler/
│   │   ├── chat.go             # POST /api/agent/chat (interfaz AgentCaller)
│   │   └── health.go           # GET /health (paralelo, interfaz AgentLister)
│   ├── metrics/
│   │   └── metrics.go          # Prometheus: counters + histogramas
│   ├── middleware/
│   │   ├── cors.go             # CORS configurable
│   │   └── logger.go           # Request logging (method, path, status, duration)
│   └── proxy/
│       └── agents.go           # HTTP client + circuit breaker por agente
├── .env.example
├── Dockerfile                  # Multi-stage build (alpine, non-root)
├── compose.yaml
├── go.mod
└── go.sum
```

### Paquetes y responsabilidades

| Paquete | Responsabilidad |
|---|---|
| `agent` | Registro dinamico de agentes desde env vars + routing por modalidad |
| `config` | Configuracion del servidor HTTP (sin logica de agentes) |
| `domain` | Tipos compartidos: `FlexBool`, `FlexInt`, `Preview()` |
| `handler` | Handlers HTTP. Definen interfaces que consumen (`AgentCaller`, `AgentLister`) |
| `metrics` | Definicion de metricas Prometheus |
| `middleware` | CORS y logging de requests |
| `proxy` | Cliente HTTP hacia agentes con circuit breaker |

### Grafo de dependencias

```
main.go
 ├── config      (carga env vars del servidor)
 ├── agent       (registry + routing)
 ├── handler     (chat, health)
 │    ├── domain (FlexBool, FlexInt)
 │    └── metrics
 ├── middleware   (cors, logger)
 └── proxy       (invoker con circuit breaker)
      ├── agent  (registry para URLs/enabled)
      └── domain (Preview)
```

### Interfaces (DIP — Go convention: definidas en el consumidor)

```go
// handler/chat.go — lo que el handler necesita del proxy
type AgentCaller interface {
    InvokeAgent(ctx, agent, message string, sessionID int, contextMap map[string]interface{}) (reply string, url *string, err error)
}

// handler/health.go — lo que el health check necesita del registry
type AgentLister interface {
    All() []agent.AgentInfo
}

// agent/routing.go — tipo para la funcion de routing
type RouteFunc func(modalidad string) (agentKey string)
```

## Registro dinamico de agentes

Los agentes se registran automaticamente escaneando variables de entorno con el patron `AGENT_*_URL`:

```env
AGENT_VENTA_URL=http://localhost:8001/api/chat
AGENT_CITA_URL=http://localhost:8002/api/chat
AGENT_SOPORTE_URL=http://localhost:8005/api/chat   # agregar agente = solo esto
```

**Agregar un nuevo agente = solo agregar `AGENT_<KEY>_URL` al `.env`**. Sin tocar codigo.

Para cada `AGENT_<KEY>_URL`, el registry busca opcionalmente `AGENT_<KEY>_ENABLED` (default `true`). Tambien deriva automaticamente la URL de health (`/health`).

### Routing por modalidad

El campo `config.modalidad` del request determina el agente. Se normaliza con `trim + lowercase`:

| Modalidad (n8n) | Agente | Variable de entorno |
|---|---|---|
| `Citas` | `cita` | `AGENT_CITA_URL` |
| `Ventas` | `venta` | `AGENT_VENTA_URL` |
| `Reservas` | `reserva` | `AGENT_RESERVA_URL` |
| `Citas y Ventas` | `citas_ventas` | `AGENT_CITAS_VENTAS_URL` |
| _(otro/fallback)_ | `cita` | `AGENT_CITA_URL` |

El routing es por valor exacto. No usa LLM.

## Endpoints

### `GET /` — Info del servicio

```json
{"service": "MaravIA Gateway", "status": "running", "endpoints": {"/api/agent/chat": "POST", "/health": "GET", "/metrics": "GET"}}
```

### `POST /api/agent/chat` — Chat principal

Recibe el request de n8n, enruta al agente por modalidad, devuelve la respuesta.

**Request:**

```json
{
  "message": "Quiero agendar una cita para manana",
  "session_id": 3796,
  "config": {
    "nombre_bot": "MaravIA",
    "id_empresa": 1,
    "modalidad": "Citas",
    "frase_saludo": "Hola! En que puedo ayudarte?",
    "archivo_saludo": "",
    "personalidad": "amigable y profesional",
    "frase_des": "Fue un gusto ayudarte",
    "frase_no_sabe": "No tengo esa informacion",
    "correo_usuario": "usuario@ejemplo.com",
    "duracion_cita_minutos": 30,
    "slots": 5,
    "agendar_usuario": true,
    "agendar_sucursal": false,
    "id_prospecto": 3796,
    "usuario_id": 42,
    "id_chatbot": 1
  }
}
```

**Response exitosa (200):**

```json
{
  "reply": "Claro, te ayudare a agendar tu cita. Que dia te conviene?",
  "session_id": 3796,
  "agent_used": "cita",
  "url": null
}
```

**Response en error de agente (200):** el gateway responde 200 con mensaje fallback para que n8n no rompa el flujo.

```json
{
  "reply": "No pude conectar con el agente. Intenta de nuevo en un momento.",
  "session_id": 3796,
  "agent_used": "cita",
  "url": null
}
```

**Errores de validacion:**

| Status | Causa |
|---|---|
| 400 | JSON invalido, `message` vacio, `session_id` negativo, `config.id_empresa` <= 0 |
| 405 | Metodo distinto a POST |
| 413 | Body mayor a 512 KB |

### `GET /health` — Health check compuesto (paralelo)

Verifica el gateway y cada agente habilitado en paralelo (`sync.WaitGroup`). Timeout 2s por agente.

**Todo OK (200):**

```json
{
  "status": "ok",
  "service": "gateway",
  "agents": {"cita": "ok", "citas_ventas": "ok", "reserva": "ok", "venta": "ok"}
}
```

**Degradado (503):**

```json
{
  "status": "degraded",
  "service": "gateway",
  "agents": {"cita": "unreachable", "citas_ventas": "ok", "reserva": "disabled", "venta": "ok"}
}
```

| Estado agente | Significado |
|---|---|
| `ok` | Respondio con 2xx |
| `unreachable` | No responde o timeout |
| `disabled` | Deshabilitado via `AGENT_<KEY>_ENABLED=false` |
| `no_url` | Sin URL configurada |

### `GET /metrics` — Metricas Prometheus

- `gateway_requests_total{agent, status}` — Contador por agente y resultado (`ok`/`error`)
- `gateway_request_duration_seconds{agent}` — Histograma de latencia por agente

## Circuit Breaker

Cada agente tiene su propio circuit breaker ([gobreaker v2](https://github.com/sony/gobreaker)):

| Parametro | Valor |
|---|---|
| Umbral de apertura | 5 fallos consecutivos |
| Intervalo de evaluacion | 60s |
| Timeout en estado abierto | 60s |
| Max requests en half-open | 3 |

```
Closed ──(5 fallos)──> Open ──(60s)──> Half-Open ──(exito)──> Closed
                                             │
                                         (fallo)
                                             v
                                           Open
```

Los cambios de estado se registran en logs.

## Variables de entorno

### Servidor HTTP

| Variable | Default | Descripcion |
|---|---|---|
| `GATEWAY_HTTP_PORT` | `8000` | Puerto HTTP |
| `GATEWAY_READ_HEADER_TIMEOUT_SEC` | `10` | Timeout lectura de headers (mitiga slowloris) |
| `GATEWAY_READ_TIMEOUT_SEC` | `40` | Timeout lectura completa (headers + body) |
| `GATEWAY_WRITE_TIMEOUT_SEC` | `35` | Timeout escritura de respuesta. Debe ser > `AGENT_TIMEOUT` + 5s |
| `GATEWAY_IDLE_TIMEOUT_SEC` | `60` | Timeout conexiones keep-alive idle (`0` = desactivado) |
| `CORS_ALLOWED_ORIGINS` | `*` | Origenes permitidos (comma-separated) |
| `LOG_LEVEL` | `info` | Nivel de log: `debug`, `info`, `warn`, `error` |

### Agentes (dinamico)

Los agentes se detectan automaticamente por patron `AGENT_<KEY>_URL`:

| Variable | Default | Descripcion |
|---|---|---|
| `AGENT_<KEY>_URL` | — | URL del endpoint del agente. Agregar = registrar agente |
| `AGENT_<KEY>_ENABLED` | `true` | Habilitar/deshabilitar agente |
| `AGENT_TIMEOUT` | `25` | Timeout HTTP para llamadas a agentes (segundos) |

Ejemplo con 4 agentes:

```env
AGENT_VENTA_URL=http://localhost:8001/api/chat
AGENT_CITA_URL=http://localhost:8002/api/chat
AGENT_RESERVA_URL=http://localhost:8003/api/chat
AGENT_CITAS_VENTAS_URL=http://localhost:8004/api/chat

AGENT_VENTA_ENABLED=true
AGENT_CITA_ENABLED=true
AGENT_RESERVA_ENABLED=true
AGENT_CITAS_VENTAS_ENABLED=true
AGENT_TIMEOUT=25
```

## Contrato del agente

Cada agente backend debe exponer:

**POST** `<agent_url>` — Content-Type: `application/json`

**Body recibido:**
```json
{
  "message": "texto del usuario",
  "session_id": 3796,
  "context": {
    "config": {
      "nombre_bot": "MaravIA",
      "id_empresa": 1,
      "modalidad": "Citas",
      "personalidad": "amigable",
      "..."
    }
  }
}
```

**Respuesta esperada (200):**
```json
{"reply": "respuesta del agente", "url": null}
```

**Health check:** `GET /health` retornando 2xx.

## Seguridad

- **Limite de body:** 512 KB por request (previene DoS)
- **Timeouts HTTP:** ReadHeader, Read, Write, Idle (mitiga slowloris y conexiones colgadas)
- **Circuit breaker:** Aislamiento de fallos por agente
- **CORS configurable:** Origenes restringidos en produccion
- **Container no-root:** Ejecuta como `appuser` (UID 10001)
- **Binario estatico:** Sin dependencias de runtime en el container
- **Validacion de input:** campos requeridos, tipos, limites

## HTTP Client (Transport)

El gateway usa un `http.Client` compartido con transport tuneado:

| Parametro | Valor |
|---|---|
| `MaxConnsPerHost` | 25 |
| `MaxIdleConnsPerHost` | 10 |
| `MaxIdleConns` | 50 |
| `DialTimeout` | 5s |
| `KeepAlive` | 30s |
| `TLSHandshakeTimeout` | 5s |
| `ResponseHeaderTimeout` | 20s |
| `IdleConnTimeout` | 90s |
