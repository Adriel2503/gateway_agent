package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Recorder wraps Prometheus metrics for chat requests.
type Recorder struct {
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
}

// NewRecorder creates a Recorder with registered Prometheus metrics.
func NewRecorder() *Recorder {
	return &Recorder{
		requestsTotal: promauto.NewCounterVec(
			prometheus.CounterOpts{
				Name: "gateway_requests_total",
				Help: "Total chat requests by agent and status",
			},
			[]string{"agent", "status"},
		),
		requestDuration: promauto.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "gateway_request_duration_seconds",
				Help:    "Chat request duration in seconds",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"agent"},
		),
	}
}

// Record registers a completed request with the given agent, status, and duration.
func (r *Recorder) Record(agent, status string, duration time.Duration) {
	r.requestsTotal.WithLabelValues(agent, status).Inc()
	r.requestDuration.WithLabelValues(agent).Observe(duration.Seconds())
}
