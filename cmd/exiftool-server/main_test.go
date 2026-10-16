package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"exiftool/internal/exiftool"
)

func serve(app *application, method, path string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for key, values := range header {
		req.Header[key] = values
	}
	rec := httptest.NewRecorder()
	app.telemetry.instrument(app.routes()).ServeHTTP(rec, req)
	return rec
}

func TestHealthEndpoints(t *testing.T) {
	app := newApplication("13.55", fakeStripper(), 1)
	for _, path := range []string{"/livez", "/readyz", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			rec := serve(app, http.MethodGet, path, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			var body healthResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Status != "ok" {
				t.Fatalf("status = %q, want ok", body.Status)
			}
		})
	}
}

func TestHealthEndpointsRejectOtherMethods(t *testing.T) {
	app := newApplication("13.55", fakeStripper(), 1)
	for _, path := range []string{"/livez", "/readyz", "/healthz"} {
		t.Run(path, func(t *testing.T) {
			rec := serve(app, http.MethodPost, path, nil)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
			if got := rec.Header().Get("Allow"); got != http.MethodGet {
				t.Fatalf("Allow = %q, want GET", got)
			}
		})
	}
}

func TestReadinessDuringShutdown(t *testing.T) {
	app := newApplication("13.55", fakeStripper(), 1)
	app.draining.Store(true)
	if rec := serve(app, http.MethodGet, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if rec := serve(app, http.MethodGet, "/livez", nil); rec.Code != http.StatusOK {
		t.Fatalf("livez status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadinessWithoutExiftool(t *testing.T) {
	app := newApplication("", fakeStripper(), 1)
	if rec := serve(app, http.MethodGet, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestRequestID(t *testing.T) {
	app := newApplication("13.55", fakeStripper(), 1)
	rec := serve(app, http.MethodGet, "/livez", http.Header{"X-Request-Id": {"caller-id_1"}})
	if got := rec.Header().Get("X-Request-ID"); got != "caller-id_1" {
		t.Fatalf("X-Request-ID = %q, want caller-id_1", got)
	}
	rec = serve(app, http.MethodGet, "/livez", http.Header{"X-Request-Id": {"not valid!"}})
	if got := rec.Header().Get("X-Request-ID"); len(got) != 32 {
		t.Fatalf("X-Request-ID = %q, want a generated 32-character ID", got)
	}
}

func TestMetrics(t *testing.T) {
	app := newApplication("13.55", fakeStripper(), 1)
	serve(app, http.MethodGet, "/unknown", nil)
	rec := serve(app, http.MethodGet, "/metrics", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`exiftool_build_info{commit="unknown",exiftool_version="13.55",max_concurrent_strips="1",version="dev"} 1`,
		`exiftool_http_requests_total{method="GET",route="unmatched",status="404"} 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestDetectExiftoolVersion(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "exiftool")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 13.55\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := detectExiftoolVersion(context.Background(), fake)
	if err != nil {
		t.Fatal(err)
	}
	if got != "13.55" {
		t.Fatalf("version = %q, want 13.55", got)
	}
}

func TestDetectExiftoolVersionFailures(t *testing.T) {
	dir := t.TempDir()
	silent := filepath.Join(dir, "silent")
	failing := filepath.Join(dir, "failing")
	if err := os.WriteFile(silent, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "missing"), silent, failing} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, err := detectExiftoolVersion(context.Background(), path); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestPositiveEnvDuration(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 30 * time.Second, false},
		{"5s", 5 * time.Second, false},
		{"0s", 0, true},
		{"-1s", 0, true},
		{"soon", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("SHUTDOWN_TIMEOUT", tc.value)
			got, err := positiveEnvDuration("SHUTDOWN_TIMEOUT", 30*time.Second)
			if (err != nil) != tc.invalid || got != tc.want {
				t.Fatalf("positiveEnvDuration() = %v, %v; want %v, invalid=%v", got, err, tc.want, tc.invalid)
			}
		})
	}
}

const (
	testFile     = "IMAGE-DATA;GPS=37.7749"
	testStripped = "IMAGE-DATA"
)

type stripperFunc func(context.Context, string) error

func (f stripperFunc) Strip(ctx context.Context, path string) error {
	return f(ctx, path)
}

// fakeStripper keeps everything before the first ';', standing in for ExifTool.
func fakeStripper() stripperFunc {
	return func(_ context.Context, path string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if index := bytes.IndexByte(data, ';'); index >= 0 {
			data = data[:index]
		}
		return os.WriteFile(path, data, 0o600)
	}
}

type gatedStripper struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *gatedStripper) Strip(ctx context.Context, path string) error {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return fakeStripper()(ctx, path)
}

type countingReader struct {
	reader  io.Reader
	started *atomic.Int32
	once    sync.Once
}

func (r *countingReader) Read(data []byte) (int, error) {
	r.once.Do(func() { r.started.Add(1) })
	return r.reader.Read(data)
}

func newStripApplication(t *testing.T, stripper stripper, maxConcurrentStrips int) *application {
	t.Helper()
	app := newApplication("13.55", stripper, maxConcurrentStrips)
	app.tempDir = t.TempDir()
	return app
}

func stripRequest(body io.Reader) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/strip", body)
	req.Header.Set("Content-Type", "application/octet-stream")
	return req
}

func multipartRequest(t *testing.T, field string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(field, "photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/strip", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func serveRequest(app *application, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	app.telemetry.instrument(app.routes()).ServeHTTP(rec, req)
	return rec
}

func TestStrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func(t *testing.T) *http.Request
	}{
		{"raw body", func(*testing.T) *http.Request { return stripRequest(strings.NewReader(testFile)) }},
		{"multipart file", func(t *testing.T) *http.Request { return multipartRequest(t, "file", []byte(testFile)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveRequest(newStripApplication(t, fakeStripper(), 1), tc.req(t))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			if got := rec.Body.String(); got != testStripped {
				t.Fatalf("body = %q, want %q", got, testStripped)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
				t.Fatalf("Content-Type = %q, want application/octet-stream", got)
			}
			if got := rec.Header().Get("Content-Length"); got != "10" {
				t.Fatalf("Content-Length = %q, want 10", got)
			}
		})
	}
}

func TestStripRemovesTemporaryFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stripper stripper
	}{
		{"success", fakeStripper()},
		{"failure", stripperFunc(func(context.Context, string) error { return exiftool.ErrNotStripped })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newStripApplication(t, tc.stripper, 1)
			serveRequest(app, stripRequest(strings.NewReader(testFile)))

			entries, err := os.ReadDir(app.tempDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("temporary directory has %d entries, want 0", len(entries))
			}
		})
	}
}

func TestStripErrors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		method      string
		contentType string
		body        []byte
		stripper    stripper
		want        int
	}{
		{name: "method", method: http.MethodGet, want: http.StatusMethodNotAllowed},
		{name: "missing content type", method: http.MethodPost, body: []byte(testFile), want: http.StatusBadRequest},
		{name: "empty body", method: http.MethodPost, contentType: "application/octet-stream", want: http.StatusBadRequest},
		{name: "multipart without boundary", method: http.MethodPost, contentType: "multipart/form-data", body: []byte(testFile), want: http.StatusBadRequest},
		{name: "request too large", method: http.MethodPost, contentType: "application/octet-stream", body: make([]byte, defaultMaxRequestBytes+1), want: http.StatusRequestEntityTooLarge},
		{
			name: "not stripped", method: http.MethodPost, contentType: "application/octet-stream", body: []byte(testFile),
			stripper: stripperFunc(func(context.Context, string) error { return exiftool.ErrNotStripped }),
			want:     http.StatusUnprocessableEntity,
		},
		{
			name: "process failure", method: http.MethodPost, contentType: "application/octet-stream", body: []byte(testFile),
			stripper: stripperFunc(func(context.Context, string) error { return errors.New("exiftool exited") }),
			want:     http.StatusInternalServerError,
		},
		{
			name: "empty result", method: http.MethodPost, contentType: "application/octet-stream", body: []byte(testFile),
			stripper: stripperFunc(func(_ context.Context, path string) error { return os.WriteFile(path, nil, 0o600) }),
			want:     http.StatusInternalServerError,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stripper := tc.stripper
			if stripper == nil {
				stripper = fakeStripper()
			}
			req := httptest.NewRequest(tc.method, "/strip", bytes.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rec := serveRequest(newStripApplication(t, stripper, 1), req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.want, rec.Body.String())
			}
			var body errorResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil || body.Error == "" {
				t.Fatalf("error response = %+v, %v; want a message", body, err)
			}
		})
	}
}

func TestStripMultipartErrors(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		data        []byte
	}{
		{"missing file field", "image", []byte(testFile)},
		{"empty file field", "file", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveRequest(newStripApplication(t, fakeStripper(), 1), multipartRequest(t, tc.field, tc.data))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

func TestStripConfiguredRequestLimit(t *testing.T) {
	app := newStripApplication(t, fakeStripper(), 1)
	app.maxRequestBytes = int64(len(testFile))

	if rec := serveRequest(app, stripRequest(strings.NewReader(testFile))); rec.Code != http.StatusOK {
		t.Fatalf("status at the limit = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec := serveRequest(app, stripRequest(strings.NewReader(testFile+"!"))); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status above the limit = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestStripDeadlineCancelsStrip(t *testing.T) {
	cancelled := make(chan struct{})
	app := newStripApplication(t, stripperFunc(func(ctx context.Context, _ string) error {
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	}), 1)
	app.stripTimeout = 20 * time.Millisecond

	rec := serveRequest(app, stripRequest(strings.NewReader(testFile)))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusGatewayTimeout, rec.Body.String())
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("strip context was not cancelled")
	}
}

func TestStripClientCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	app := newStripApplication(t, stripperFunc(func(ctx context.Context, _ string) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}), 1)

	rec := serveRequest(app, stripRequest(strings.NewReader(testFile)).WithContext(ctx))

	if rec.Code != 499 {
		t.Fatalf("status = %d, want 499; body = %s", rec.Code, rec.Body.String())
	}
}

func TestStripBoundsConcurrentWork(t *testing.T) {
	var inFlight, maxInFlight atomic.Int32
	unblock := make(chan struct{})
	sawTwo := make(chan struct{})
	var sawTwoOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(unblock) }) }
	stripper := stripperFunc(func(ctx context.Context, path string) error {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			maximum := maxInFlight.Load()
			if n <= maximum || maxInFlight.CompareAndSwap(maximum, n) {
				break
			}
		}
		if n >= 2 {
			sawTwoOnce.Do(func() { close(sawTwo) })
		}
		select {
		case <-unblock:
		case <-time.After(3 * time.Second):
		}
		return fakeStripper()(ctx, path)
	})
	const concurrency = 2
	app := newStripApplication(t, stripper, concurrency)

	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			if rec := serveRequest(app, stripRequest(strings.NewReader(testFile))); rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
			}
		})
	}
	select {
	case <-sawTwo:
	case <-time.After(3 * time.Second):
		release()
		wait.Wait()
		t.Fatalf("maximum concurrent strips = %d, want %d", maxInFlight.Load(), concurrency)
	}
	release()
	wait.Wait()
	if got := maxInFlight.Load(); got != concurrency {
		t.Fatalf("maximum concurrent strips = %d, want %d", got, concurrency)
	}
}

func TestStripBoundsBufferedUploads(t *testing.T) {
	stripper := &gatedStripper{started: make(chan struct{}), release: make(chan struct{})}
	// The only strip slot is occupied, so this isolates upload admission.
	app := newStripApplication(t, stripper, 1)

	var wait sync.WaitGroup
	startRequest := func(body io.Reader) {
		wait.Go(func() {
			if rec := serveRequest(app, stripRequest(body)); rec.Code != http.StatusOK {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
			}
		})
	}

	startRequest(strings.NewReader(testFile))
	<-stripper.started // The strip slot is occupied, but its upload slot is free.

	var bodiesStarted atomic.Int32
	for range maxConcurrentUploads + 1 {
		startRequest(&countingReader{reader: strings.NewReader(testFile), started: &bodiesStarted})
	}
	deadline := time.After(time.Second)
	for bodiesStarted.Load() < maxConcurrentUploads {
		select {
		case <-deadline:
			t.Fatalf("only %d request bodies started", bodiesStarted.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	time.Sleep(20 * time.Millisecond)
	if got := bodiesStarted.Load(); got != maxConcurrentUploads {
		t.Fatalf("buffered request bodies = %d, want %d", got, maxConcurrentUploads)
	}

	close(stripper.release)
	wait.Wait()
}

func TestStripDuringShutdown(t *testing.T) {
	app := newStripApplication(t, fakeStripper(), 1)
	app.draining.Store(true)

	rec := serveRequest(app, stripRequest(strings.NewReader(testFile)))

	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("status = %d, Retry-After = %q; want %d, 1", rec.Code, rec.Header().Get("Retry-After"), http.StatusServiceUnavailable)
	}
}

func TestReadinessWithoutStripper(t *testing.T) {
	app := newApplication("13.55", nil, 1)
	if rec := serve(app, http.MethodGet, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestStripMetrics(t *testing.T) {
	app := newStripApplication(t, fakeStripper(), 1)
	serveRequest(app, stripRequest(strings.NewReader(testFile)))
	rec := serve(app, http.MethodGet, "/metrics", nil)
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`exiftool_strips_total{outcome="success"} 1`,
		`exiftool_http_requests_total{method="POST",route="/strip",status="200"} 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
}

func TestPositiveEnvInt(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    int
		invalid bool
	}{
		{"", 3, false},
		{"8", 8, false},
		{"0", 0, true},
		{"-1", 0, true},
		{"many", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("MAX_CONCURRENT_STRIPS", tc.value)
			got, err := positiveEnvInt("MAX_CONCURRENT_STRIPS", 3)
			if (err != nil) != tc.invalid || got != tc.want {
				t.Fatalf("positiveEnvInt() = %v, %v; want %v, invalid=%v", got, err, tc.want, tc.invalid)
			}
		})
	}
}

func TestParseByteSize(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    int64
		invalid bool
	}{
		{"1024", 1024, false},
		{"25MiB", 25 << 20, false},
		{"20MB", 20_000_000, false},
		{"1 KiB", 1 << 10, false},
		{"0", 0, true},
		{"-5MB", 0, true},
		{"lots", 0, true},
		{"9999999999GiB", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got, err := parseByteSize(tc.value)
			if (err != nil) != tc.invalid || got != tc.want {
				t.Fatalf("parseByteSize() = %v, %v; want %v, invalid=%v", got, err, tc.want, tc.invalid)
			}
		})
	}
}

func TestStripSlowUploadTimesOut(t *testing.T) {
	app := newStripApplication(t, fakeStripper(), 1)
	server := httptest.NewUnstartedServer(app.telemetry.instrument(app.routes()))
	// Shorter than the strip timeout, so the server's read deadline fires first.
	server.Config.ReadTimeout = 100 * time.Millisecond
	server.Start()
	t.Cleanup(server.Close)

	body, writer := io.Pipe()
	t.Cleanup(func() { writer.Close() })
	go func() {
		_, _ = writer.Write([]byte("IMAGE"))
	}()
	req, err := http.NewRequest(http.MethodPost, server.URL+"/strip", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusGatewayTimeout)
	}
}
