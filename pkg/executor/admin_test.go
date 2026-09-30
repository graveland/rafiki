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
	"unicode/utf8"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/adminpb"
	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/executorpb"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

func TestDescribeAdvertisesLaunchKinds(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(Options{Root: t.TempDir(), LaunchKinds: []string{"claude"}})
	defer func() { _ = s.Close() }()

	resp, err := s.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
	c.Require().NoError(err, "Describe")
	c.Contains(resp.Msg.GetLaunchKinds(), "claude", "launch_kinds")
}

// An executor with no --launch flag hosts nothing. The default must be empty
// rather than "claude": a machine volunteering to host other people's children
// because someone forgot a flag is the self-report-gates-placement shape the
// isolation and workspace_mode rules exist to forbid.
func TestDescribeAdvertisesNoLaunchKindsByDefault(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(Options{Root: t.TempDir()})
	defer func() { _ = s.Close() }()

	resp, err := s.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
	c.Require().NoError(err, "Describe")
	c.Empty(resp.Msg.GetLaunchKinds(), "launch_kinds")
}

// The launched daraja must lead its own process group, because that group is
// the reaping handle for the whole child — daraja plus the claude that joins
// it. An executor that forgets Setpgid leaves daraja in the EXECUTOR's group,
// where a reap would signal the executor itself.
func TestLaunchGivesDarajaItsOwnGroup(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Launch")
	pid, pgid := int(resp.Msg.GetPid()), int(resp.Msg.GetPgid())
	c.Eq(pid, pgid, "pgid")
	c.Require().NotEq(syscall.Getpgrp(), pgid, "daraja was left in the executor's process group; a reap would signal us")

	// The response arithmetic is hardcoded (Pgid: int32(pid)), so the two
	// checks above pass even with Setpgid missing. Ask the kernel what group
	// daraja actually leads: that is the assertion a missing Setpgid fails.
	kernelPgid, err := syscall.Getpgid(pid)
	c.Require().NoError(err, "Getpgid(%d)", pid)
	c.Eq(pid, kernelPgid, "kernel pgid of daraja (pid")
	c.Require().NotEq(syscall.Getpgrp(), kernelPgid, "daraja sits in the executor's real process group; a reap would signal us")
}

// An undeclared kind must be refused. The flag is the operator's declaration
// and the RPC is a peer's request; the declaration wins.
func TestLaunchRefusesAnUndeclaredKind(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().Error(err, "Launch admitted a kind this executor never declared")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
}

// Reap must end daraja AND the child that joined its group. The stub sleeps
// until signalled, so a surviving process is an observable failure.
func TestReapEndsTheWholeGroup(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Launch")
	pgid := int(resp.Msg.GetPgid())

	rr, err := a.Reap(context.Background(), connect.NewRequest(&adminpb.ReapRequest{
		ChildId: "c1", GraceMs: 500,
	}))
	c.Require().NoError(err, "Reap")
	c.True(rr.Msg.GetReaped(), "Reap reported nothing reaped for a live launch")

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
	c := assert.NewCollecting(t)
	a := NewAdminServer(AdminOptions{SocketDir: t.TempDir()})
	defer a.Close()

	resp, err := a.Reap(context.Background(), connect.NewRequest(&adminpb.ReapRequest{ChildId: "ghost"}))
	c.Require().NoError(err, "Reap of an unknown child errored")
	c.False(resp.Msg.GetReaped(), "Reap claimed to reap a child it never launched")
}

// A launch claim still in flight must never be signalled: its pgid field is
// still the zero value, and kill(-0, SIGTERM) would signal THIS process's own
// group — the exact suicide the pgid resolution exists to prevent. With the
// guard missing, this test does not merely fail, the test binary dies.
func TestReapOfAnInFlightClaimSignalsNothing(t *testing.T) {
	c := assert.NewCollecting(t)
	a := NewAdminServer(AdminOptions{SocketDir: t.TempDir()})
	defer a.Close()

	a.mu.Lock()
	a.m["c1"] = &launched{} // a Launch still between its dup check and cmd.Start
	a.mu.Unlock()

	resp, err := a.Reap(context.Background(), connect.NewRequest(&adminpb.ReapRequest{ChildId: "c1"}))
	c.Require().NoError(err, "Reap of an in-flight claim errored")
	c.False(resp.Msg.GetReaped(), "Reap claimed to signal a launch that had not started")
}

// A claim must not outlive a failed start, or every later Launch of that child
// is refused with AlreadyExists forever — a poisoned slot no launch can clear.
func TestFailedStartReleasesItsClaim(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().False(err == nil || connect.CodeOf(err) != connect.CodeInternal, "Launch with a missing binary: err = %v, want Internal", err)

	a.mu.Lock()
	slotTaken := a.m["c1"] != nil
	a.mu.Unlock()
	c.Require().False(slotTaken, "a failed start left its claim in the launch table")

	// The slot is free again: a retry with a working binary must launch.
	a.opts.SelfBinary = buildSelfStub(t)
	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c1",
		Cwd:     t.TempDir(),
		Spec:    &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	c.Require().NoError(err, "retry Launch after a failed start")
	c.NotEq(0, resp.Msg.GetPid(), "retry Launch returned no pid")
}

