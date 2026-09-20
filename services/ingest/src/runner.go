package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// RunResult carries what a finished command left behind. Stderr matters as much
// as the error: ffmpeg's last few lines are what you actually need to debug a
// failed encode, and they end up in the .failed sidecar.
type RunResult struct {
	Stdout string
	Stderr string
}

// Runner exists so the transcode path can be tested without ffmpeg installed.
type Runner interface {
	Run(ctx context.Context, name string, args []string) (RunResult, error)
}

type execRunner struct {
	timeout time.Duration
}

func NewExecRunner(timeout time.Duration) Runner { return &execRunner{timeout: timeout} }

func (r *execRunner) Run(ctx context.Context, name string, args []string) (RunResult, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	// Interrupt rather than kill: ffmpeg flushes and closes the container on
	// SIGINT. WaitDelay is a short backstop for a process that ignores it —
	// short because the output is a temp file we discard on cancellation
	// anyway, so there is nothing worth waiting to flush.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 3 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := RunResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		return res, fmt.Errorf("%s: %w: %s", name, err, tail(res.Stderr, 400))
	}
	return res, nil
}

// tail returns the last n characters of s, collapsed to one line.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no stderr)"
	}
	if len(s) > n {
		s = "..." + s[len(s)-n:]
	}
	return strings.Join(strings.Fields(s), " ")
}
