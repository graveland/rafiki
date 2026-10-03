package daraja

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// testChildBinary writes a fake child: a shell script with the given body that
// ignores its arguments. Ignoring them is the point — the host now builds argv
// through claudeargv.Build, so the fake runs under claude's flags (-p
// --input-format stream-json ...) and /bin/sh or /bin/cat would die on the
// first one.
func testChildBinary(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-child")
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	return path
}

// testEchoBinary is the long-lived fake child: it announces itself by echoing
// its argv and stays up, so a restart test can tell the processes apart by the
// model the spec names.
func testEchoBinary(t *testing.T) string {
	t.Helper()
	return testChildBinary(t, `echo "$@"; sleep 30`)
}

// collectStdout drains events until it sees want or the deadline passes.
func collectStdout(t *testing.T, h *Host, want string, d time.Duration) string {
	t.Helper()
	deadline := time.After(d)
	var sb strings.Builder
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatalf("event channel closed; got %q, want %q", sb.String(), want)
			}
			if len(ev.Stdout) > 0 {
				sb.Write(ev.Stdout)
				if strings.Contains(sb.String(), want) {
					return sb.String()
				}
			}
		case <-deadline:
			t.Fatalf("timeout; got %q, want %q", sb.String(), want)
		}
	}
}

// assertPair checks argv carries flag immediately followed by value.
func assertPair(t *testing.T, argv []string, flag, value string) {
	t.Helper()
	for i, a := range argv {
		if a == flag && i+1 < len(argv) && argv[i+1] == value {
			return
		}
	}
	t.Errorf("argv %v missing pair [%s %s]", argv, flag, value)
}

// The host runs a process and relays what it writes.
func TestHostRelaysStdout(t *testing.T) {
	c := assert.NewCollecting(t)
	h := NewHost(HostOptions{Binary: testEchoBinary(t), Spec: ChildSpec{Kind: KindClaude}})
	c.Require().NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	collectStdout(t, h, "stream-json", 5*time.Second)

	c.NotEq(0, h.PID(), "PID is 0 after a successful Start")
	c.True(h.Running(), "Running is false after a successful Start")
}

// TestArgvSingleModelWithModelArgs proves the suppression claudeargv.Params
// applies: with HostOptions.ModelArgs set, the spawned process's argv carries
// ModelArgs' own --model pair (matching its custom-model-option env vars) and
// does NOT also carry a second, plain --model from the spec's Model — which
// would risk Claude Code's client-side allowlist rejecting the model before
// the custom option is even consulted (see HostOptions.ModelArgs).
func TestArgvSingleModelWithModelArgs(t *testing.T) {
	c := assert.NewAborting(t)
	h := NewHost(HostOptions{
		Binary:    testEchoBinary(t),
		Spec:      ChildSpec{Kind: KindClaude, Model: "openai/gpt-4o"},
		ModelArgs: []string{"--model", "rafiki: openai/gpt-4o"},
	})
	c.NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	argv := collectStdout(t, h, "stream-json", 5*time.Second)

	got := strings.Count(argv, "--model")
	c.Eq(1, got, "argv %q contains %d occurrences of --model, want exactly 1", argv, got)
	c.StrContains(argv, "rafiki: openai/gpt-4o", "argv")
	c.False(strings.Contains(argv, "--model openai/gpt-4o ") || strings.HasSuffix(strings.TrimSpace(argv), "--model openai/gpt-4o"), "argv %q carries the PLAIN --model claudeargv.Build would add unsuppressed", argv)
}

// TestNoModelArgsLeavesPlainModelFlagAlone is the control case: with no
// ModelArgs (every unproxied daraja), Model must still produce
// claudeargv.Build's ordinary --model.
func TestNoModelArgsLeavesPlainModelFlagAlone(t *testing.T) {
	c := assert.NewAborting(t)
	h := NewHost(HostOptions{
		Binary: testEchoBinary(t),
		Spec:   ChildSpec{Kind: KindClaude, Model: "claude-sonnet-5"},
	})
	c.NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	argv := collectStdout(t, h, "stream-json", 5*time.Second)
	c.StrContains(argv, "--model claude-sonnet-5", "argv")
}