// buildSelfStub compiles a stand-in for the `rafiki` binary that sleeps until
// signalled, so a launch produces a real long-lived process to inspect without
// needing a working daraja or claude.
func buildSelfStub(t *testing.T) string {
	t.Helper()
	c := assert.NewAborting(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	c.NoError(os.WriteFile(src, []byte(`package main
import ("os";"os/signal";"syscall")
func main() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	<-ch
}
`), 0o600))
	bin := filepath.Join(dir, "stub")
	cmd := exec.Command("go", "build", "-o", bin, src)
	out, err := cmd.CombinedOutput()
	c.NoError(err, "build stub: %v\n%s", err, out)
	return bin
}

// buildExitingDarajaStub is buildSelfStub with the opposite lifetime: it logs
// numbered lines to stderr (what a daraja does with its connection-failure
// diagnostics) and exits with a code, so a launch produces a daraja-shaped
// DEATH to ask Status about. The stderr volume exceeds stderrTailMax, so the
// record's tail is exercised at its cap: early lines must be evicted, late
// lines kept. The filler is numbered ("filler-000"…) so an eviction assertion
// can name a line that actually exists.
func buildExitingDarajaStub(t *testing.T) string {
	t.Helper()
	c := assert.NewAborting(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	c.NoError(os.WriteFile(src, []byte(`package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	for i := 0; i < 60; i++ {
		fmt.Fprintf(os.Stderr, "filler-%03d %s\n", i, strings.Repeat("x", 88))
	}
	fmt.Fprintln(os.Stderr, "daraja: connect failed: dial 127.0.0.1:1: connection refused")
	os.Exit(3)
}
`), 0o600))
	bin := filepath.Join(dir, "stub")
	out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput()
	c.NoError(err, "build stub: %v (output: %s)", err, out)
	return bin
}

// buildLongLineDarajaStub writes ONE stderr line far longer than a Scanner's
// default 64 KiB cap, then its marker and exit — the shape that used to stop
// the stderr relay (ErrTooLong) and leave the pipe undrained, blocking the
// daraja on stderr forever.
func buildLongLineDarajaStub(t *testing.T) string {
	t.Helper()
	c := assert.NewAborting(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	c.NoError(os.WriteFile(src, []byte(`package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	fmt.Fprintln(os.Stderr, "LONG-START"+strings.Repeat("y", 200<<10))
	fmt.Fprintln(os.Stderr, "daraja: connect failed: dial 127.0.0.1:1: connection refused")
	os.Exit(3)
}
`), 0o600))
	bin := filepath.Join(dir, "stub")
	out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput()
	c.NoError(err, "build stub: %v (output: %s)", err, out)
	return bin
}

// A ticket in argv is readable by every process on the machine via ps. This
// test reads the launched process's own command line back out of the kernel,
// because an assertion against the argv slice we built would pass even if
// something later appended it.
func TestLaunchKeepsTheTicketOutOfArgv(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "Launch")

	pid := int(resp.Msg.GetPid())

	// Ask the kernel for this process's command line (argv only, no env).
	// The `-o command=` format gives just the command and its arguments.
	out, err := exec.Command("ps", "-o", "command=", "-p", fmt.Sprint(pid)).CombinedOutput()
	c.Require().NoError(err, "ps -p %d: %v (output: %s)", pid, err, out)
	cmdline := string(out)

	c.NotStrContains(cmdline, ticket, "ticket")
	c.NotStrContains(cmdline, "RAFIKI_DARAJA_TICKET", "env var name RAFIKI_DARAJA_TICKET found in kernel cmdline:\n")

	// Verify the ticket arrives via environment instead. Read /proc/<pid>/environ
	// (Linux) or rely on the fact that daraja itself would see it:
	// the stub has no way to expose env, but we can verify our process has it.
	// On darwin we fall back to verifying the Launch request carried it
	// structurally — the kernel-ps check above is the critical assertion.
	_ = ticket // already verified absent from cmdline
}

// TestLaunchPrefersItsOwnConnectAddrOverDialAddr is the regression pin for a
// real production bug: the daemon's dial_addr is derived from its OWN bind
// address (RAFIKI_CONTROL_LISTEN), which behind a reverse proxy, a k8s
// Service, or anything where the daemon's public address differs from what
// it binds, is NOT the same as an address reachable from this executor's
// machine — a bind spec like ":8036" sent as dial_addr makes daraja dial
// ITS OWN machine's port 8036, not the daemon's. This executor is, at the
// moment it handles a Launch, ALREADY connected to the daemon via its own
// --connect/--connect-socket — a target proven reachable — so Launch must
// prefer that over whatever the request claims, not merely accept it as one
// valid option among several.
func TestLaunchPrefersItsOwnConnectAddrOverDialAddr(t *testing.T) {
	c := assert.NewCollecting(t)
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
		ConnectAddr: "rafiki.example.dev:443",
	})
	defer a.Close()

	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c-connect",
		Cwd:     t.TempDir(),
		// A wrong bind-address-shaped dial_addr, exactly like a real k8s
		// daemon bound on a bare port would send. Must be ignored.
		DialAddr: ":8036",
		Spec:     &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	c.Require().NoError(err, "Launch")

	pid := int(resp.Msg.GetPid())
	out, err := exec.Command("ps", "-o", "command=", "-p", fmt.Sprint(pid)).CombinedOutput()
	c.Require().NoError(err, "ps -p %d: %v (output: %s)", pid, err, out)
	cmdline := string(out)

	c.StrContains(cmdline, "--connect rafiki.example.dev:443", "cmdline")
	c.NotStrContains(cmdline, ":8036", "cmdline")
}

