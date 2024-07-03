package main

import (
	"log/slog"
	"net/http"

	"github.com/drand/http-server/grpc"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// HTTPMetrics about the public surface area (http requests, cdn stuff)
	HTTPMetrics = prometheus.NewRegistry()

	// HTTPCallCounter (HTTP) how many http requests
	HTTPCallCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_call_counter",
		Help: "Number of HTTP calls received",
	}, []string{"code", "method"})

	// HTTPLatency (HTTP) how long http request handling takes
	HTTPLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "http_response_duration",
		Help: "histogram of request latencies",
		// Based on our current AWS same region latency:
		//  P50       P75      P90       P95      P99      P99.9     P99.99
		//  1.094ms  6.62ms  17.886ms  25.78ms  48.124ms  81.533ms  123.466ms
		// with extra long buckets to try and catch the connections that are "too early"
		Buckets:     []float64{.002, .007, .02, .05, .125, .5, 1, 2, 5, 10, 25},
		ConstLabels: prometheus.Labels{"handler": "http"},
	}, []string{"method"})

	// HTTPInFlight (HTTP) how many http requests exist
	HTTPInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "http_in_flight",
		Help: "A gauge of requests currently being served.",
	})
)

func serveMetrics() {
	bindMetrics()
	handler := promhttp.HandlerFor(prometheus.Gatherers{HTTPMetrics, grpc.ClientMetrics}, promhttp.HandlerOpts{
		Registry: HTTPMetrics,
		// Opt into OpenMetrics e.g. to support exemplars.
		EnableOpenMetrics: true,
	})

	mClient, err := grpc.CreateChannelzMonitor()
	if err != nil {
		slog.Error("error creating channelz monitor", "err", err)
		return
	}

	slog.Info("starting to serve metrics on /metrics")
	http.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Debug("serving metrics on /metrics")
		grpc.UpdateMetrics(mClient)
		handler.ServeHTTP(w, r)
	}))
	http.Handle("/chanz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		slog.Debug("display channelz data on /chanz")
		w.Write([]byte(grpc.UpdateMetrics(mClient)))
	}))
	//nolint:gosec // Ignoring G114
	if err := http.ListenAndServe(*metricFlag, nil); err != nil {
		slog.Error("error serving http metrics", "addr", *metricFlag, "err", err)
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
}
