package main

import (
	"log"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const meterName = "YOLOSWAG"

var (
	// HTTPMetrics about the public surface area (http requests, cdn stuff)
	HTTPMetrics = prometheus.NewRegistry()
	// ClientMetrics about the drand client requests to servers
	ClientMetrics = prometheus.NewRegistry()

	// HTTPCallCounter (HTTP) how many http requests
	HTTPCallCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_call_counter",
		Help: "Number of HTTP calls received",
	}, []string{"code", "method"})
	// HTTPLatency (HTTP) how long http request handling takes
	HTTPLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:        "http_response_duration",
		Help:        "histogram of request latencies",
		Buckets:     prometheus.DefBuckets,
		ConstLabels: prometheus.Labels{"handler": "http"},
	}, []string{"method"})
	// HTTPInFlight (HTTP) how many http requests exist
	HTTPInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "http_in_flight",
		Help: "A gauge of requests currently being served.",
	})

	// Client observation metrics

	// ClientWatchLatency measures the latency of the watch channel from the client's perspective.
	ClientWatchLatency = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "client_watch_latency",
		Help: "Duration between time round received and time round expected.",
	})

	// ClientHTTPHeartbeatSuccess measures the success rate of HTTP hearbeat randomness requests.
	ClientHTTPHeartbeatSuccess = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "client_http_heartbeat_success",
		Help: "Number of successful HTTP heartbeats.",
	}, []string{"http_address"})

	// ClientHTTPHeartbeatFailure measures the number of times HTTP heartbeats fail.
	ClientHTTPHeartbeatFailure = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "client_http_heartbeat_failure",
		Help: "Number of unsuccessful HTTP heartbeats.",
	}, []string{"http_address"})

	// ClientHTTPHeartbeatLatency measures the randomness latency of an HTTP source.
	ClientHTTPHeartbeatLatency = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "client_http_heartbeat_latency",
		Help: "Randomness latency of an HTTP source.",
	}, []string{"http_address"})

	// ClientInFlight measures how many active requests have been made
	ClientInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "client_in_flight",
		Help: "A gauge of in-flight drand client http requests.",
	},
		[]string{"url"},
	)

	// ClientRequests measures how many total requests have been made
	ClientRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "client_api_requests_total",
			Help: "A counter for requests from the drand client.",
		},
		[]string{"code", "method", "url"},
	)

	// ClientDNSLatencyVec tracks the observed DNS resolution times
	ClientDNSLatencyVec = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "client_dns_duration_seconds",
			Help:    "Client drand dns latency histogram.",
			Buckets: []float64{.005, .01, .025, .05},
		},
		[]string{"event", "url"},
	)

	// ClientTLSLatencyVec tracks observed TLS connection times
	ClientTLSLatencyVec = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "client_tls_duration_seconds",
			Help:    "Client drand tls latency histogram.",
			Buckets: []float64{.05, .1, .25, .5},
		},
		[]string{"event", "url"},
	)

	// ClientLatencyVec tracks raw http request latencies
	ClientLatencyVec = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "client_request_duration_seconds",
			Help:    "A histogram of client request latencies.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"url"},
	)
)

func serveMetrics() {
	bindMetrics()
	handler := promhttp.HandlerFor(prometheus.Gatherers{HTTPMetrics, ClientMetrics}, promhttp.HandlerOpts{
		Registry: HTTPMetrics,
		// Opt into OpenMetrics e.g. to support exemplars.
		EnableOpenMetrics: true,
	})

	slog.Info("starting to serve metrics on localhost:9999/metrics")
	http.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("serving metrics on localhost:9999/metrics")
		handler.ServeHTTP(w, r)
	}))
	err := http.ListenAndServe(":9999", nil) //nolint:gosec // Ignoring G114
	if err != nil {
		log.Fatalf("error serving http: %v", err)
		return
	}
}

func bindMetrics() {
	// HTTP metrics
	httpMetrics := []prometheus.Collector{
		HTTPCallCounter,
		HTTPLatency,
		HTTPInFlight,
	}
	for _, c := range httpMetrics {
		if err := HTTPMetrics.Register(c); err != nil {
			slog.Error("error in bindMetrics", "metrics", "bindMetrics", "err", err)
			return
		}
	}

	// Client metrics
	if err := RegisterClientMetrics(ClientMetrics); err != nil {
		slog.Error("error in bindMetrics", "metrics", "bindMetrics", "err", err)
		return
	}
}

// RegisterClientMetrics registers drand client metrics with the given registry
func RegisterClientMetrics(r prometheus.Registerer) error {
	// Client metrics
	client := []prometheus.Collector{
		ClientDNSLatencyVec,
		ClientInFlight,
		ClientLatencyVec,
		ClientRequests,
		ClientTLSLatencyVec,
		ClientWatchLatency,
		ClientHTTPHeartbeatSuccess,
		ClientHTTPHeartbeatFailure,
		ClientHTTPHeartbeatLatency,
	}
	for _, c := range client {
		if err := r.Register(c); err != nil {
			return err
		}
	}
	return nil
}
