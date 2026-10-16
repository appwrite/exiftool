package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	app := newApplication("13.55")
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
	app := newApplication("13.55")
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
	app := newApplication("13.55")
	app.draining.Store(true)
	if rec := serve(app, http.MethodGet, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if rec := serve(app, http.MethodGet, "/livez", nil); rec.Code != http.StatusOK {
		t.Fatalf("livez status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadinessWithoutExiftool(t *testing.T) {
	app := newApplication("")
	if rec := serve(app, http.MethodGet, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

func TestRequestID(t *testing.T) {
	app := newApplication("13.55")
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
	app := newApplication("13.55")
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
		`exiftool_build_info{commit="unknown",exiftool_version="13.55",version="dev"} 1`,
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