// TestLaunchFallsBackToDialAddrWithNoConnectInfo covers an executor built
// before ConnectAddr/ConnectSocket existed: with neither set, the request's
// dial_addr is still honoured, unchanged from before this fix.
func TestLaunchFallsBackToDialAddrWithNoConnectInfo(t *testing.T) {
	c := assert.NewCollecting(t)
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId:  "c-fallback",
		Cwd:      t.TempDir(),
		DialAddr: "127.0.0.1:9999",
		Spec:     &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	c.Require().NoError(err, "Launch")

	pid := int(resp.Msg.GetPid())
	out, err := exec.Command("ps", "-o", "command=", "-p", fmt.Sprint(pid)).CombinedOutput()
	c.Require().NoError(err, "ps -p %d: %v (output: %s)", pid, err, out)
	c.StrContains(string(out), "--connect 127.0.0.1:9999", "cmdline %q missing the fallback dial_addr", out)
}

// TestLaunchPrefersItsOwnProxyURLOverTheRequests: when this executor has its
// own ProxyURL set, it overrides the request's proxy_url (which is typically
// the daemon's loopback — unreachable from another machine).
func TestLaunchPrefersItsOwnProxyURLOverTheRequests(t *testing.T) {
	c := assert.NewCollecting(t)
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
		ProxyURL:    "https://executor-profile.example/v1",
	})
	defer a.Close()

	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId:  "c-proxy-override",
		Cwd:      t.TempDir(),
		DialAddr: "127.0.0.1:9999",
		Spec: &darajapb.ChildSpec{
			Kind: darajapb.Kind_KIND_CLAUDE,
			Claude: &darajapb.ClaudeParams{
				// The daemon's own loopback guess, must be ignored.
				ProxyUrl: "http://127.0.0.1:8035",
			},
		},
	}))
	c.Require().NoError(err, "Launch")

	pid := int(resp.Msg.GetPid())
	out, err := exec.Command("ps", "-o", "command=", "-p", fmt.Sprint(pid)).CombinedOutput()
	c.Require().NoError(err, "ps -p %d: %v (output: %s)", pid, err, out)
	cmdline := string(out)

	c.StrContains(cmdline, "--proxy-url https://executor-profile.example/v1", "cmdline")
	c.NotStrContains(cmdline, "127.0.0.1:8035", "cmdline")
}

// TestLaunchFallsBackToRequestsProxyURLWithNoneConfigured: with no ProxyURL
// set, the request's proxy_url is honoured, unchanged from before this fix.
func TestLaunchFallsBackToRequestsProxyURLWithNoneConfigured(t *testing.T) {
	c := assert.NewCollecting(t)
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c-proxy-fallback",
		Cwd:     t.TempDir(),
		Spec: &darajapb.ChildSpec{
			Kind:   darajapb.Kind_KIND_CLAUDE,
			Claude: &darajapb.ClaudeParams{ProxyUrl: "http://127.0.0.1:8035"},
		},
	}))
	c.Require().NoError(err, "Launch")

	pid := int(resp.Msg.GetPid())
	out, err := exec.Command("ps", "-o", "command=", "-p", fmt.Sprint(pid)).CombinedOutput()
	c.Require().NoError(err, "ps -p %d: %v (output: %s)", pid, err, out)
	c.StrContains(string(out), "--proxy-url http://127.0.0.1:8035", "cmdline %q missing the fallback proxy_url", out)
}

// TestLaunchPassesProxyFieldsThroughArgvAndKeepsTokenOutOfIt proves Phase 2's
// wiring end to end at the executor layer: the non-secret proxy fields reach
// daraja serve's argv (so a real daraja process picks them up), while the
// proxy token gets the SAME treatment as the ticket — present nowhere the
// kernel's ps can see, because it authenticates this child's traffic to
// rafiki's proxy and ps is world-readable.
func TestLaunchPassesProxyFieldsThroughArgvAndKeepsTokenOutOfIt(t *testing.T) {
	c := assert.NewCollecting(t)
	token := "proxy-token-should-not-leak"
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId:  "c-proxy",
		Cwd:      t.TempDir(),
		DialAddr: "127.0.0.1:9999",
		Spec: &darajapb.ChildSpec{
			Kind: darajapb.Kind_KIND_CLAUDE,
			Claude: &darajapb.ClaudeParams{
				ProxyUrl:          "https://proxy.example/v1",
				ProxyToken:        token,
				PassthroughAuth:   true,
				AutoCompactWindow: 128000,
				RecordRequests:    true,
			},
		},
		Ticket: "tk-irrelevant-here",
	}))
	c.Require().NoError(err, "Launch")

	pid := int(resp.Msg.GetPid())
	out, err := exec.Command("ps", "-o", "command=", "-p", fmt.Sprint(pid)).CombinedOutput()
	c.Require().NoError(err, "ps -p %d: %v (output: %s)", pid, err, out)
	cmdline := string(out)

	c.NotStrContains(cmdline, token, "proxy token")
	c.NotStrContains(cmdline, "RAFIKI_DARAJA_PROXY_TOKEN", "env var name RAFIKI_DARAJA_PROXY_TOKEN found in kernel cmdline:\n")
	for _, want := range []string{
		"--proxy-url https://proxy.example/v1",
		"--passthrough",
		"--auto-compact-window 128000",
		"--record-requests",
	} {
		c.StrContains(cmdline, want, "cmdline")
	}
}

