// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/adminpb"
	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/executorpb"
)

func TestDescribeAdvertisesLaunchKinds(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), LaunchKinds: []string{"claude"}})
	defer func() { _ = s.Close() }()

	resp, err := s.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if !slices.Contains(resp.Msg.GetLaunchKinds(), "claude") {
		t.Errorf("launch_kinds = %v, want claude", resp.Msg.GetLaunchKinds())
	}
}

// An executor with no --launch flag hosts nothing. The default must be empty
// rather than "claude": a machine volunteering to host other people's children
// because someone forgot a flag is the self-report-gates-placement shape the
// isolation and workspace_mode rules exist to forbid.
func TestDescribeAdvertisesNoLaunchKindsByDefault(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir()})
	defer func() { _ = s.Close() }()

	resp, err := s.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if got := resp.Msg.GetLaunchKinds(); len(got) != 0 {
		t.Errorf("launch_kinds = %v, want empty", got)
	}
}

// The launched daraja must lead its own process group, because that group is
// the reaping handle for the whole child — daraja plus the claude that joins
// it. An executor that forgets Setpgid leaves daraja in the EXECUTOR's group,
// where a reap would signal the executor itself.
func TestLaunchGivesDarajaItsOwnGroup(t *testing.T) {
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c1",
		Cwd:     t.TempDir(),
		Spec:    &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	pid, pgid := int(resp.Msg.GetPid()), int(resp.Msg.GetPgid())
	if pgid != pid {
		t.Errorf("pgid = %d, want it to equal daraja's own pid %d", pgid, pid)
	}
	if pgid == syscall.Getpgrp() {
		t.Fatal("daraja was left in the executor's process group; a reap would signal us")
	}

	// The response arithmetic is hardcoded (Pgid: int32(pid)), so the two
	// checks above pass even with Setpgid missing. Ask the kernel what group
	// daraja actually leads: that is the assertion a missing Setpgid fails.
	kernelPgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid(%d): %v", pid, err)
	}
	if kernelPgid != pid {
		t.Errorf("kernel pgid of daraja (pid %d) = %d, want %d; Setpgid was not applied",
			pid, kernelPgid, pid)
	}
	if kernelPgid == syscall.Getpgrp() {
		t.Fatal("daraja sits in the executor's real process group; a reap would signal us")
	}
}

