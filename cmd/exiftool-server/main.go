package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"exiftool/internal/exiftool"
)

const (
	defaultExiftoolPath    = "exiftool"
	defaultMaxRequestBytes = 25 << 20 // 25 MiB: Appwrite strips files up to 20 MB, plus multipart overhead.
	maxConcurrentUploads   = 4        // Bounds buffered bodies without reserving a process.
	defaultStripTimeout    = 30 * time.Second
	defaultShutdownTimeout = 30 * time.Second
	exiftoolStartupTimeout = 10 * time.Second
)

var (
	version = "dev"
	commit  = "unknown"
)

type stripper interface {
	// Strip rewrites the file at path in place without its metadata.
	Strip(context.Context, string) error
}

type application struct {
	exiftoolVersion string
	stripper        stripper
	uploadSlots     chan struct{}
	stripSlots      chan struct{}
	telemetry       *telemetry
	stripTimeout    time.Duration
	maxRequestBytes int64
	tempDir         string
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
	maxConcurrentStrips, err := positiveEnvInt("MAX_CONCURRENT_STRIPS", runtime.GOMAXPROCS(0))
	if err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	stripTimeout, err := positiveEnvDuration("STRIP_TIMEOUT", defaultStripTimeout)
	if err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	shutdownTimeout, err := positiveEnvDuration("SHUTDOWN_TIMEOUT", defaultShutdownTimeout)
	if err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	maxRequestBytes, err := positiveEnvBytes("MAX_REQUEST_SIZE", defaultMaxRequestBytes)
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

	started := time.Now()
	pool, err := exiftool.New(context.Background(), exiftool.Options{
		Binary:         exiftoolPath,
		Size:           maxConcurrentStrips,
		StartupTimeout: exiftoolStartupTimeout,
	})
	if err != nil {
		return fmt.Errorf("startup: %w", err)
	}
	defer pool.Close()
	slog.Info("exiftool processes started", "processes", maxConcurrentStrips,
		"duration_ms", time.Since(started).Milliseconds())

	app := newApplication(exiftoolVersion, pool, maxConcurrentStrips)
	app.stripTimeout = stripTimeout
	app.maxRequestBytes = maxRequestBytes
	server := &http.Server{
		Addr:              addr,
		Handler:           app.telemetry.instrument(app.routes()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       stripTimeout,
		WriteTimeout:      stripTimeout + 5*time.Second,
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
		slog.Info("listening", "address", addr, "max_request_bytes", maxRequestBytes)
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

func newApplication(exiftoolVersion string, stripper stripper, maxConcurrentStrips int) *application {
	return &application{
		exiftoolVersion: exiftoolVersion,
		stripper:        stripper,
		uploadSlots:     make(chan struct{}, maxConcurrentUploads),
		stripSlots:      make(chan struct{}, maxConcurrentStrips),
		telemetry:       newTelemetry(exiftoolVersion),
		stripTimeout:    defaultStripTimeout,
		maxRequestBytes: defaultMaxRequestBytes,
	}
}

func (app *application) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/strip", app.handleStrip)
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
	if app.exiftoolVersion == "" || app.stripper == nil || app.draining.Load() {
		writeError(w, http.StatusServiceUnavailable, "service not ready")
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

func (app *application) handleStrip(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if app.draining.Load() {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "service is shutting down")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), app.stripTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	outcome := "success"
	defer func() { app.telemetry.strips.WithLabelValues(outcome).Inc() }()
	observeCancellation := func() {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			outcome = "timeout"
			app.telemetry.cancellations.WithLabelValues("deadline").Inc()
		} else {
			outcome = "cancelled"
			app.telemetry.cancellations.WithLabelValues("client").Inc()
		}
	}
	uploadWaitStarted := time.Now()
	select {
	case app.uploadSlots <- struct{}{}:
		app.telemetry.stageDuration.WithLabelValues("upload_queue").Observe(time.Since(uploadWaitStarted).Seconds())
	case <-ctx.Done():
		observeCancellation()
		writeContextError(w, ctx)
		return
	}
	uploadSlotHeld := true
	defer func() {
		if uploadSlotHeld {
			<-app.uploadSlots
		}
	}()

	r.Body = http.MaxBytesReader(w, r.Body, app.maxRequestBytes)
	defer r.Body.Close()
	data, err := readFile(r)
	if err != nil {
		// The server's read deadline starts before this handler, so a slow upload
		// can hit it first. The server then also cancels the request context,
		// which would otherwise look like the client leaving.
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			outcome = "timeout"
			app.telemetry.cancellations.WithLabelValues("deadline").Inc()
			writeError(w, http.StatusGatewayTimeout, "strip timed out")
			return
		}
		if ctx.Err() != nil {
			observeCancellation()
			writeContextError(w, ctx)
			return
		}
		outcome = "invalid_request"
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file exceeds the %s request limit", formatByteSize(app.maxRequestBytes)))
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	stripWaitStarted := time.Now()
	select {
	case app.stripSlots <- struct{}{}:
		app.telemetry.stageDuration.WithLabelValues("strip_queue").Observe(time.Since(stripWaitStarted).Seconds())
		defer func() { <-app.stripSlots }()
	case <-ctx.Done():
		observeCancellation()
		writeContextError(w, ctx)
		return
	}
	// Keep the upload slot while waiting for a process so buffered request bodies
	// remain bounded. Release it once the strip slot is secured.
	<-app.uploadSlots
	uploadSlotHeld = false

	// No extension, so ExifTool detects the format from the content and a
	// renamed file is still stripped.
	file, err := os.CreateTemp(app.tempDir, "strip-*")
	if err != nil {
		outcome = "internal_error"
		slog.Error("create temporary file failed", "error", err)
		writeError(w, http.StatusInternalServerError, "strip failed")
		return
	}
	path := file.Name()
	defer os.Remove(path)
	_, writeErr := file.Write(data)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		outcome = "internal_error"
		slog.Error("write temporary file failed", "error", err)
		writeError(w, http.StatusInternalServerError, "strip failed")
		return
	}

	stripStarted := time.Now()
	app.telemetry.stripActive.Inc()
	err = app.stripper.Strip(ctx, path)
	app.telemetry.stripActive.Dec()
	app.telemetry.stageDuration.WithLabelValues("strip").Observe(time.Since(stripStarted).Seconds())
	if err != nil {
		if ctx.Err() != nil {
			observeCancellation()
			writeContextError(w, ctx)
			return
		}
		if errors.Is(err, exiftool.ErrNotStripped) {
			outcome = "not_stripped"
			writeError(w, http.StatusUnprocessableEntity, "file format is not supported or the file is corrupt")
			return
		}
		outcome = "strip_error"
		slog.Error("strip failed", "error", err)
		writeError(w, http.StatusInternalServerError, "strip failed")
		return
	}

	stripped, err := os.ReadFile(path)
	if err != nil || len(stripped) == 0 {
		outcome = "internal_error"
		slog.Error("read stripped file failed", "error", err, "bytes", len(stripped))
		writeError(w, http.StatusInternalServerError, "strip failed")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(stripped)))
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(stripped); err != nil {
		slog.Error("write response failed", "error", err)
	}
}