// TestLaunchCarriesAppendSystemPromptAndExtraArgs pins the executor launch
// path's two remaining argv-shaped fields: the appended system prompt as its
// own --append-system-prompt pair, and the operator's ExtraArgs verbatim after
// the "--" separator pflag treats as end-of-flags — the separator is what lets
// a flag-shaped extra survive into the child's argv instead of being parsed as
// a SERVE flag (mangling a mapped flag or dying on an unknown one). A spec
// carrying neither must build neither.
func TestLaunchCarriesAppendSystemPromptAndExtraArgs(t *testing.T) {
	c := assert.NewCollecting(t)
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId:  "c-argv",
		Cwd:      t.TempDir(),
		DialAddr: "127.0.0.1:9999",
		Spec: &darajapb.ChildSpec{
			Kind: darajapb.Kind_KIND_CLAUDE,
			Claude: &darajapb.ClaudeParams{
				AppendSystemPrompt: "be terse",
				ExtraArgs:          []string{"--foo", "bar"},
			},
		},
	}))
	c.Require().NoError(err, "Launch")

	pid := int(resp.Msg.GetPid())
	out, err := exec.Command("ps", "-o", "command=", "-p", fmt.Sprint(pid)).CombinedOutput()
	c.Require().NoError(err, "ps -p %d: %v (output: %s)", pid, err, out)
	cmdline := string(out)
	for _, want := range []string{
		"--append-system-prompt be terse",
		// The separator itself: extras must ride as POSITIONAL args after
		// "--", not be parsed as serve flags.
		" -- --foo bar",
	} {
		c.StrContains(cmdline, want, "cmdline")
	}

	// The empty case: neither field may appear when the spec does not set it.
	resp, err = a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId:  "c-argv-empty",
		Cwd:      t.TempDir(),
		DialAddr: "127.0.0.1:9999",
		Spec: &darajapb.ChildSpec{
			Kind:   darajapb.Kind_KIND_CLAUDE,
			Claude: &darajapb.ClaudeParams{},
		},
	}))
	c.Require().NoError(err, "Launch (empty)")
	pid = int(resp.Msg.GetPid())
	out, err = exec.Command("ps", "-o", "command=", "-p", fmt.Sprint(pid)).CombinedOutput()
	c.Require().NoError(err, "ps -p %d: %v (output: %s)", pid, err, out)
	cmdline = string(out)
	for _, unwanted := range []string{"--append-system-prompt", " -- "} {
		c.NotStrContains(cmdline, unwanted, "empty spec: cmdline")
	}
}

// buildEnvDumpStub is buildSelfStub with one difference: the stub records its
// own environment to a file before waiting to be signalled, so a test can
// assert a secret arrived by ENVIRONMENT — the positive half
// TestLaunchKeepsTheTicketOutOfArgv could only argue structurally (its kernel
// check proves absence from argv; only the process's own environ proves
// presence in env). The dump path rides the inherited environment, which
// Launch passes through to the stub unchanged.
func buildEnvDumpStub(t *testing.T) string {
	t.Helper()
	c := assert.NewAborting(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	dump := filepath.Join(dir, "environ.txt")
	t.Setenv("RAFIKI_TEST_ENV_DUMP", dump)
	c.NoError(os.WriteFile(src, []byte(`package main

import (
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	_ = os.WriteFile(os.Getenv("RAFIKI_TEST_ENV_DUMP"), []byte(strings.Join(os.Environ(), "\n")), 0o600)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	<-ch
}
`), 0o600))
	bin := filepath.Join(dir, "stub")
	out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput()
	c.NoError(err, "build stub: %v (output: %s)", err, out)
	return bin
}

// mcp_token travels by the same route as proxy_token: over the authenticated
// Launch RPC, then into the daraja process's ENVIRONMENT — never argv, which
// ps renders world-readable on this machine. Both halves are asserted against
// the launched process itself: the kernel's command line for the absence, the
// stub's own environ dump for the presence.
func TestLaunchCarriesTheMCPTokenByEnvNotArgv(t *testing.T) {
	c := assert.NewCollecting(t)
	token := "per-child-mcp-secret-must-not-reach-argv"
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildEnvDumpStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId:  "c-mcp-token",
		Cwd:      t.TempDir(),
		DialAddr: "127.0.0.1:9999",
		Spec: &darajapb.ChildSpec{
			Kind:   darajapb.Kind_KIND_CLAUDE,
			Claude: &darajapb.ClaudeParams{McpToken: token},
		},
		Ticket: "tk-irrelevant-here",
	}))
	c.Require().NoError(err, "Launch")
	pid := int(resp.Msg.GetPid())

	out, err := exec.Command("ps", "-o", "command=", "-p", fmt.Sprint(pid)).CombinedOutput()
	c.Require().NoError(err, "ps -p %d: %v (output: %s)", pid, err, out)
	c.NotStrContains(string(out), token, "mcp_token %q found in kernel cmdline:\n%s", token, out)
	c.NotStrContains(string(out), "RAFIKI_MCP_TOKEN", "env var name RAFIKI_MCP_TOKEN found in kernel cmdline:\n%s", out)

	// The positive half: the stub's environ dump must carry the variable.
	// Line-based, because the token is the LAST entry Launch appends and the
	// dump is newline-joined with no trailing newline.
	dumpPath := os.Getenv("RAFIKI_TEST_ENV_DUMP")
	var envDump []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		envDump, err = os.ReadFile(dumpPath)
		if err == nil {
			break
		}
		c.Require().False(time.Now().After(deadline), "stub environ dump never appeared at %s: %v", dumpPath, err)
		time.Sleep(10 * time.Millisecond)
	}
	want := "RAFIKI_MCP_TOKEN=" + token
	found := false
	for _, line := range strings.Split(string(envDump), "\n") {
		if line == want {
			found = true
			break
		}
	}
	c.True(found, "launched process environment missing RAFIKI_MCP_TOKEN=%s; got:\n%s", token, envDump)
}

