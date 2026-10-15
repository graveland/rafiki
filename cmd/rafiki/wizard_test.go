// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// newTestWizard builds a non-interactive wizard via the production constructor
// newWizard over a fresh cobra command: the flag layer is the only one under
// test, so the wizard's stdin/stdout are replaced with harmless buffers and
// interactive is forced false (a test binary's stdin is not a terminal anyway).
func newTestWizard(t *testing.T, flags func(*cobra.Command)) *wizard {
	t.Helper()
	cmd := &cobra.Command{Use: "setup", RunE: func(*cobra.Command, []string) error { return nil }}
	cmd.Flags().Bool("yes", false, "")
	flags(cmd)
	w := newWizard(cmd)
	w.in = &bytes.Buffer{}
	w.out = &bytes.Buffer{}
	w.interactive = false
	return w
}

func TestWizardPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		setFlag  bool
		flagVal  string
		saved    string
		detected string
		want     string
	}{
		{name: "flag wins over saved and detected", setFlag: true, flagVal: "from-flag", saved: "saved", detected: "detected", want: "from-flag"},
		{name: "saved wins over detected", saved: "saved", detected: "detected", want: "saved"},
		{name: "detected when nothing else", detected: "detected", want: "detected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newTestWizard(t, func(cmd *cobra.Command) {
				cmd.Flags().String("name", "", "")
				if tt.setFlag {
					if err := cmd.Flags().Set("name", tt.flagVal); err != nil {
						t.Fatal(err)
					}
				}
			})
			got, err := w.askString("Name", "name", tt.saved, tt.detected, false)
			if err != nil {
				t.Fatalf("askString: %v", err)
			}
			if got != tt.want {
				t.Errorf("askString = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWizardRequiredMissingUnderYes(t *testing.T) {
	w := newTestWizard(t, func(cmd *cobra.Command) {
		cmd.Flags().String("name", "", "")
	})
	w.yes = true

	_, err := w.askString("Name", "name", "", "", true)
	if err == nil {
		t.Fatal("askString with a required empty value under --yes: want error, got nil")
	}
	if !strings.Contains(err.Error(), "--name") {
		t.Errorf("error %q must name the flag --name", err.Error())
	}
}

func TestWizardConfirmNilSavedFallsToDetected(t *testing.T) {
	w := newTestWizard(t, func(cmd *cobra.Command) {
		cmd.Flags().Bool("force", false, "")
	})

	got, err := w.askConfirm("Force?", "force", nil, true)
	if err != nil {
		t.Fatalf("askConfirm(nil saved): %v", err)
	}
	if !got {
		t.Error("nil saved + detected=true: want true, got false")
	}

	no := false
	got, err = w.askConfirm("Force?", "force", &no, true)
	if err != nil {
		t.Fatalf("askConfirm(saved false): %v", err)
	}
	if got {
		t.Error("saved=false must beat detected=true: want false, got true")
	}
}

func TestWizardMultiPrecedenceAndOrder(t *testing.T) {
	w := newTestWizard(t, func(cmd *cobra.Command) {
		cmd.Flags().StringSlice("tools", nil, "")
	})
	options := []string{"bash", "edit", "read"}

	// saved preselection returns in options' order, not saved's order.
	got, err := w.askMulti("Tools", "tools", options, []string{"read", "bash"}, nil)
	if err != nil {
		t.Fatalf("askMulti: %v", err)
	}
	if strings.Join(got, ",") != "bash,read" {
		t.Errorf("askMulti = %v, want [bash read] in options order", got)
	}

	// flag beats saved.
	if err := w.cmd.Flags().Set("tools", "edit"); err != nil {
		t.Fatal(err)
	}
	got, err = w.askMulti("Tools", "tools", options, []string{"read", "bash"}, nil)
	if err != nil {
		t.Fatalf("askMulti: %v", err)
	}
	if strings.Join(got, ",") != "edit" {
		t.Errorf("askMulti with flag set = %v, want [edit]", got)
	}
}

// TestWizardConfirmRun pins confirmRun's non-interactive contract: it prints
// the joined argv line and, with no terminal, agrees only when --yes was given.
func TestWizardConfirmRun(t *testing.T) {
	for _, yes := range []bool{false, true} {
		t.Run(map[bool]string{false: "without --yes", true: "with --yes"}[yes], func(t *testing.T) {
			cmd := &cobra.Command{Use: "setup", RunE: func(*cobra.Command, []string) error { return nil }}
			cmd.Flags().Bool("yes", false, "")
			if yes {
				if err := cmd.Flags().Set("yes", "true"); err != nil {
					t.Fatal(err)
				}
			}
			w := newWizard(cmd)
			var out bytes.Buffer
			w.in = &bytes.Buffer{}
			w.out = &out
			w.interactive = false

			got, err := w.confirmRun([]string{"brew", "install", "x"})
			if err != nil {
				t.Fatalf("confirmRun: %v", err)
			}
			if got != yes {
				t.Errorf("confirmRun (--yes=%v) = %v, want %v", yes, got, yes)
			}
			if !strings.Contains(out.String(), "brew install x") {
				t.Errorf("confirmRun did not print the command line; out = %q", out.String())
			}
		})
	}
}

func TestWaitForSocketAlreadyThere(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ws")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := waitForSocket(ctx, path); err != nil {
		t.Fatalf("waitForSocket on a listening socket: %v", err)
	}
}

func TestWaitForSocketCreatedLater(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ws")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "s.sock")
	lnCh := make(chan net.Listener, 1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		ln, err := net.Listen("unix", path)
		if err != nil {
			t.Errorf("listen: %v", err)
			lnCh <- nil
			return
		}
		lnCh <- ln
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := waitForSocket(ctx, path); err != nil {
		t.Fatalf("waitForSocket on a socket created after the watch: %v", err)
	}
	if ln := <-lnCh; ln != nil {
		ln.Close()
	}
}

func TestWaitForSocketContextDeadline(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "ws")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "never.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err = waitForSocket(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForSocket with nothing listening = %v, want context.DeadlineExceeded", err)
	}
}
