//go:build integration

package main

import (
	"context"
	"os"
	"testing"
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