// stagedFile extracts the file path a staged launch file's flag points at,
// from a fake child's echoed argv. Handles both the pair form
// (--append-system-prompt-file <path>) and the = form (--mcp-config=<path>).
func stagedFile(t *testing.T, out, flag string) string {
	t.Helper()
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == flag && i+1 < len(fields) {
			return fields[i+1]
		}
		if v, ok := strings.CutPrefix(f, flag+"="); ok {
			return v
		}
	}
	t.Fatalf("argv %q has no %s value", out, flag)
	return ""
}

// TestMCPConfigWithoutModelArgsLeavesPlainModelFlagAlone is the case Wave 1
// of the MCP-injection plan introduced: MCPConfig can be non-empty while
// carrying NO --model (just the MCP config JSON), and that must not suppress
// the plain --model claudeargv.Build would otherwise add.
func TestMCPConfigWithoutModelArgsLeavesPlainModelFlagAlone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := assert.NewCollecting(t)
	const mcp = `{"mcpServers":{}}`
	h := NewHost(HostOptions{
		Binary:    testEchoBinary(t),
		Spec:      ChildSpec{Kind: KindClaude, Model: "claude-sonnet-5"},
		MCPConfig: mcp,
	})
	c.Require().NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	argv := collectStdout(t, h, "stream-json", 5*time.Second)
	c.StrContains(argv, "--model claude-sonnet-5", "argv %q missing the plain --model; MCPConfig without a "+
		"ModelArgs pair must not suppress it", argv)
	got, err := os.ReadFile(stagedFile(t, argv, "--mcp-config"))
	c.Require().NoError(err, "read staged MCP config")
	c.Eq(mcp, string(got), "staged MCP config content")
}

// TestHostMCPConfigYieldsExactlyOneElementNotDoubled pins the standalone
// daraja path's consumption of proxyenv.Values.MCPConfig through
// HostOptions.MCPConfig: the value is BARE JSON and must render as EXACTLY ONE
// --mcp-config= element in the child's argv, now pointing at the staged file
// rather than carrying the JSON inline. The pre-fix producer assigned the
// full rendered element, so this path — which feeds the value to
// claudeargv.Params.MCPConfig verbatim — emitted --mcp-config=--mcp-config={...},
// a live bug invisible to host-level fixtures that constructed bare JSON
// directly.
func TestHostMCPConfigYieldsExactlyOneElementNotDoubled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := assert.NewAborting(t)
	const mcp = `{"mcpServers":{"rafiki":{"type":"http","url":"http://127.0.0.1:8035/mcp"}}}`
	h := NewHost(HostOptions{
		Binary:    testEchoBinary(t),
		Spec:      ChildSpec{Kind: KindClaude},
		MCPConfig: mcp,
	})
	c.NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	argv := collectStdout(t, h, "stream-json", 5*time.Second)
	got := strings.Count(argv, "--mcp-config=")
	c.Eq(1, got, "child argv %q carries %d --mcp-config= elements, want exactly 1 (a doubled prefix means the host was fed a rendered element instead of bare JSON)", argv, got)
	c.NotStrContains(argv, "--mcp-config=--mcp-config=", "child argv")
	c.NotStrContains(argv, `{"mcpServers"`, "child argv still carries the inline MCP JSON; it must be staged to a file")
	content, err := os.ReadFile(stagedFile(t, argv, "--mcp-config"))
	c.Require().NoError(err, "read staged MCP config")
	c.Eq(mcp, string(content), "staged MCP config content")
}

// A spec's AppendSystemPrompt and ExtraArgs must both reach the built argv:
// the prompt as its own pair, the extra args appended after everything else.
func TestArgvCarriesAppendSystemPromptAndExtraArgs(t *testing.T) {
	argv := ChildSpec{
		Kind:               KindClaude,
		AppendSystemPrompt: "be terse",
		ExtraArgs:          []string{"--foo", "bar"},
	}.Argv("", nil)
	assertPair(t, argv, "--append-system-prompt", "be terse")
	assert.NewAborting(t).False(len(argv) < 2 || argv[len(argv)-2] != "--foo" || argv[len(argv)-1] != "bar", "want ExtraArgs last, got %v", argv)
}