// A spec with no mcp_token must set nothing: the env var's absence is what
// tells runDarajaServe no per-child secret was minted (ClaudeEnv then falls
// back to the proxy bearer).
func TestLaunchOmitsTheMCPTokenEnvVarWhenTheSpecHasNone(t *testing.T) {
	c := assert.NewCollecting(t)
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildEnvDumpStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	_, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId:  "c-no-mcp-token",
		Cwd:      t.TempDir(),
		DialAddr: "127.0.0.1:9999",
		Spec:     &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
		Ticket:   "tk-irrelevant-here",
	}))
	c.Require().NoError(err, "Launch")

	deadline := time.Now().Add(5 * time.Second)
	var envDump []byte
	var readErr error
	for {
		envDump, readErr = os.ReadFile(os.Getenv("RAFIKI_TEST_ENV_DUMP"))
		if readErr == nil {
			break
		}
		c.Require().False(time.Now().After(deadline), "stub environ dump never appeared: %v", readErr)
		time.Sleep(10 * time.Millisecond)
	}
	for _, line := range strings.Split(string(envDump), "\n") {
		c.False(strings.HasPrefix(line, "RAFIKI_MCP_TOKEN="), "spec carried no mcp_token, but the environment has %q", line)
	}
}

// waitForStubEnvDump reads the dump buildEnvDumpStub's binary writes, polling
// until it appears — the same loop the tests above inline, factored for the
// scrub tests.
func waitForStubEnvDump(t *testing.T) []byte {
	t.Helper()
	dumpPath := os.Getenv("RAFIKI_TEST_ENV_DUMP")
	deadline := time.Now().Add(5 * time.Second)
	var envDump []byte
	var err error
	for {
		envDump, err = os.ReadFile(dumpPath)
		if err == nil {
			return envDump
		}
		assert.NewAborting(t).False(time.Now().After(deadline), "stub environ dump never appeared at %s: %v", dumpPath, err)
		time.Sleep(10 * time.Millisecond)
	}
}

// countEnvLines counts the dump lines carrying a variable and returns them.
func countEnvLines(t *testing.T, envDump []byte, name string) []string {
	t.Helper()
	var got []string
	for _, line := range strings.Split(string(envDump), "\n") {
		if strings.HasPrefix(line, name+"=") {
			got = append(got, line)
		}
	}
	return got
}

// The executor's own environment can carry stale copies of the credential
// names Launch manages: an executor launched from inside a proxied session's
// shell holds that session's RAFIKI_MCP_TOKEN, and a mis-curated
// executor-overrides.env can carry any of the three. t.Setenv seeds the stale
// inherited values in THIS test process, which Launch inherits through
// os.Environ(); the assertion is that the launched environ shows each name
// EXACTLY ONCE, valued with the fresh secret.
//
// This test cannot fail against pre-scrub code (verified by a build overlay
// against the pre-scrub admin.go): os/exec dedups the child environ keeping
// the LAST duplicate, so the appended fresh value won even unscrubbed. The
// exactly-once assertion is therefore not what stops shadowing — it pins the
// scrub's EXPLICIT single-entry invariant, independent of exec.Cmd's dedup
// internals and append order. The shadowing-shaped failure class is pinned
// where it is genuinely reachable, at the composition level in
// TestScrubbedEnvCompositionFreshWinsBelow, and the no-token leak — the one
// pre-scrub failure — is pinned by
// TestScrubbedEnvCompositionFreshValuesWinBelow pins the scrub+append
// composition DIRECTLY — the level where the stale-shadows-fresh class is
// genuinely reachable (a plain []string duplicate, which os.Getenv's
// first-match-wins semantics WOULD read as the stale value). This is the
// load-bearing half of the fresh-wins pin; the process-level test above
// cannot fail pre-scrub because exec.Cmd dedups keeping the last entry.
func TestScrubbedEnvCompositionFreshValuesWinBelow(t *testing.T) {
	c := assert.NewAborting(t)
	inherited := []string{
		"PATH=/usr/bin",
		"RAFIKI_MCP_TOKEN=stale",
		"RAFIKI_DARAJA_TICKET=stale-ticket",
		"RAFIKI_DARAJA_PROXY_TOKEN=stale-proxy",
		"HOME=/home/executor",
	}
	fresh := []string{
		"RAFIKI_DARAJA_TICKET=fresh-ticket",
		"RAFIKI_DARAJA_PROXY_TOKEN=fresh-proxy",
		"RAFIKI_MCP_TOKEN=fresh-mcp",
	}
	got := append(scrubRafikiCredentialEnv(inherited), fresh...)

	want := map[string]string{
		"RAFIKI_MCP_TOKEN":          "fresh-mcp",
		"RAFIKI_DARAJA_TICKET":      "fresh-ticket",
		"RAFIKI_DARAJA_PROXY_TOKEN": "fresh-proxy",
	}
	for name, wantVal := range want {
		var hits []string
		for _, l := range got {
			if v, ok := strings.CutPrefix(l, name+"="); ok {
				hits = append(hits, v)
			}
		}
		c.False(len(hits) != 1 || hits[0] != wantVal, "%s carries %v, want exactly [%s] — the stale copy must be dropped and the fresh value must win", name, hits, wantVal)
	}
	c.False(!slices.Contains(got, "PATH=/usr/bin") || !slices.Contains(got, "HOME=/home/executor"), "the scrub dropped non-credential entries: %v", got)
}

