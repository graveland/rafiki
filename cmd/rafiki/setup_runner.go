// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
)

// runner is the one seam every setup step uses to reach a subprocess, so tests
// never exec anything. Production wires execRunner; tests wire fakeRunner
// (setup_prereq_test.go).
type runner interface {
	// Run executes argv with extra env appended to os.Environ(); stdout and
	// stderr are returned separately. A non-zero exit is an *exec.ExitError.
	Run(ctx context.Context, env []string, argv ...string) (stdout, stderr []byte, err error)
	// Stream starts argv and returns its stdout as a line reader; the process
	// is killed when ctx is done. Used for `docker events`.
	Stream(ctx context.Context, argv ...string) (io.ReadCloser, error)
	LookPath(name string) (string, error)
	// RunInteractive runs argv attached to this process's terminal
	// (stdin/stdout/stderr), for `claude auth login` and sudo prompts.
	RunInteractive(ctx context.Context, argv ...string) error
}

// execRunner is the production runner.
type execRunner struct{}

// execRunner must satisfy runner.
var _ runner = execRunner{}

func (execRunner) Run(ctx context.Context, env []string, argv ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func (execRunner) Stream(ctx context.Context, argv ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// A bare `docker events` on a TTY buffers; --format is passed by callers.
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return nil, err
	}
	go func() {
		_ = pw.CloseWithError(cmd.Wait())
	}()
	return pr, nil
}

func (execRunner) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (execRunner) RunInteractive(ctx context.Context, argv ...string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}