// TestStagedArgvMatchesArgvModuloFiles: staged argv is plain argv with the
// appendix pair and the MCP element swapped for file paths, and the files hold
// exactly the texts the plain form put on argv. This is the pin that the two
// producers cannot drift on a flag when one gains staging.
func TestStagedArgvMatchesArgvModuloFiles(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := assert.NewCollecting(t)
	const appendix = "be terse"
	const mcp = `{"mcpServers":{}}`
	spec := ChildSpec{Kind: KindClaude, Model: "m", AppendSystemPrompt: appendix}

	plain := spec.Argv(mcp, nil)
	staged, err := spec.StagedArgv(mcp, nil, t.TempDir())
	c.Require().NoError(err, "StagedArgv")

	appendPath := stagedFile(t, strings.Join(staged, " "), "--append-system-prompt-file")
	mcpPath := stagedFile(t, strings.Join(staged, " "), "--mcp-config")

	var want []string
	for i := 0; i < len(plain); {
		switch a := plain[i]; {
		case a == "--append-system-prompt":
			want = append(want, "--append-system-prompt-file", appendPath)
			i += 2
		case strings.HasPrefix(a, "--mcp-config="):
			want = append(want, "--mcp-config="+mcpPath)
			i++
		default:
			want = append(want, a)
			i++
		}
	}
	c.EqDiff(want, staged, "StagedArgv = %v, want plain argv with the appendix pair and MCP element swapped for files", staged)

	content, err := os.ReadFile(appendPath)
	c.Require().NoError(err, "read staged appendix")
	c.Eq(appendix, string(content), "staged appendix content")
	mcpContent, err := os.ReadFile(mcpPath)
	c.Require().NoError(err, "read staged MCP config")
	c.Eq(mcp, string(mcpContent), "staged MCP config content")
}

// TestStartLockedStagesAndRestartRestages: a live launch stages the appendix
// to a file, and a spec-less Restart restages the SAME held text — the file the
// replacement process is pointed at holds the appendix the host still holds.
func TestStartLockedStagesAndRestartRestages(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	c := assert.NewCollecting(t)
	const appendix = "restart me"
	h := NewHost(HostOptions{
		Binary: testEchoBinary(t),
		Spec:   ChildSpec{Kind: KindClaude, AppendSystemPrompt: appendix},
	})
	c.Require().NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	first := collectStdout(t, h, "--dangerously-skip-permissions", 5*time.Second)
	c.NotStrContains(first, "--append-system-prompt ", "launch argv still carries the inline appendix")
	assertStagedAppendix(t, first, appendix)

	_, err := h.Restart(ChildSpec{}, time.Second)
	c.Require().NoError(err, "Restart")
	second := collectStdout(t, h, "--dangerously-skip-permissions", 5*time.Second)
	assertStagedAppendix(t, second, appendix)
}

// assertStagedAppendix reads the file the echo'd argv points its
// --append-system-prompt-file at and asserts it holds want.
func assertStagedAppendix(t *testing.T, out, want string) {
	t.Helper()
	content, err := os.ReadFile(stagedFile(t, out, "--append-system-prompt-file"))
	assert.NewAborting(t).False(err != nil || string(content) != want, "staged appendix = %q, %v; want %q", string(content), err, want)
}

// IsZero is the Restart reuse predicate. A struct comparison cannot be used:
// ChildSpec carries a slice and is no longer comparable.
func TestChildSpecIsZero(t *testing.T) {
	c := assert.NewCollecting(t)
	c.True((ChildSpec{}).IsZero(), "the zero ChildSpec should report IsZero()")
	c.False((ChildSpec{ExtraArgs: []string{"--foo"}}).IsZero(), "a ChildSpec with only ExtraArgs set should not report IsZero()")
}

// stdin reaches the process.
func TestHostWritesStdin(t *testing.T) {
	c := assert.NewAborting(t)
	h := NewHost(HostOptions{Binary: testChildBinary(t, "cat"), Spec: ChildSpec{Kind: KindClaude}})
	c.NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	c.NoError(h.WriteStdin([]byte("ping\n")), "WriteStdin")
	collectStdout(t, h, "ping", 5*time.Second)
}