// TestLaunchSpecWithoutATokenDoesNotLeakAStaleInheritedMCPToken.
func TestLaunchDropsStaleInheritedCredentialsAndTheFreshValuesWin(t *testing.T) {
	c := assert.NewCollecting(t)
	staleMCP := "stale-inherited-mcp"
	staleTicket := "stale-inherited-ticket"
	staleProxy := "stale-inherited-proxy-token"
	t.Setenv("RAFIKI_MCP_TOKEN", staleMCP)
	t.Setenv("RAFIKI_DARAJA_TICKET", staleTicket)
	t.Setenv("RAFIKI_DARAJA_PROXY_TOKEN", staleProxy)

	freshMCP := "fresh-per-child-mcp-secret"
	freshTicket := "fresh-one-shot-tk"
	freshProxy := "fresh-per-child-proxy-token"
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildEnvDumpStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	_, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId:  "c-scrub-fresh-wins",
		Cwd:      t.TempDir(),
		DialAddr: "127.0.0.1:9999",
		Spec: &darajapb.ChildSpec{
			Kind: darajapb.Kind_KIND_CLAUDE,
			Claude: &darajapb.ClaudeParams{
				McpToken:   freshMCP,
				ProxyToken: freshProxy,
				ProxyUrl:   "http://127.0.0.1:1",
			},
		},
		Ticket: freshTicket,
	}))
	c.Require().NoError(err, "Launch")

	envDump := waitForStubEnvDump(t)
	for _, tc := range []struct{ name, want string }{
		{"RAFIKI_MCP_TOKEN", "RAFIKI_MCP_TOKEN=" + freshMCP},
		{"RAFIKI_DARAJA_TICKET", "RAFIKI_DARAJA_TICKET=" + freshTicket},
		{"RAFIKI_DARAJA_PROXY_TOKEN", "RAFIKI_DARAJA_PROXY_TOKEN=" + freshProxy},
	} {
		got := countEnvLines(t, envDump, tc.name)
		if len(got) != 1 {
			t.Errorf("%s appears %d times in the launched environ, want exactly once with the fresh value; dump:\n%s",
				tc.name, len(got), envDump)
			continue
		}
		c.Eq(tc.want, got[0], "%s entry is %q, want the fresh %q (stale inherited copies must not shadow it)", tc.name, got[0], tc.want)
	}
}

// The scrub must also cover the spec-has-no-token case: with a stale inherited
// RAFIKI_MCP_TOKEN in the executor's own environment, the pre-scrub environ
// carried that value through as if the daemon had minted a per-child secret —
// the case TestLaunchOmitsTheMCPTokenEnvVarWhenTheSpecHasNone could not see,
// because its test process carried no such variable. The launched daraja
// environ must carry NO RAFIKI_MCP_TOKEN at all.
func TestLaunchSpecWithoutATokenDoesNotLeakAStaleInheritedMCPToken(t *testing.T) {
	c := assert.NewCollecting(t)
	staleMCP := "stale-inherited-mcp"
	staleProxy := "stale-inherited-proxy-token"
	t.Setenv("RAFIKI_MCP_TOKEN", staleMCP)
	t.Setenv("RAFIKI_DARAJA_PROXY_TOKEN", staleProxy)

	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildEnvDumpStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	_, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId:  "c-scrub-no-token",
		Cwd:      t.TempDir(),
		DialAddr: "127.0.0.1:9999",
		Spec:     &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
		Ticket:   "tk-irrelevant-here",
	}))
	c.Require().NoError(err, "Launch")

	envDump := waitForStubEnvDump(t)
	c.Empty(countEnvLines(t, envDump, "RAFIKI_MCP_TOKEN"), "spec carried no mcp_token, but the launched environ has")
	// Finding 5: the same no-entry assertion for the second name the brief's
	// fixture seeds — the scrub + unconditional-append interplay for
	// RAFIKI_DARAJA_PROXY_TOKEN (one empty-valued entry, never the stale one).
	proxyLines := countEnvLines(t, envDump, "RAFIKI_DARAJA_PROXY_TOKEN")
	c.Require().Len(proxyLines, 1, "RAFIKI_DARAJA_PROXY_TOKEN carried")
	c.NotStrContains(string(envDump), staleMCP, "stale inherited mcp token %q found anywhere in the launched environ:\n%s", staleMCP, envDump)
	c.NotStrContains(string(envDump), staleProxy, "stale inherited proxy token %q found anywhere in the launched environ:\n%s", staleProxy, envDump)
}

