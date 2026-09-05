package treadmark

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
)

// runner wraps the communicator with timeout-aware exec/upload/download
// helpers.
type runner struct {
	comm packersdk.Communicator
	ui   packersdk.Ui
}

// run executes cmdline in the guest, streaming output to the UI, and returns
// the exit code.
func (r *runner) run(ctx context.Context, cmdline string, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := &packersdk.RemoteCmd{Command: cmdline}
	if err := cmd.RunWithUi(ctx, r.comm, r.ui); err != nil {
		return -1, fmt.Errorf("running %q: %w", cmdline, err)
	}
	return cmd.ExitStatus(), nil
}

// captureQuiet executes cmdline buffering stdout and stderr for the caller;
// nothing reaches the UI. Probes whose failure is an expected outcome (uname
// on a Windows guest) use this so the build log stays clean — the caller
// decides whether the buffered stderr is worth reporting.
func (r *runner) captureQuiet(ctx context.Context, cmdline string, timeout time.Duration) (stdout, stderr string, code int, err error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := &packersdk.RemoteCmd{Command: cmdline, Stdout: &out, Stderr: &errb}
	if err := r.comm.Start(ctx, cmd); err != nil {
		return "", "", -1, fmt.Errorf("starting %q: %w", cmdline, err)
	}
	done := make(chan int, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case code := <-done:
		return out.String(), errb.String(), code, nil
	case <-ctx.Done():
		return "", "", -1, fmt.Errorf("command %q timed out", cmdline)
	}
}

// capture executes cmdline buffering stdout for the caller; stderr from a
// completed command is surfaced to the UI.
func (r *runner) capture(ctx context.Context, cmdline string, timeout time.Duration) (string, int, error) {
	out, stderr, code, err := r.captureQuiet(ctx, cmdline, timeout)
	if err == nil {
		if s := strings.TrimSpace(stderr); s != "" {
			r.ui.Message(s)
		}
	}
	return out, code, err
}

func (r *runner) uploadFile(local, remote string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if err := r.comm.Upload(remote, f, &fi); err != nil {
		return fmt.Errorf("uploading %s to %s: %w", local, remote, err)
	}
	return nil
}

func (r *runner) downloadFile(remote, local string) error {
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return err
	}
	f, err := os.Create(local)
	if err != nil {
		return err
	}
	if err := r.comm.Download(remote, f); err != nil {
		f.Close()
		os.Remove(local)
		return fmt.Errorf("downloading %s: %w", remote, err)
	}
	return f.Close()
}