// Restart replaces the process and announces the boundary IN the event stream,
// so a consumer holding per-process state knows exactly where to reset.
func TestHostRestartEmitsBoundaryMarker(t *testing.T) {
	c := assert.NewAborting(t)
	h := NewHost(HostOptions{
		Binary: testEchoBinary(t),
		Spec:   ChildSpec{Kind: KindClaude, Model: "first"},
	})
	c.NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()
	collectStdout(t, h, "first", 5*time.Second)
	oldPID := h.PID()

	newPID, err := h.Restart(ChildSpec{Kind: KindClaude, Model: "second"}, time.Second)
	c.NoError(err, "Restart")
	c.NotEq(oldPID, newPID, "pid unchanged across restart")

	// The marker must arrive, and it must arrive BEFORE the new process's bytes.
	deadline := time.After(5 * time.Second)
	sawMarker := false
	var got strings.Builder
	for {
		select {
		case ev := <-h.Events():
			switch {
			case ev.Restarted != nil:
				sawMarker = true
				c.Eq(newPID, *ev.Restarted, "marker pid")
			case len(ev.Stdout) > 0:
				got.Write(ev.Stdout)
				if strings.Contains(got.String(), "second") {
					if !sawMarker {
						t.Fatal("new process output arrived before the restart marker; " +
							"a consumer would fold it into the old process's state")
					}
					return
				}
			}
		case <-deadline:
			t.Fatalf("timeout waiting for restart marker + new output; got %q", got.String())
		}
	}
}

// A Restart with no spec must reuse the held one. The alternative — treating an
// absent spec as an empty one — would relaunch claude with no --output-format
// and no --resume: a running process that emits nothing parseable and has lost
// the conversation.
func TestRestartWithNoSpecReusesTheHeldOne(t *testing.T) {
	c := assert.NewCollecting(t)
	h := NewHost(HostOptions{
		Binary: testEchoBinary(t),
		Spec:   ChildSpec{Kind: KindClaude, Model: "m1", ResumeSession: "s1"},
	})
	c.Require().NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	_, err := h.Restart(ChildSpec{}, time.Second)
	c.Require().NoError(err, "Restart")

	h.mu.Lock()
	got := h.spec
	h.mu.Unlock()
	c.False(got.Model != "m1" || got.ResumeSession != "s1", "after a spec-less Restart the host holds %+v, want the original", got)
}

// Shutdown ends the process and reports how it went.
func TestHostShutdownReportsOutcome(t *testing.T) {
	c := assert.NewCollecting(t)
	h := NewHost(HostOptions{Binary: testEchoBinary(t), Spec: ChildSpec{Kind: KindClaude}})
	c.Require().NoError(h.Start(), "Start")

	_, sig, err := h.Shutdown(time.Second)
	c.Require().NoError(err, "Shutdown")
	c.NotEq("", sig, "signal is empty; a sleeping process must have been signalled")
	c.False(h.Running(), "Running is true after Shutdown")
}

// Shutdown with a chatty child and no consumer used to panic: Shutdown closed
// the event channel while the stdout pump was blocked sending into it.
// os.Process.Wait returns when the process is reaped, NOT when its pipes drain,
// so the pump is essentially always still live at that moment.
func TestShutdownWithBlockedPumpDoesNotPanic(t *testing.T) {
	c := assert.NewAborting(t)
	h := NewHost(HostOptions{
		Binary: testChildBinary(t, `while :; do echo x; done`),
		Spec:   ChildSpec{Kind: KindClaude},
	})
	c.NoError(h.Start(), "Start")
	time.Sleep(300 * time.Millisecond) // fill the buffer so the pump blocks

	_, _, err := h.Shutdown(time.Second)
	c.NoError(err, "Shutdown")
	time.Sleep(300 * time.Millisecond) // a surviving pump would panic here

	select {
	case <-h.Done():
	default:
		t.Fatal("Done did not close after Shutdown")
	}
}