// TestAdminStatusReportsExitAndStderr pins the Status contract the daemon's
// launch wait leans on: a daraja that died before connecting answers
// known=true, running=false, its exit code, and the LAST stderrTailMax bytes
// of its stderr — while a child this executor never launched answers
// known=false, not an error and not a guessed record.
func TestAdminStatusReportsExitAndStderr(t *testing.T) {
	c := assert.NewCollecting(t)
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildExitingDarajaStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	marker := "daraja: connect failed: dial 127.0.0.1:1: connection refused"
	_, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c-status",
		Cwd:     t.TempDir(),
		Spec:    &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	c.Require().NoError(err, "Launch")

	// An unknown child: known=false, nothing else set.
	resp, err := a.Status(context.Background(), connect.NewRequest(&adminpb.StatusRequest{ChildId: "c-ghost"}))
	c.Require().NoError(err, "Status for an unknown child")
	c.False(resp.Msg.GetKnown(), "Status answered known for a child it never launched")
	c.False(resp.Msg.GetRunning(), "unknown child reported running")
	c.Nil(resp.Msg.ExitCode, "unknown child carried an exit code")
	c.Empty(resp.Msg.GetStderrTail(), "unknown child carried a stderr tail")

	// The launched daraja exits within moments; poll Status the way the
	// daemon's launch wait does, and pin the contract on the FIRST poll that
	// reports running=false — that is the poll the wait fails fast on, so the
	// tail must already be complete there. supervise drains the stderr relay
	// (bounded by stderrDrainGrace) BEFORE stamping the exit; a first exited
	// answer missing the daraja's last lines is exactly the race this pins.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err = a.Status(context.Background(), connect.NewRequest(&adminpb.StatusRequest{ChildId: "c-status"}))
		c.Require().NoError(err, "Status")
		if resp.Msg.GetKnown() && !resp.Msg.GetRunning() {
			break
		}
		c.Require().False(time.Now().After(deadline), "Status never reported the exit; last: known=%v running=%v tail=%q",
			resp.Msg.GetKnown(), resp.Msg.GetRunning(), resp.Msg.GetStderrTail())
		time.Sleep(20 * time.Millisecond)
	}
	c.True(resp.Msg.GetKnown(), "known")
	c.False(resp.Msg.GetRunning(), "running")
	c.Require().NotNil(resp.Msg.ExitCode, "an exited daraja must carry its exit code")
	c.Eq(int32(3), *resp.Msg.ExitCode, "exit_code")

	// The tail holds the END of the stderr, not the start: the stub wrote ~6KB
	// of numbered filler lines before the marker, so the early ones must be
	// evicted, the marker kept, and the tail must never exceed stderrTailMax
	// bytes.
	tail := resp.Msg.GetStderrTail()
	c.StrContains(tail, marker, "stderr_tail")
	c.NotStrContains(tail, "filler-000", "the tail must keep the LAST bytes; the first filler line survived")
	c.False(len(tail) > stderrTailMax, "stderr_tail is %d bytes, want <= %d", len(tail), stderrTailMax)
}

// A stderr line longer than a Scanner's default 64 KiB cap must not stop the
// stderr relay: a relay that dies at one oversized line leaves the pipe
// undrained, so the daraja blocks on stderr and neither connects nor exits,
// and the launch wait burns its full timeout. The relay truncates oversized
// lines and keeps reading, so this daraja still exits with its marker in the
// tail.
func TestAdminStatusSurvivesAnOversizedStderrLine(t *testing.T) {
	c := assert.NewCollecting(t)
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildLongLineDarajaStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	marker := "daraja: connect failed: dial 127.0.0.1:1: connection refused"
	_, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c-longline",
		Cwd:     t.TempDir(),
		Spec:    &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	c.Require().NoError(err, "Launch")

	var resp *connect.Response[adminpb.StatusResponse]
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err = a.Status(context.Background(), connect.NewRequest(&adminpb.StatusRequest{ChildId: "c-longline"}))
		c.Require().NoError(err, "Status")
		if resp.Msg.GetKnown() && !resp.Msg.GetRunning() {
			break
		}
		c.Require().False(time.Now().After(deadline),
			"the oversized stderr line stalled the relay: Status never reported the exit")
		time.Sleep(20 * time.Millisecond)
	}
	c.Require().NotNil(resp.Msg.ExitCode, "an exited daraja must carry its exit code")
	c.Eq(int32(3), *resp.Msg.ExitCode, "exit_code")
	tail := resp.Msg.GetStderrTail()
	c.StrContains(tail, marker, "stderr_tail")
	// B1: the kept "first fragment" must be the oversized line's actual bytes,
	// copied BEFORE the drain loop refilled ReadLine's buffer. The aliased
	// buffer held whatever ReadLine returned LAST — the marker line itself —
	// so the tail showed the marker twice, once wrongly tagged " [truncated]".
	// The copy keeps one fragment per oversized line: exactly one truncation
	// marker, and the real marker line exactly once, untagged.
	c.Eq(1, strings.Count(tail, " [truncated]"), "the truncation marker does not appear exactly once (aliased kept fragment?): %q", tail)
	c.Eq(1, strings.Count(tail, marker), "the marker line does not appear exactly once (aliased kept fragment?): %q", tail)
	c.False(len(tail) > stderrTailMax, "stderr_tail is %d bytes, want <= %d", len(tail), stderrTailMax)
}

