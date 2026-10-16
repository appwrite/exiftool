// Package exiftool removes metadata from files with ExifTool processes kept
// running in -stay_open mode, so each request skips Perl's startup.
package exiftool

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotStripped means ExifTool ran but could not rewrite the file, for
// example because the format is not writable or the file is corrupt.
var ErrNotStripped = errors.New("exiftool could not rewrite the file")

var errClosed = errors.New("exiftool pool is closed")

// A 1x1 JPEG stripped once per process at startup, so ExifTool loads its
// writer modules before the first real request.
//
//go:embed warmup.jpg
var warmUpJPEG []byte

const closeTimeout = 5 * time.Second

// Options controls the processes started by the pool.
type Options struct {
	// Binary is the ExifTool executable. Defaults to "exiftool" on PATH.
	Binary string
	// Size is the number of ExifTool processes, and so the number of files
	// stripped concurrently.
	Size int
	// StartupTimeout bounds starting a process and stripping the warm-up file.
	StartupTimeout time.Duration
}

// Pool owns a fixed number of ExifTool processes. A process that fails or
// times out is killed and replaced on its next use.
type Pool struct {
	binary         string
	size           int
	startupTimeout time.Duration
	// Holds exactly size entries while idle. A nil entry is a slot whose
	// process must be started before use.
	processes chan *process
	closed    atomic.Bool
	closeOnce sync.Once
}

// New starts and warms up every process. It fails if any process fails, so a
// misconfigured service never reports ready.
func New(ctx context.Context, options Options) (*Pool, error) {
	if options.Binary == "" {
		options.Binary = "exiftool"
	}
	if options.Size < 1 {
		return nil, errors.New("pool size must be at least one")
	}
	if options.StartupTimeout <= 0 {
		return nil, errors.New("startup timeout must be positive")
	}

	pool := &Pool{
		binary:         options.Binary,
		size:           options.Size,
		startupTimeout: options.StartupTimeout,
		processes:      make(chan *process, options.Size),
	}

	results := make(chan error, options.Size)
	for range options.Size {
		go func() {
			proc, err := pool.start(ctx)
			pool.processes <- proc
			results <- err
		}()
	}
	// Processes usually fail for the same reason, so one error is enough.
	var startErr error
	for range options.Size {
		if err := <-results; err != nil && startErr == nil {
			startErr = err
		}
	}
	if startErr != nil {
		pool.Close()
		return nil, startErr
	}
	return pool, nil
}

// Strip removes all metadata from the file at path, keeping the ICC color
// profile and orientation. It returns ErrNotStripped when ExifTool cannot
// rewrite the file; the file is left unchanged in that case.
func (p *Pool) Strip(ctx context.Context, path string) error {
	if ctx == nil {
		return errors.New("strip context is required")
	}
	// Each argument is one line of ExifTool's argument file.
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n") {
		return errors.New("path must be absolute and must not contain line breaks")
	}
	if p.closed.Load() {
		return errClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	var proc *process
	select {
	case proc = <-p.processes:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.closed.Load() {
		p.processes <- proc
		return errClosed
	}

	if proc == nil {
		var err error
		if proc, err = p.start(ctx); err != nil {
			p.processes <- nil
			return err
		}
	}

	output, err := proc.execute(ctx, stripArguments(path))
	if err != nil {
		// The process may still be writing this file or holding its output, so it
		// can't serve another request.
		proc.kill()
		p.processes <- nil
		return err
	}
	p.processes <- proc

	if !stripped(output) {
		return ErrNotStripped
	}
	return nil
}

// Close stops every process, waiting for strips in progress to finish.
func (p *Pool) Close() {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		for range p.size {
			if proc := <-p.processes; proc != nil {
				proc.close()
			}
		}
	})
}

func (p *Pool) start(ctx context.Context) (*process, error) {
	ctx, cancel := context.WithTimeout(ctx, p.startupTimeout)
	defer cancel()

	proc, err := startProcess(p.binary)
	if err != nil {
		return nil, err
	}
	if err := proc.warmUp(ctx); err != nil {
		proc.kill()
		return nil, fmt.Errorf("warm up exiftool: %w", err)
	}
	return proc, nil
}

func stripArguments(path string) []string {
	return []string{
		"-all=",
		"-tagsfromfile", "@",
		"-icc_profile",
		"-orientation",
		"-overwrite_original",
		path,
	}
}

func stripped(output string) bool {
	return strings.Contains(output, "1 image files updated") ||
		strings.Contains(output, "1 image files unchanged")
}

type process struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *bufio.Reader
	sequence int
	waitOnce sync.Once
}

func startProcess(binary string) (*process, error) {
	cmd := exec.Command(binary, "-stay_open", "True", "-@", "-")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("start exiftool: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("start exiftool: %w", err)
	}
	// Stderr is discarded: failures are detected from the summary on stdout.
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start exiftool: %w", err)
	}
	return &process{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

func (p *process) warmUp(ctx context.Context) error {
	file, err := os.CreateTemp("", "exiftool-warmup-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(warmUpJPEG)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}

	output, err := p.execute(ctx, stripArguments(file.Name()))
	if err != nil {
		return err
	}
	if !stripped(output) {
		return errors.New("exiftool could not strip the warm-up image")
	}
	return nil
}

// execute runs one command and returns its output. A numbered -execute makes
// ExifTool end the output with a matching {readyN} marker.
func (p *process) execute(ctx context.Context, args []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.sequence++
	marker := "{ready" + strconv.Itoa(p.sequence) + "}"
	command := strings.Join(args, "\n") + "\n-execute" + strconv.Itoa(p.sequence) + "\n"

	type result struct {
		output string
		err    error
	}
	done := make(chan result, 1)
	go func() {
		if _, err := io.WriteString(p.stdin, command); err != nil {
			done <- result{err: fmt.Errorf("send command to exiftool: %w", err)}
			return
		}
		var output strings.Builder
		for {
			line, err := p.stdout.ReadString('\n')
			if strings.TrimRight(line, "\r\n") == marker {
				done <- result{output: output.String()}
				return
			}
			output.WriteString(line)
			if err != nil {
				done <- result{err: fmt.Errorf("read exiftool output: %w", err)}
				return
			}
		}
	}()

	select {
	case r := <-done:
		return r.output, r.err
	case <-ctx.Done():
		// Killing the process closes its pipes, which unblocks the goroutine.
		p.kill()
		<-done
		return "", ctx.Err()
	}
}

func (p *process) kill() {
	_ = p.cmd.Process.Kill()
	p.wait()
}

func (p *process) close() {
	_, _ = io.WriteString(p.stdin, "-stay_open\nFalse\n")
	_ = p.stdin.Close()
	exited := make(chan struct{})
	go func() {
		p.wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(closeTimeout):
		p.kill()
	}
}

func (p *process) wait() {
	p.waitOnce.Do(func() { _ = p.cmd.Wait() })
}
