# MaravIA Gateway

API Gateway inteligente en Go que recibe requests de **n8n** y enruta al agente especializado segun la **modalidad** del negocio (ventas, citas, reservas). Implementa circuit breaker, metricas Prometheus y health checks compuestos.

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

## Stack

| Componente | Tecnologia |
|---|---|
| Router | [Chi v5](https://github.com/go-chi/chi) |
| Config | [cleanenv](https://github.com/ilyakaznacheev/cleanenv) (env -> struct) |
| Logging | `log/slog` (stdlib, JSON estructurado) |
| Metricas | [Prometheus client_golang](https://github.com/prometheus/client_golang) |
| Circuit Breaker | [gobreaker v2](https://github.com/sony/gobreaker) (por agente) |
| HTTP Client | `net/http.Client` (connection pooling) |

## Requisitos

- Go 1.26+ (`go.mod` declara `go 1.26.0`)
- Docker y Docker Compose (opcional, para despliegue)

## Inicio rapido

### Desarrollo local

```bash
# Clonar y entrar al proyecto
cd gateway

# Copiar variables de entorno
cp .env.example .env

# Descargar dependencias y ejecutar
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
# Build y run con Docker
docker build -t maravia-gateway .
docker run -p 8000:8000 --env-file .env maravia-gateway

# O con Docker Compose
docker compose up --build
```

La imagen Docker usa **multi-stage build** (golang:1.26-alpine -> alpine:3.19), binario estatico, usuario no-root (`appuser`). Imagen final ~20-30 MB.

## Endpoints

### `GET /` - Info del servicio

```json
{
  "service": "MaravIA Gateway",
  "status": "running",
  "endpoints": {
    "/api/agent/chat": "POST",
    "/health": "GET",
    "/metrics": "GET"
  }
}
```

### `POST /api/agent/chat` - Chat principal

Endpoint principal. Recibe el request de n8n, determina el agente por `config.modalidad` y hace proxy.

**Request:**

```json
{
  "message": "Quiero agendar una cita para manana",
  "session_id": 123,
  "config": {
    "nombre_bot": "AsistenteIA",
    "id_empresa": 1,
    "rol_bot": "asistente",
    "tipo_bot": "chat",
    "objetivo_principal": "Agendar citas",
    "modalidad": "Citas",
    "frase_saludo": "Hola! En que puedo ayudarte?",
    "frase_des": "Soy tu asistente virtual",
    "frase_esc": "Dame un momento",
    "personalidad": "amigable",
    "duracion_cita_minutos": 30,
    "slots": 5,
    "agendar_usuario": true,
    "agendar_sucursal": false,
    "usuario_id": 42,
    "correo_usuario": "usuario@ejemplo.com"
  }
}
```

**Response exitosa (200):**

```json
{
  "reply": "Claro, te ayudare a agendar tu cita. Que dia te conviene?",
  "session_id": 123,
  "agent_used": "cita",
  "action": "delegate"
}
```

**Response en error de agente (200):** el gateway responde 200 con mensaje seguro para que n8n no rompa el flujo.

```json
{
  "reply": "No pude conectar con el agente. Intenta de nuevo en un momento.",
  "session_id": 123,
  "agent_used": "cita",
  "action": "delegate"
}
```

**Errores de validacion:**

| Status | Causa |
|---|---|
| 400 | JSON invalido, `message` vacio, `session_id` negativo, `config.id_empresa` <= 0 |
| 405 | Metodo distinto a POST |
| 413 | Body mayor a 512 KB |

### `GET /health` - Health check compuesto

Verifica el gateway y cada agente habilitado (GET a `{base_url}/health`, timeout 2s).

**Todo OK (200):**

```json
{
  "status": "ok",
  "service": "gateway",
  "agents": {
    "venta": "ok",
    "cita": "ok",
    "reserva": "ok",
    "citas_ventas": "ok"
  }
}
```

**Degradado (503):**

```json
{
  "status": "degraded",
  "service": "gateway",
  "agents": {
    "venta": "ok",
    "cita": "unreachable",
    "reserva": "disabled",
    "citas_ventas": "no_url"
  }
}
```

| Estado agente | Significado |
|---|---|
| `ok` | Respondio con 2xx |
| `unreachable` | No responde o timeout |
| `disabled` | Deshabilitado via config |
| `no_url` | Sin URL configurada |

### `GET /metrics` - Metricas Prometheus

Expone metricas en formato Prometheus:

- `gateway_requests_total{agent, status}` - Contador de requests por agente y resultado (`ok`/`error`)
- `gateway_request_duration_seconds{agent}` - Histograma de latencia por agente

## Enrutado por modalidad

El campo `config.modalidad` del request de n8n determina a que agente se envia. Se normaliza con `trim + lowercase`:

| Modalidad (n8n) | Agente | Variable de entorno |
|---|---|---|
| `Citas` | `cita` | `AGENT_CITA_URL` |
| `Ventas` | `venta` | `AGENT_VENTA_URL` |
| `Reservas` | `reserva` | `AGENT_RESERVA_URL` |
| `Citas y Ventas` | `citas_ventas` | `AGENT_CITAS_VENTAS_URL` |
| _(cualquier otro)_ | `cita` | `AGENT_CITA_URL` (fallback) |

No usa LLM para routing; es proxy directo por valor de modalidad.

## Circuit Breaker

Cada agente tiene su propio circuit breaker ([gobreaker](https://github.com/sony/gobreaker)) que previene cascadas de fallos:

| Parametro | Valor |
|---|---|
| Umbral de apertura | 5 fallos consecutivos |
| Intervalo de evaluacion | 60 segundos |
| Timeout en estado abierto | 60 segundos |
| Max requests en half-open | 3 |

**Flujo de estados:**

```
Closed ──(5 fallos)──> Open ──(60s)──> Half-Open ──(exito)──> Closed
                                            │
                                        (fallo)
                                            │
                                            v
                                          Open
```

Los cambios de estado se registran en los logs.

## Variables de entorno

### Servidor HTTP

| Variable | Default | Descripcion |
|---|---|---|
| `GATEWAY_HTTP_PORT` | `8000` | Puerto HTTP |
| `GATEWAY_READ_HEADER_TIMEOUT_SEC` | `10` | Timeout lectura de headers (mitiga slowloris) |
| `GATEWAY_READ_TIMEOUT_SEC` | `30` | Timeout lectura completa (headers + body) |
| `GATEWAY_WRITE_TIMEOUT_SEC` | `30` | Timeout escritura de respuesta |
| `GATEWAY_IDLE_TIMEOUT_SEC` | `60` | Timeout conexiones keep-alive idle (`0` = desactivado) |

### CORS y Logging

| Variable | Default | Descripcion |
|---|---|---|
| `CORS_ALLOWED_ORIGINS` | `*` | Origenes permitidos (comma-separated) |
| `LOG_LEVEL` | `info` | Nivel de log: `debug`, `info`, `warn`, `error` |

### Agentes

| Variable | Default | Descripcion |
|---|---|---|
| `AGENT_VENTA_URL` | `http://localhost:8001/api/chat` | URL del agente de ventas |
| `AGENT_CITA_URL` | `http://localhost:8002/api/chat` | URL del agente de citas |
| `AGENT_RESERVA_URL` | `http://localhost:8003/api/chat` | URL del agente de reservas |
| `AGENT_CITAS_VENTAS_URL` | `http://localhost:8004/api/chat` | URL del agente combinado |
| `AGENT_VENTA_ENABLED` | `true` | Habilitar/deshabilitar agente de ventas |
| `AGENT_CITA_ENABLED` | `true` | Habilitar/deshabilitar agente de citas |
| `AGENT_RESERVA_ENABLED` | `true` | Habilitar/deshabilitar agente de reservas |
| `AGENT_CITAS_VENTAS_ENABLED` | `true` | Habilitar/deshabilitar agente combinado |
| `AGENT_TIMEOUT` | `30` | Timeout HTTP para llamadas a agentes (segundos) |

## Contrato del agente

Cada agente backend debe exponer:

- **Metodo:** `POST`
- **Content-Type:** `application/json`
- **Body recibido:**
  ```json
  {
    "message": "texto del usuario",
    "session_id": 123,
    "context": {
      "nombre_bot": "...",
      "id_empresa": 1,
      "modalidad": "Citas",
      "..."
    }
  }
  ```
- **Respuesta esperada (200):**
  ```json
  { "reply": "respuesta del agente" }
  ```
- **Health check:** `GET /health` retornando 2xx

## Estructura del proyecto

```
gateway/
├── cmd/gateway/
│   └── main.go              # Entry point, router, graceful shutdown
├── internal/
│   ├── config/
│   │   └── config.go        # Carga de env vars, lookup de URLs/enabled
│   ├── handler/
│   │   ├── chat.go          # POST /api/agent/chat
│   │   └── health.go        # GET /health (compuesto)
│   ├── metrics/
│   │   └── metrics.go       # Prometheus: counters + histogramas
│   ├── middleware/
│   │   ├── cors.go          # CORS configurable
│   │   └── logger.go        # Request logging (method, path, status, duration)
│   └── proxy/
│       └── agents.go        # Invocacion de agentes + circuit breaker
├── .env.example
├── Dockerfile               # Multi-stage build (alpine, non-root)
├── compose.yaml
├── go.mod
└── go.sum
```

## Seguridad

- **Limite de body:** 512 KB por request (previene DoS)
- **Timeouts HTTP:** ReadHeader, Read, Write, Idle (mitiga slowloris y conexiones colgadas)
- **Circuit breaker:** Aislamiento de fallos por agente
- **CORS configurable:** Origenes restringidos en produccion
- **Container no-root:** Ejecuta como `appuser` (UID 10001)
- **Binario estatico:** Sin dependencias de runtime en el container
- **Validacion de input:** JSON schema, campos requeridos, tipos