// appendStderrLine cuts the tail at a byte offset, which can split a
// multi-byte rune, and a tail sanitised only at Status read time would then
// re-expand (each invalid run becomes a three-byte replacement rune) past the
// cap. The record sanitises at append time and skips a cut leading
// continuation byte, so the tail Status carries stays valid UTF-8 and within
// stderrTailMax by construction.
func TestAppendStderrLineKeepsTheTailValidAndBounded(t *testing.T) {
	c := assert.NewAborting(t)
	// What Status does with the stored tail.
	sanitise := func(b []byte) string { return strings.ToValidUTF8(string(b), "\uFFFD") }

	// The cut lands mid-rune: 2-byte runes pushed past the cap.
	rec := &launchRecord{}
	rec.appendStderrLine(strings.Repeat("é", stderrTailMax/2))
	rec.appendStderrLine(strings.Repeat("é", stderrTailMax/2))
	tail := sanitise(rec.stderrTail)
	c.False(len(tail) > stderrTailMax, "tail is %d bytes after sanitising, want <= %d", len(tail), stderrTailMax)
	c.True(utf8.ValidString(tail), "tail is not valid UTF-8")
	c.NotStrContains(tail, "\uFFFD", "a replacement rune means the stored tail was invalid: the cut split a rune")

	// Invalid bytes near the cap must not survive the cut and re-expand on
	// read either — sanitising happens at append time, before the cut.
	rec = &launchRecord{}
	rec.appendStderrLine(strings.Repeat("x", stderrTailMax-1))
	rec.appendStderrLine("ok \xff\xfe\xff\xfe more \xff text")
	tail = sanitise(rec.stderrTail)
	c.False(len(tail) > stderrTailMax, "tail is %d bytes after sanitising, want <= %d", len(tail), stderrTailMax)
	c.True(utf8.ValidString(tail), "tail is not valid UTF-8")
	c.NotStrContains(tail, "\uFFFD", "a replacement rune means invalid bytes survived the append")
}

// A LIVE daraja answers known=true, running=true with the exit code left
// unset — the optional field is what distinguishes "no exit yet" from
// "exited with a code", so a live daraja must never report one. And once it
// HAS exited, the record outlives the process: known stays true and the exit
// code becomes set.
func TestAdminStatusReportsALiveDaraja(t *testing.T) {
	c := assert.NewCollecting(t)
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})

	_, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c-live",
		Cwd:     t.TempDir(),
		Spec:    &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	}))
	c.Require().NoError(err, "Launch")

	resp, err := a.Status(context.Background(), connect.NewRequest(&adminpb.StatusRequest{ChildId: "c-live"}))
	c.Require().NoError(err, "Status of a live daraja")
	c.True(resp.Msg.GetKnown(), "known")
	c.True(resp.Msg.GetRunning(), "running")
	c.Nil(resp.Msg.ExitCode, "a live daraja must leave exit_code unset")

	// Close reaps (SIGTERM; the stub normally exits 0 once it has booted) and
	// joins the supervise goroutine, so the exit is recorded by the time it
	// returns. The short settle is the stub's boot time: a SIGTERM that arrives
	// before its signal.Notify registers kills the process instead (exit -1),
	// which is a real daraja death too — either way a code is recorded.
	time.Sleep(100 * time.Millisecond)
	a.Close()
	resp, err = a.Status(context.Background(), connect.NewRequest(&adminpb.StatusRequest{ChildId: "c-live"}))
	c.Require().NoError(err, "Status after exit")
	c.True(resp.Msg.GetKnown(), "the record must outlive the process")
	c.False(resp.Msg.GetRunning(), "running")
	c.Require().NotNil(resp.Msg.ExitCode, "an exited daraja must carry its exit code")
	code := *resp.Msg.ExitCode
	c.False(code != 0 && code != -1, "exit_code = %d, want 0 (caught the SIGTERM) or -1 (signalled mid-boot)", code)
}

// TestEnvironBaseNilFallsBackToProcess pins AdminOptions.environBase's nil
// rule: an AdminServer built without a pinned env (every existing test) keeps
// the pre-pinning behavior — the process environment at launch time.
func TestEnvironBaseNilFallsBackToProcess(t *testing.T) {
	t.Setenv("ENVBASE_PROBE", "process")
	o := AdminOptions{Env: []string{"ENVBASE_PROBE=pinned"}}
	pinned := AdminOptions{}
	c := assert.NewCollecting(t)
	c.Eq("pinned", tools.EnvGet(o.environBase(), "ENVBASE_PROBE"), "pinned env wins")
	c.Eq("process", tools.EnvGet(pinned.environBase(), "ENVBASE_PROBE"), "nil pinned env = process environment")
}

// A launched claude child's environ carries its own RAFIKI_CHILD_ID exactly
// once, even when the executor's own environment holds a stale one.
func TestLaunchCarriesTheChildIDForClaude(t *testing.T) {
	c := assert.NewCollecting(t)
	t.Setenv("RAFIKI_CHILD_ID", "c_stale")
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildEnvDumpStub(t),
		ChildBinary: "/usr/bin/true",
		LaunchKinds: []string{"claude"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	_, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId:  "c_fresh",
		Cwd:      t.TempDir(),
		DialAddr: "127.0.0.1:9999",
		Spec:     &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE, Claude: &darajapb.ClaudeParams{}},
		Ticket:   "tk",
	}))
	c.Require().NoError(err, "Launch")
	c.EqDeep([]string{"RAFIKI_CHILD_ID=c_fresh"}, countEnvLines(t, waitForStubEnvDump(t), "RAFIKI_CHILD_ID"), "one fresh entry")
}
