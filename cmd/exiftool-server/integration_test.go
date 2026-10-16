//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"exiftool/internal/exiftool"
)

func TestDetectExiftoolVersionWithRealExiftool(t *testing.T) {
	path := os.Getenv("EXIFTOOL_PATH")
	if path == "" {
		t.Fatal("EXIFTOOL_PATH must point to an ExifTool executable")
	}
	got, err := detectExiftoolVersion(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if want := os.Getenv("EXIFTOOL_VERSION"); want != "" && got != want {
		t.Fatalf("version = %q, want %q", got, want)
	}
}

func TestStripWithRealExiftool(t *testing.T) {
	path := os.Getenv("EXIFTOOL_PATH")
	if path == "" {
		t.Fatal("EXIFTOOL_PATH must point to an ExifTool executable")
	}
	pool, err := exiftool.New(context.Background(), exiftool.Options{Binary: path, Size: 2, StartupTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	app := newStripApplication(t, pool, 2)
	original := taggedJPEG(t, path)

	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{"raw body", stripRequest(bytes.NewReader(original))},
		{"multipart file", multipartRequest(t, "file", original)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveRequest(app, tc.req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			stripped := rec.Body.Bytes()
			if len(stripped) >= len(original) {
				t.Fatalf("stripped size = %d, want less than %d", len(stripped), len(original))
			}
			tags := readTags(t, path, stripped)
			for _, tag := range []string{"GPSLatitude", "Make"} {
				if value, ok := tags[tag]; ok {
					t.Errorf("%s = %v was not removed", tag, value)
				}
			}
			if got := tags["Orientation"]; got != float64(6) {
				t.Errorf("Orientation = %v, want 6", got)
			}
		})
	}

	t.Run("unsupported file", func(t *testing.T) {
		rec := serveRequest(app, stripRequest(bytes.NewReader([]byte("plain text, not an image"))))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
		}
	})
}

// taggedJPEG returns a JPEG carrying a location, camera, and orientation.
func taggedJPEG(t *testing.T, exiftoolPath string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 12))
	for x := range 16 {
		for y := range 12 {
			img.Set(x, y, color.RGBA{R: uint8(x * 15), G: uint8(y * 20), B: 120, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "photo.jpg")
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(exiftoolPath, "-GPSLatitude=37.7749", "-GPSLatitudeRef=N",
		"-Make=Canon", "-Orientation#=6", "-overwrite_original", path).CombinedOutput()
	if err != nil {
		t.Fatalf("tag fixture: %v: %s", err, output)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readTags(t *testing.T, exiftoolPath string, data []byte) map[string]any {
	t.Helper()
	path := filepath.Join(t.TempDir(), "result")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(exiftoolPath, "-j", "-n", path).Output()
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	var tags []map[string]any
	if err := json.Unmarshal(output, &tags); err != nil || len(tags) != 1 {
		t.Fatalf("decode tags: %v", err)
	}
	return tags[0]
}
