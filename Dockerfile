# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26
FROM golang:${GO_VERSION}-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /gateway ./cmd/gateway

# Imagen final mínima (solo para ejecutar el binario)
FROM alpine:3.19

RUN adduser -D -u 10001 -g appuser appuser

WORKDIR /app
COPY --from=builder /gateway .

USER appuser

EXPOSE 8000

CMD ["./gateway"]
