package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestHTTPMetricsBoundMethodLabelCardinality(t *testing.T) {
	app := newApplication("13.55", fakeStripper(), 1)
	const distinct = 64
	for i := range distinct {
		serve(app, fmt.Sprintf("BADMETHOD%d", i), "/livez", nil)
	}
	serve(app, http.MethodGet, "/livez", nil)
	serve(app, http.MethodPost, "/livez", nil)

	// Standard HTTP methods plus one bucket for everything else.
	const maxMethodLabels = 10
	for _, metric := range []string{
		"exiftool_http_requests_total",
		"exiftool_http_request_duration_seconds",
	} {
		methods := metricLabelValues(t, app.telemetry, metric, "method")
		if _, ok := methods["other"]; !ok {
			t.Errorf("%s missing method=%q for non-standard methods; got %v", metric, "other", sortedKeys(methods))
		}
		if _, ok := methods[http.MethodGet]; !ok {
			t.Errorf("%s missing method=%q; got %v", metric, http.MethodGet, sortedKeys(methods))
		}
		if _, ok := methods[http.MethodPost]; !ok {
			t.Errorf("%s missing method=%q; got %v", metric, http.MethodPost, sortedKeys(methods))
		}
		for i := range distinct {
			raw := fmt.Sprintf("BADMETHOD%d", i)
			if _, ok := methods[raw]; ok {
				t.Errorf("%s recorded unbounded method label %q", metric, raw)
			}
		}
		if len(methods) > maxMethodLabels {
			t.Errorf("%s method cardinality = %d (%v), want <= %d",
				metric, len(methods), sortedKeys(methods), maxMethodLabels)
		}
	}
}

func metricLabelValues(t *testing.T, tel *telemetry, metricName, label string) map[string]struct{} {
	t.Helper()
	families, err := tel.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]struct{})
	for _, family := range families {
		if family.GetName() != metricName {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, pair := range metric.GetLabel() {
				if pair.GetName() == label {
					values[pair.GetValue()] = struct{}{}
				}
			}
		}
	}
	return values
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func TestTelemetryMethod(t *testing.T) {
	for _, method := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace,
	} {
		if got := telemetryMethod(method); got != method {
			t.Errorf("telemetryMethod(%q) = %q, want %q", method, got, method)
		}
	}
	for _, method := range []string{"", "get", "BADMETHOD0", "GET "} {
		if got := telemetryMethod(method); got != "other" {
			t.Errorf("telemetryMethod(%q) = %q, want other", method, got)
		}
	}
}

func TestLoggedMethod(t *testing.T) {
	if got := loggedMethod(http.MethodGet); got != http.MethodGet {
		t.Fatalf("loggedMethod(GET) = %q, want GET", got)
	}
	long := strings.Repeat("A", maxLoggedMethodLen+8)
	got := loggedMethod(long)
	if got == long || len(got) > maxLoggedMethodLen+3 {
		t.Fatalf("loggedMethod did not bound a long method: len=%d", len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("A", maxLoggedMethodLen)) || !strings.HasSuffix(got, "...") {
		t.Fatalf("loggedMethod(%q) = %q, want a truncated prefix and ...", long, got)
	}
}
