package hub

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// serverMetrics holds the hub's Prometheus collectors. Each Server owns its
// own registry so tests can build servers freely without duplicate
// registration panics. All record methods are nil-receiver safe: tests that
// construct &Server{} by hand simply record nothing.
type serverMetrics struct {
	registry *prometheus.Registry

	httpRequests  *prometheus.CounterVec   // route, method, status
	httpDuration  *prometheus.HistogramVec // route, method
	wsMessages    *prometheus.CounterVec   // direction (in/out), peer (claw/user)
	webhookErrors *prometheus.CounterVec   // integration
}

// clawStatusCollector exports elasticclaw_claws{status} gauges computed from
// the claws table at scrape time (a cheap GROUP BY on SQLite).
type clawStatusCollector struct {
	db   *sql.DB
	desc *prometheus.Desc
}

// knownClawStatuses is the baseline set always exported (as zero when no claw
// has that status) so dashboards and alerts see the series from the start.
// Unexpected statuses found in the DB are exported as well.
var knownClawStatuses = []string{"provisioning", "starting", "connected", "offline", "error", "deleted"}

func (c *clawStatusCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *clawStatusCollector) Collect(ch chan<- prometheus.Metric) {
	counts := make(map[string]float64, len(knownClawStatuses))
	for _, status := range knownClawStatuses {
		counts[status] = 0
	}
	rows, err := c.db.Query(`SELECT status, COUNT(*) FROM claws GROUP BY status`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var count float64
		if err := rows.Scan(&status, &count); err != nil {
			continue
		}
		counts[status] = count
	}
	for status, count := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, count, status)
	}
}

func newServerMetrics(db *sql.DB) *serverMetrics {
	m := &serverMetrics{
		registry: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "elasticclaw_http_requests_total",
			Help: "HTTP requests handled by the hub, by route pattern, method and status code.",
		}, []string{"route", "method", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "elasticclaw_http_request_duration_seconds",
			Help:    "HTTP request latency by route pattern and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		wsMessages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "elasticclaw_ws_messages_total",
			Help: "WebSocket messages by direction (in/out) and peer (claw/user).",
		}, []string{"direction", "peer"}),
		webhookErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "elasticclaw_webhook_errors_total",
			Help: "Integration webhook deliveries answered with a 4xx/5xx status, by integration.",
		}, []string{"integration"}),
	}
	m.registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.httpRequests,
		m.httpDuration,
		m.wsMessages,
		m.webhookErrors,
	)
	if db != nil {
		m.registry.MustRegister(collectors.NewDBStatsCollector(db, "hub"))
		m.registry.MustRegister(&clawStatusCollector{
			db: db,
			desc: prometheus.NewDesc("elasticclaw_claws",
				"Number of claws by status.", []string{"status"}, nil),
		})
	}
	return m
}

// handler serves the Prometheus exposition format for this server's registry.
func (m *serverMetrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// observeRequest records one handled HTTP request.
func (m *serverMetrics) observeRequest(route, method string, status int, elapsed time.Duration) {
	if m == nil {
		return
	}
	m.httpRequests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
	m.httpDuration.WithLabelValues(route, method).Observe(elapsed.Seconds())
}

// wsMessage counts one WebSocket message. direction is "in" or "out"; peer is
// "claw" (bridge connection) or "user" (browser connection).
func (m *serverMetrics) wsMessage(direction, peer string) {
	if m == nil {
		return
	}
	m.wsMessages.WithLabelValues(direction, peer).Inc()
}

// webhookError counts one failed integration webhook delivery.
func (m *serverMetrics) webhookError(integration string) {
	if m == nil {
		return
	}
	m.webhookErrors.WithLabelValues(integration).Inc()
}

// webhookIntegration extracts the integration name from a webhook route
// pattern ("/api/integrations/linear/webhook" or
// "/api/workspaces/{workspace}/webhooks/linear"); it returns "" for
// non-webhook routes.
func webhookIntegration(route string) string {
	parts := strings.Split(strings.Trim(route, "/"), "/")
	switch {
	case len(parts) == 4 && parts[0] == "api" && parts[1] == "integrations" && parts[3] == "webhook":
		return parts[2]
	case len(parts) == 5 && parts[0] == "api" && parts[1] == "workspaces" && parts[3] == "webhooks":
		return parts[4]
	}
	return ""
}