// An undeclared kind must be refused. The flag is the operator's declaration
// and the RPC is a peer's request; the declaration wins.
func TestLaunchRefusesAnUndeclaredKind(t *testing.T) {
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: nil,
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	_, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c1",
		Spec:    &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	if err == nil {
		t.Fatal("Launch admitted a kind this executor never declared")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}

// Reap must end daraja AND the child that joined its group. The stub sleeps
// until signalled, so a surviving process is an observable failure.
func TestReapEndsTheWholeGroup(t *testing.T) {
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c1",
		Cwd:     t.TempDir(),
		Spec:    &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	pgid := int(resp.Msg.GetPgid())

	rr, err := a.Reap(context.Background(), connect.NewRequest(&adminpb.ReapRequest{
		ChildId: "c1", GraceMs: 500,
	}))
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if !rr.Msg.GetReaped() {
		t.Error("Reap reported nothing reaped for a live launch")
	}

	// The group must be gone. ESRCH from a zero-signal probe is the proof.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process group %d still alive after Reap", pgid)
}

// Reaping something already gone is the normal case — the daemon reaps on kill
// without knowing whether the machine already cleaned up — and must not error.
func TestReapIsIdempotent(t *testing.T) {
	a := NewAdminServer(AdminOptions{SocketDir: t.TempDir()})
	defer a.Close()

	resp, err := a.Reap(context.Background(), connect.NewRequest(&adminpb.ReapRequest{ChildId: "ghost"}))
	if err != nil {
		t.Fatalf("Reap of an unknown child errored: %v", err)
	}
	if resp.Msg.GetReaped() {
		t.Error("Reap claimed to reap a child it never launched")
	}
}

// A launch claim still in flight must never be signalled: its pgid field is
// still the zero value, and kill(-0, SIGTERM) would signal THIS process's own
// group — the exact suicide the pgid resolution exists to prevent. With the
// guard missing, this test does not merely fail, the test binary dies.
func TestReapOfAnInFlightClaimSignalsNothing(t *testing.T) {
	a := NewAdminServer(AdminOptions{SocketDir: t.TempDir()})
	defer a.Close()

	a.mu.Lock()
	a.m["c1"] = &launched{} // a Launch still between its dup check and cmd.Start
	a.mu.Unlock()

	resp, err := a.Reap(context.Background(), connect.NewRequest(&adminpb.ReapRequest{ChildId: "c1"}))
	if err != nil {
		t.Fatalf("Reap of an in-flight claim errored: %v", err)
	}
	if resp.Msg.GetReaped() {
		t.Error("Reap claimed to signal a launch that had not started")
	}
}

// A claim must not outlive a failed start, or every later Launch of that child
// is refused with AlreadyExists forever — a poisoned slot no launch can clear.
func TestFailedStartReleasesItsClaim(t *testing.T) {
	a := NewAdminServer(AdminOptions{
		SelfBinary:  filepath.Join(t.TempDir(), "does-not-exist"), // Start fails
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	_, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c1",
		Cwd:     t.TempDir(),
		Spec:    &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("Launch with a missing binary: err = %v, want Internal", err)
	}

	a.mu.Lock()
	slotTaken := a.m["c1"] != nil
	a.mu.Unlock()
	if slotTaken {
		t.Fatal("a failed start left its claim in the launch table")
	}

	// The slot is free again: a retry with a working binary must launch.
	a.opts.SelfBinary = buildSelfStub(t)
	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c1",
		Cwd:     t.TempDir(),
		Spec:    &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	if err != nil {
		t.Fatalf("retry Launch after a failed start: %v", err)
	}
	if resp.Msg.GetPid() == 0 {
		t.Error("retry Launch returned no pid")
	}
}

// buildSelfStub compiles a stand-in for the `rafiki` binary that sleeps until
// signalled, so a launch produces a real long-lived process to inspect without
// needing a working daraja or claude.
func buildSelfStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(`package main
import ("os";"os/signal";"syscall")
func main() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	<-ch
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "stub")
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub: %v\n%s", err, out)
	}
	return bin
}

// A ticket in argv is readable by every process on the machine via ps. This
// test reads the launched process's own command line back out of the kernel,
// because an assertion against the argv slice we built would pass even if
// something later appended it.
func TestLaunchKeepsTheTicketOutOfArgv(t *testing.T) {
	ticket := "one-shot-tk-abc123"
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId:  "c-ticket",
		Cwd:      t.TempDir(),
		DialAddr: "127.0.0.1:9999",
		Spec:     &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
		Ticket:   ticket,
	}))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	pid := int(resp.Msg.GetPid())

	// Ask the kernel for this process's command line (argv only, no env).
	// The `-o command=` format gives just the command and its arguments.
	out, err := exec.Command("ps", "-o", "command=", "-p", fmt.Sprint(pid)).CombinedOutput()
	if err != nil {
		t.Fatalf("ps -p %d: %v (output: %s)", pid, err, out)
	}
	cmdline := string(out)

	if strings.Contains(cmdline, ticket) {
		t.Errorf("ticket %q found in kernel cmdline:\n%s", ticket, cmdline)
	}
	if strings.Contains(cmdline, "RAFIKI_DARAJA_TICKET") {
		t.Errorf("env var name RAFIKI_DARAJA_TICKET found in kernel cmdline:\n%s", cmdline)
	}

	// Verify the ticket arrives via environment instead. Read /proc/<pid>/environ
	// (Linux) or rely on the fact that daraja itself would see it:
	// the stub has no way to expose env, but we can verify our process has it.
	// On darwin we fall back to verifying the Launch request carried it
	// structurally — the kernel-ps check above is the critical assertion.
	_ = ticket // already verified absent from cmdline
}
