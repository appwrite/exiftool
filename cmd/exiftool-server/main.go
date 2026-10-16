package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultExiftoolPath    = "exiftool"
	defaultShutdownTimeout = 30 * time.Second
	exiftoolStartupTimeout = 10 * time.Second
)

var (
	version = "dev"
	commit  = "unknown"
)

type application struct {
	exiftoolVersion string
	telemetry       *telemetry
	draining        atomic.Bool
}

type healthResponse struct {
	Status string `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := envOrDefault("ADDR", ":8080")
	exiftoolPath := envOrDefault("EXIFTOOL_PATH", defaultExiftoolPath)
	shutdownTimeout, err := positiveEnvDuration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout)
	if err != nil {
		return fmt.Errorf("startup: %w", err)
	}

	versionCtx, cancelVersion := context.WithTimeout(context.Background(), exiftoolStartupTimeout)
	exiftoolVersion, err := detectExiftoolVersion(versionCtx, exiftoolPath)
	cancelVersion()
	if err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	slog.Info("exiftool found", "path", exiftoolPath, "version", exiftoolVersion)

	app := newApplication(exiftoolVersion)
	server := &http.Server{
		Addr:              addr,
		Handler:           app.telemetry.instrument(app.routes()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      35 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serviceCtx, cancelService := context.WithCancel(context.Background())
	defer cancelService()
	server.BaseContext = func(net.Listener) context.Context { return serviceCtx }

	shutdownSignals := make(chan os.Signal, 1)
	signal.Notify(shutdownSignals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(shutdownSignals)
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("listening", "address", addr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	select {
	case sig := <-shutdownSignals:
		slog.Info("shutting down", "signal", sig.String())
	case err := <-serverErrors:
		return fmt.Errorf("serve HTTP: %w", err)
	}

	app.draining.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		cancelService()
		if closeErr := server.Close(); closeErr != nil {
			return errors.Join(fmt.Errorf("graceful shutdown: %w", err), fmt.Errorf("force close: %w", closeErr))
		}
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	return nil
}

func newApplication(exiftoolVersion string) *application {
	return &application{
		exiftoolVersion: exiftoolVersion,
		telemetry:       newTelemetry(exiftoolVersion),
	}
}

func (app *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/livez", app.handleLivez)
	mux.HandleFunc("/readyz", app.handleReadyz)
	mux.HandleFunc("/healthz", app.handleReadyz)
	mux.Handle("/metrics", app.telemetry.handler())
	return mux
}

func detectExiftoolVersion(ctx context.Context, path string) (string, error) {
	output, err := exec.CommandContext(ctx, path, "-ver").Output()
	if err != nil {
		return "", fmt.Errorf("run %s -ver: %w", path, err)
	}
	detected := strings.TrimSpace(string(output))
	if detected == "" {
		return "", fmt.Errorf("%s -ver printed no version", path)
	}
	return detected, nil
}

func (app *application) handleLivez(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

func (app *application) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if app.exiftoolVersion == "" || app.draining.Load() {
		writeError(w, http.StatusServiceUnavailable, "service not ready")
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("write response failed", "error", err)
	}
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func positiveEnvDuration(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration, got %q", key, value)
	}
	return parsed, nil
}
