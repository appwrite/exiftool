package main

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type telemetry struct {
	registry      *prometheus.Registry
	httpRequests  *prometheus.CounterVec
	httpDuration  *prometheus.HistogramVec
	httpActive    prometheus.Gauge
	stageDuration *prometheus.HistogramVec
	strips        *prometheus.CounterVec
	stripActive   prometheus.Gauge
	cancellations *prometheus.CounterVec
}

func newTelemetry(exiftoolVersion string, maxConcurrentStrips int) *telemetry {
	registry := prometheus.NewRegistry()
	t := &telemetry{
		registry: registry,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "exiftool", Subsystem: "http", Name: "requests_total",
			Help: "HTTP requests by route, method, and status code.",
		}, []string{"route", "method", "status"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "exiftool", Subsystem: "http", Name: "request_duration_seconds",
			Help: "HTTP request duration by route and method.",
		}, []string{"route", "method"}),
		httpActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "exiftool", Subsystem: "http", Name: "requests_active",
			Help: "HTTP requests currently being served.",
		}),
		stageDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "exiftool", Name: "stage_duration_seconds",
			Help: "Strip pipeline stage duration.",
		}, []string{"stage"}),
		strips: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "exiftool", Name: "strips_total",
			Help: "Strip requests by outcome.",
		}, []string{"outcome"}),
		stripActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "exiftool", Name: "strip_active",
			Help: "ExifTool commands currently running.",
		}),
		cancellations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "exiftool", Name: "cancellations_total",
			Help: "Cancelled strips by reason.",
		}, []string{"reason"}),
	}
	registry.MustRegister(t.httpRequests, t.httpDuration, t.httpActive,
		t.stageDuration, t.strips, t.stripActive, t.cancellations,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Namespace: "exiftool", Name: "build_info", Help: "Build, ExifTool version, and pool size.",
		ConstLabels: prometheus.Labels{
			"version": version, "commit": commit, "exiftool_version": exiftoolVersion,
			"max_concurrent_strips": strconv.Itoa(maxConcurrentStrips),
		},
	}, func() float64 { return 1 }))
	return t
}

func (t *telemetry) handler() http.Handler {
	return promhttp.HandlerFor(t.registry, promhttp.HandlerOpts{})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}

func (t *telemetry) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		route := telemetryRoute(r.URL.Path)
		requestID := validRequestID(r.Header.Get("X-Request-ID"))
		if requestID == "" {
			requestID = newRequestID()
		}
		w.Header().Set("X-Request-ID", requestID)
		recorder := &statusRecorder{ResponseWriter: w}
		t.httpActive.Inc()
		defer func() {
			t.httpActive.Dec()
			status := recorder.status
			if status == 0 {
				status = http.StatusOK
			}
			duration := time.Since(started)
			method := telemetryMethod(r.Method)
			t.httpRequests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
			t.httpDuration.WithLabelValues(route, method).Observe(duration.Seconds())
			slog.Info("request completed", "request_id", requestID, "method", loggedMethod(r.Method),
				"route", route, "status", status, "response_bytes", recorder.bytes,
				"duration_ms", duration.Milliseconds())
		}()
		next.ServeHTTP(recorder, r)
	})
}

func telemetryRoute(path string) string {
	switch path {
	case "/strip", "/livez", "/readyz", "/healthz", "/metrics":
		return path
	default:
		return "unmatched"
	}
}

const maxLoggedMethodLen = 32

func telemetryMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "other"
	}
}

func loggedMethod(method string) string {
	if len(method) <= maxLoggedMethodLen {
		return method
	}
	return method[:maxLoggedMethodLen] + "..."
}

func validRequestID(value string) string {
	if len(value) < 1 || len(value) > 64 {
		return ""
	}
	for _, char := range value {
		if !(char == '-' || char == '_' || char >= '0' && char <= '9' ||
			char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z') {
			return ""
		}
	}
	return value
}

func newRequestID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(data[:])
}