// Restart used to emit the boundary marker while holding h.mu, so a full buffer
// with no consumer blocked every other method behind it — including Health.
func TestRestartDoesNotBlockOtherCallsOnASlowConsumer(t *testing.T) {
	h := NewHost(HostOptions{
		Binary: testChildBinary(t, `while :; do echo x; done`),
		Spec:   ChildSpec{Kind: KindClaude},
	})
	assert.NewAborting(t).NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()
	time.Sleep(300 * time.Millisecond)

	go func() { _, _ = h.Restart(ChildSpec{}, time.Second) }()

	done := make(chan struct{})
	go func() {
		time.Sleep(400 * time.Millisecond)
		h.PID()
		h.Running()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("PID/Running blocked behind Restart's marker send; the marker must " +
			"be emitted with h.mu released")
	}
}

// A child that dies on its own must say so: nothing else tells the consumer,
// and Running must stop claiming a process that is gone.
func TestUnexpectedExitEmitsExitedAndClearsRunning(t *testing.T) {
	c := assert.NewCollecting(t)
	h := NewHost(HostOptions{Binary: testChildBinary(t, "exit 7"), Spec: ChildSpec{Kind: KindClaude}})
	c.Require().NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.Events():
			if ev.Exited == nil {
				continue
			}
			c.Require().Eq(7, ev.Exited.ExitCode, "exit code")
			c.False(h.Running(), "Running is still true after the child exited on its own")
			return
		case <-deadline:
			t.Fatal("no Exited event for a child that exited on its own")
		}
	}
}

// A deliberate stop reports through Shutdown's return value and must NOT also
// arrive as an Exited event; the caller that asked does not need telling twice.
func TestDeliberateShutdownEmitsNoExitedEvent(t *testing.T) {
	c := assert.NewAborting(t)
	h := NewHost(HostOptions{Binary: testEchoBinary(t), Spec: ChildSpec{Kind: KindClaude}})
	c.NoError(h.Start(), "Start")
	_, _, err := h.Shutdown(time.Second)
	c.NoError(err, "Shutdown")
	select {
	case ev := <-h.Events():
		c.Nil(ev.Exited, "deliberate Shutdown also emitted an Exited event")
	default:
	}
}

// testShortLivedBinary returns a binary that exits immediately and
// successfully, standing in for a claude that dies on its own.
func testShortLivedBinary(t *testing.T) string {
	t.Helper()
	return "/usr/bin/true"
}

// A child that dies on its own must come back: when the controller's connection
// is also down, nothing else can restart it, and the alternative is a daraja
// hosting nothing.
func TestUnexpectedExitRespawnsTheChild(t *testing.T) {
	c := assert.NewCollecting(t)
	h := NewHost(HostOptions{
		Binary:         testShortLivedBinary(t),
		Spec:           ChildSpec{Kind: KindClaude},
		RespawnBackoff: time.Millisecond,
	})
	c.Require().NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	// The first exit is reported, then a replacement is announced.
	var sawExit, sawRestart bool
	deadline := time.After(10 * time.Second)
	for !sawRestart {
		select {
		case ev := <-h.Events():
			switch {
			case ev.Exited != nil:
				sawExit = true
			case ev.Restarted != nil:
				sawRestart = true
			}
		case <-deadline:
			t.Fatalf("timed out; sawExit=%v sawRestart=%v", sawExit, sawRestart)
		}
	}
	c.True(sawExit, "a respawn was announced without the exit that caused it")
}

// A child that dies instantly and forever — a bad --resume, a missing binary —
// must stop being respawned, or daraja forks at whatever rate the kernel
// allows for as long as it lives.
func TestRespawnStopsAtTheLimit(t *testing.T) {
	c := assert.NewCollecting(t)
	h := NewHost(HostOptions{
		Binary:         testShortLivedBinary(t),
		Spec:           ChildSpec{Kind: KindClaude},
		RespawnBackoff: time.Millisecond,
		RespawnLimit:   2,
	})
	c.Require().NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	var restarts int
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-h.Events():
			if ev.Restarted != nil {
				restarts++
				c.Require().LessOrEqual(2, restarts, "respawned")
			}
		case <-h.Done():
			c.Eq(2, restarts, "host finished after")
			return
		case <-deadline:
			t.Fatalf("host never gave up; restarts=%d", restarts)
		}
	}
}
