package metrics

// Prometheus client (código en GitHub): https://github.com/prometheus/client_golang/tree/main/prometheus
import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// RequestsTotal counts chat requests by agent and status.
	RequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Total chat requests by agent and status",
		},
		[]string{"agent", "status"},
	)
	// RequestDurationSeconds is the latency of chat requests.
	RequestDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "Chat request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"agent"},
	)
)