func writeContextError(w http.ResponseWriter, ctx context.Context) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, "strip timed out")
		return
	}
	writeError(w, 499, "request cancelled")
}

func readFile(r *http.Request) ([]byte, error) {
	contentType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, errors.New("invalid Content-Type header")
	}
	if contentType != "multipart/form-data" {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}
		if len(data) == 0 {
			return nil, errors.New("empty file body")
		}
		return data, nil
	}

	if params["boundary"] == "" {
		return nil, errors.New("multipart boundary is missing")
	}
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, fmt.Errorf("read multipart body: %w", err)
	}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read multipart body: %w", err)
		}
		if part.FormName() != "file" {
			part.Close()
			continue
		}
		data, readErr := io.ReadAll(part)
		part.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read file field: %w", readErr)
		}
		if len(data) == 0 {
			return nil, errors.New("file field is empty")
		}
		return data, nil
	}
	return nil, errors.New("multipart field \"file\" is required")
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

func positiveEnvInt(key string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, value)
	}
	return parsed, nil
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

func positiveEnvBytes(key string, fallback int64) (int64, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := parseByteSize(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a positive byte size such as 26214400 or 25MiB, got %q", key, value)
	}
	return parsed, nil
}

func parseByteSize(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("empty byte size")
	}
	upper := strings.ToUpper(value)
	for _, unit := range []struct {
		suffix string
		bytes  int64
	}{
		{"GIB", 1 << 30},
		{"MIB", 1 << 20},
		{"KIB", 1 << 10},
		{"GB", 1_000_000_000},
		{"MB", 1_000_000},
		{"KB", 1_000},
		{"B", 1},
	} {
		if !strings.HasSuffix(upper, unit.suffix) {
			continue
		}
		number := strings.TrimSpace(value[:len(value)-len(unit.suffix)])
		parsed, err := strconv.ParseInt(number, 10, 64)
		if err != nil || parsed < 1 {
			return 0, fmt.Errorf("invalid byte size %q", value)
		}
		if parsed > math.MaxInt64/unit.bytes {
			return 0, fmt.Errorf("byte size %q overflows", value)
		}
		return parsed * unit.bytes, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("invalid byte size %q", value)
	}
	return parsed, nil
}

func formatByteSize(n int64) string {
	switch {
	case n <= 0:
		return fmt.Sprintf("%d bytes", n)
	case n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	case n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
