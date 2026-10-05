// Package metrics defines Prometheus collectors shared by the API and the worker.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "docproc"

type Metrics struct {
	registry *prometheus.Registry

	HTTPRequests *prometheus.CounterVec
	HTTPDuration *prometheus.HistogramVec

	JobsCreated     *prometheus.CounterVec
	JobsProcessed   *prometheus.CounterVec
	RenderDuration  *prometheus.HistogramVec
	WorkersBusy     prometheus.Gauge
	OutboxPublished prometheus.Counter
}

func New() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "http_requests_total", Help: "HTTP requests by route and status code.",
		}, []string{"method", "route", "code"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "http_request_duration_seconds", Help: "HTTP request latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		JobsCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "jobs_created_total", Help: "Accepted jobs by template.",
		}, []string{"template"}),
		JobsProcessed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "jobs_processed_total", Help: "Processed jobs by template and outcome (done, retry, failed).",
		}, []string{"template", "outcome"}),
		RenderDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "render_duration_seconds", Help: "Time spent rendering a document.",
			Buckets: []float64{.01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"template"}),
		WorkersBusy: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "workers_busy", Help: "Workers currently processing a message.",
		}),
		OutboxPublished: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "outbox_published_total", Help: "Messages relayed from the outbox to the broker.",
		}),
	}

	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.HTTPRequests, m.HTTPDuration, m.JobsCreated, m.JobsProcessed,
		m.RenderDuration, m.WorkersBusy, m.OutboxPublished,
	)
	return m
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{Registry: m.registry})
}
