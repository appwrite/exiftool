//go:build integration

package exiftool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Tags written to every fixture. ExifTool must remove all of them.
var privateTags = []string{
	"-GPSLatitude=37.7749", "-GPSLatitudeRef=N",
	"-GPSLongitude=122.4194", "-GPSLongitudeRef=W",
	"-Make=Canon", "-Model=EOS 5D",
	"-DateTimeOriginal=2026:01:01 10:00:00",
	"-XMP-dc:Creator=Jane Doe",
}

func exiftoolPath(t *testing.T) string {
	t.Helper()
	path := os.Getenv("EXIFTOOL_PATH")
	if path == "" {
		t.Fatal("EXIFTOOL_PATH must point to an ExifTool executable")
	}
	return path
}

func newPool(t *testing.T, size int) *Pool {
	t.Helper()
	pool, err := New(context.Background(), Options{Binary: exiftoolPath(t), Size: size, StartupTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func writeFixture(t *testing.T, name string, data []byte, tags ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(tags) > 0 {
		args := append(append([]string{}, tags...), "-overwrite_original", path)
		if output, err := exec.Command(exiftoolPath(t), args...).CombinedOutput(); err != nil {
			t.Fatalf("tag fixture: %v: %s", err, output)
		}
	}
	return path
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for x := range 8 {
		for y := range 6 {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 40), B: 200, A: 255})
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func readTags(t *testing.T, path string) map[string]any {
	t.Helper()
	output, err := exec.Command(exiftoolPath(t), "-j", "-G0", "-n", path).Output()
	if err != nil {
		t.Fatalf("read tags: %v", err)
	}
	var tags []map[string]any
	if err := json.Unmarshal(output, &tags); err != nil || len(tags) != 1 {
		t.Fatalf("decode tags: %v", err)
	}
	return tags[0]
}

func assertNoPrivateTags(t *testing.T, tags map[string]any) {
	t.Helper()
	for _, key := range []string{
		"EXIF:GPSLatitude", "EXIF:GPSLongitude", "Composite:GPSLatitude",
		"EXIF:Make", "EXIF:Model", "EXIF:DateTimeOriginal", "XMP:Creator",
	} {
		if value, ok := tags[key]; ok {
			t.Errorf("%s = %v was not removed", key, value)
		}
	}
}

func TestStripRemovesMetadata(t *testing.T) {
	pool := newPool(t, 1)
	for _, tt := range []struct {
		name string
		data []byte
	}{
		{"jpeg", warmUpJPEG},
		{"png", testPNG(t)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// No extension: the format must come from the content.
			path := writeFixture(t, "upload", tt.data, privateTags...)
			if _, ok := readTags(t, path)["EXIF:Make"]; !ok {
				t.Fatal("fixture was not tagged")
			}

			if err := pool.Strip(context.Background(), path); err != nil {
				t.Fatal(err)
			}

			assertNoPrivateTags(t, readTags(t, path))
		})
	}
}

func TestStripKeepsOrientationAndColorProfile(t *testing.T) {
	pool := newPool(t, 1)
	path := writeFixture(t, "photo.jpg", warmUpJPEG, append([]string{"-Orientation#=6"}, privateTags...)...)
	profile := writeICCProfile(t)
	if output, err := exec.Command(exiftoolPath(t), "-icc_profile<="+profile, "-overwrite_original", path).CombinedOutput(); err != nil {
		t.Fatalf("add profile: %v: %s", err, output)
	}
	want := readProfile(t, path)
	if len(want) == 0 {
		t.Fatal("fixture has no ICC profile")
	}

	if err := pool.Strip(context.Background(), path); err != nil {
		t.Fatal(err)
	}

	tags := readTags(t, path)
	assertNoPrivateTags(t, tags)
	if got := tags["EXIF:Orientation"]; got != float64(6) {
		t.Errorf("EXIF:Orientation = %v, want 6", got)
	}
	if got := readProfile(t, path); !bytes.Equal(got, want) {
		t.Errorf("ICC profile = %d bytes after strip, want the original %d bytes", len(got), len(want))
	}
}

func readProfile(t *testing.T, path string) []byte {
	t.Helper()
	output, err := exec.Command(exiftoolPath(t), "-b", "-icc_profile", path).Output()
	if err != nil {
		t.Fatalf("read profile: %v", err)
	}
	return output
}

// writeICCProfile writes the smallest ICC header ExifTool accepts as a profile.
func writeICCProfile(t *testing.T) string {
	t.Helper()
	header := make([]byte, 132)
	size := len(header)
	header[0], header[1], header[2], header[3] = byte(size>>24), byte(size>>16), byte(size>>8), byte(size)
	copy(header[4:], "none")
	header[8] = 4 // Version 4.0.
	copy(header[12:], "mntr")
	copy(header[16:], "RGB ")
	copy(header[20:], "XYZ ")
	copy(header[36:], "acsp")
	path := filepath.Join(t.TempDir(), "profile.icc")
	if err := os.WriteFile(path, header, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStripLeavesUnsupportedFileUnchanged(t *testing.T) {
	pool := newPool(t, 1)
	data := []byte("GPSLatitude 37.7749 is just text here")
	path := writeFixture(t, "notes", data)

	err := pool.Strip(context.Background(), path)

	if !errors.Is(err, ErrNotStripped) {
		t.Fatalf("Strip() error = %v, want ErrNotStripped", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("unsupported file was modified")
	}
}

func TestStripRejectsUnsafePaths(t *testing.T) {
	pool := newPool(t, 1)
	for _, path := range []string{"relative.jpg", " /tmp/photo.jpg", "/tmp/line\nbreak.jpg", "/tmp/line\rbreak.jpg"} {
		if err := pool.Strip(context.Background(), path); err == nil {
			t.Errorf("Strip(%q) succeeded, want an error", path)
		}
	}
}

func TestStripRecoversAfterTimeout(t *testing.T) {
	pool := newPool(t, 1)
	path := writeFixture(t, "photo.jpg", warmUpJPEG, privateTags...)
	expired, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-expired.Done()

	// An expired context fails before a process is used.
	if err := pool.Strip(expired, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Strip() error = %v, want deadline exceeded", err)
	}

	// Cancelling mid-command kills the process; the slot must be replaced on next use.
	slow, cancelSlow := context.WithCancel(context.Background())
	go func() {
		time.Sleep(time.Millisecond)
		cancelSlow()
	}()
	_ = pool.Strip(slow, path)

	if err := pool.Strip(context.Background(), path); err != nil {
		t.Fatalf("Strip() after cancellation: %v", err)
	}
	assertNoPrivateTags(t, readTags(t, path))
}

func TestStripReplacesExitedProcess(t *testing.T) {
	pool := newPool(t, 1)
	proc := <-pool.processes
	proc.kill()
	pool.processes <- proc
	path := writeFixture(t, "photo.jpg", warmUpJPEG, privateTags...)

	// The dead process fails this request and is discarded.
	if err := pool.Strip(context.Background(), path); err == nil {
		t.Fatal("Strip() with an exited process succeeded")
	}

	if err := pool.Strip(context.Background(), path); err != nil {
		t.Fatalf("Strip() after replacement: %v", err)
	}
	assertNoPrivateTags(t, readTags(t, path))
}

func TestStripConcurrentRequests(t *testing.T) {
	pool := newPool(t, 2)
	var wait sync.WaitGroup
	for i := range 12 {
		path := writeFixture(t, "photo", warmUpJPEG, privateTags...)
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := pool.Strip(context.Background(), path); err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			assertNoPrivateTags(t, readTags(t, path))
		}()
	}
	wait.Wait()
}

func TestStripAfterClose(t *testing.T) {
	pool := newPool(t, 1)
	pool.Close()
	path := writeFixture(t, "photo.jpg", warmUpJPEG)

	if err := pool.Strip(context.Background(), path); !errors.Is(err, errClosed) {
		t.Fatalf("Strip() error = %v, want errClosed", err)
	}
}

func TestNewFailsWithoutExifTool(t *testing.T) {
	_, err := New(context.Background(), Options{Binary: "/nonexistent/exiftool", Size: 2, StartupTimeout: time.Second})
	if err == nil {
		t.Fatal("New() succeeded with a missing binary")
	}
}

func TestNewStopsProcessesWhenWarmUpTimesOut(t *testing.T) {
	before := countProcesses(t)

	_, err := New(context.Background(), Options{Binary: exiftoolPath(t), Size: 2, StartupTimeout: time.Microsecond})

	if err == nil {
		t.Fatal("New() succeeded with an impossible startup timeout")
	}
	if after := countProcesses(t); after != before {
		t.Fatalf("exiftool processes = %d after failed startup, want %d", after, before)
	}
}

func countProcesses(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	count := 0
	for _, entry := range entries {
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err == nil && bytes.Contains(cmdline, []byte("exiftool")) && bytes.Contains(cmdline, []byte("-stay_open")) {
			count++
		}
	}
	return count
}

func TestCloseWakesWaitingStrip(t *testing.T) {
	pool := newPool(t, 1)
	path := writeFixture(t, "photo.jpg", warmUpJPEG)
	proc := <-pool.processes // Hold the only process so the next Strip waits.

	result := make(chan error, 1)
	go func() { result <- pool.Strip(context.Background(), path) }()
	time.Sleep(50 * time.Millisecond)
	go pool.Close()

	select {
	case err := <-result:
		if !errors.Is(err, errClosed) {
			t.Fatalf("Strip() error = %v, want errClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Strip() kept waiting after Close")
	}
	pool.processes <- proc
}
