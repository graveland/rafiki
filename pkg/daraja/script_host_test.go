package daraja

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/darajapb"

	"github.com/multigres/testkit/assert"
)

// A script child's exit is its result: no respawn, one Exited event, and the
// host finishes (so the daraja process exits with it). The respawn loop
// exists for claude — a process whose value is its CONTINUING conversation —
// and must never fire for a script, whose replacement would silently restart
// work whose outcome the consumer is about to settle.
func TestScriptExitDoesNotRespawn(t *testing.T) {
	c := assert.NewAborting(t)
	// The executor resolves the script and hands daraja the interpreter (the
	// fake child here) plus the resolved argv (script path + args) positionally,
	// which arrive in ExtraArgs.
	bin := testChildBinary(t, `echo script-ran; echo boom >&2; exit 3`)
	h := NewHost(HostOptions{
		Binary: bin,
		Spec:   ChildSpec{Kind: KindScript, ExtraArgs: []string{bin}},
	})
	c.NoError(h.Start(), "Start")

	deadline := time.After(10 * time.Second)
	var sawStdout, sawStderr, sawExited bool
	for {
		select {
		case ev, ok := <-h.Events():
			c.True(ok, "event channel closed unexpectedly")
			if len(ev.Stdout) > 0 && strings.Contains(string(ev.Stdout), "script-ran") {
				sawStdout = true
			}
			if len(ev.Stderr) > 0 && strings.Contains(string(ev.Stderr), "boom") {
				sawStderr = true
			}
			if ev.Exited != nil {
				sawExited = true
				c.False(ev.Exited.ExitCode != 3 || ev.Exited.Signal != "", "exit info = %+v, want code 3, no signal", ev.Exited)
			}
			c.Nil(ev.Restarted, "a script child was restarted")
		case <-h.Done():
			c.False(!sawStdout || !sawStderr || !sawExited, "host finished with missing events: stdout=%v stderr=%v exited=%v", sawStdout, sawStderr, sawExited)
			return
		case <-deadline:
			t.Fatalf("timeout: stdout=%v stderr=%v exited=%v done=%v",
				sawStdout, sawStderr, sawExited, h.Done())
		}
	}
}

// The stderr relay is script-only: claude's stderr stays drained-and-discarded
// (its protocol is stdout; its engine chatter is noise the consumer cannot
// use), so a claude event stream must never grow a stderr event.
func TestClaudeStderrIsStillDiscarded(t *testing.T) {
	c := assert.NewAborting(t)
	h := NewHost(HostOptions{
		Binary: testChildBinary(t, `echo out-first; echo err-line >&2; sleep 30`),
		Spec:   ChildSpec{Kind: KindClaude},
	})
	c.NoError(h.Start(), "Start")
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			c.True(ok, "event channel closed unexpectedly")
			if len(ev.Stdout) > 0 && strings.Contains(string(ev.Stdout), "out-first") {
				return // claude's stdout seen; the loop below watches for stderr
			}
			c.LessOrEqual(0, len(ev.Stderr), "claude stderr was relayed: %q", ev.Stderr)
		case <-deadline:
			t.Fatal("timeout waiting for the claude child's stdout")
		}
	}
}

// SpecFromProto must map the script variant — kind only. The script's argv is
// resolved by the EXECUTOR before daraja starts; the wire spec carries names,
// not paths, and nothing that can call Restart ever calls it for a script.
func TestSpecFromProtoMapsScriptKindWithoutArgv(t *testing.T) {
	c := assert.NewAborting(t)
	got := SpecFromProto(&darajapb.ChildSpec{
		Kind: darajapb.Kind_KIND_SCRIPT,
		Script: &darajapb.ScriptParams{
			Repo:   "local",
			Script: "driver",
			Args:   []string{"--flag"},
		},
	})
	c.Eq(KindScript, got.Kind, "kind")
	c.Empty(got.ExtraArgs, "script spec mapped to argv")
	// Restarting INTO a kind-only script spec must fail rather than re-run the
	// script blind: a zero-spec Restart reuses the host's own resolved spec,
	// but an explicit script spec carries no resolved argv, and the empty
	// command line is startLocked's refusal.
	c.Empty(got.Argv("", nil), "kind-only script spec produced argv")
}

// The done-drain guarantee, over a REAL relay stream: a script that exits
// before (or while) a consumer attaches still delivers its Exited — the
// consumer's Wait is the settle input, and an exit stranded in the host's
// event queue (select picks randomly between the ready done case and the
// ready events case) leaves the daemon-side child streaming forever. This is
// the integration failure mode the drain-on-done change exists to close.
func TestRelayDeliversQueuedEventsWhenTheHostIsDone(t *testing.T) {
	ck := assert.NewCollecting(t)
	bin := testChildBinary(t, `echo final-line; exit 0`)
	h := NewHost(HostOptions{Binary: bin, Spec: ChildSpec{Kind: KindScript, ExtraArgs: []string{bin}}})
	ck.Require().NoError(h.Start(), "Start")
	// The script exits long before any relay attaches: everything it emitted
	// (and its exit) is queued in the host's channel, done is closed.
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the host never finished after the script exited")
	}

	c, _ := newTestServer(t, h)
	stream := c.Relay(context.Background())
	// A bidi stream's request goes out with the first Send (the pool's relay
	// holder opens the same way, with a nil payload): without it the server
	// handler never starts and Receive blocks on a request that was never
	// made.
	ck.Require().NoError(stream.Send(&darajapb.RelayRequest{}), "open stream")

	deadline := time.After(10 * time.Second)
	var sawStdout, sawExited bool
	for !sawStdout || !sawExited {
		select {
		case <-deadline:
			t.Fatalf("relay ended without the full queue: stdout=%v exited=%v", sawStdout, sawExited)
		default:
		}
		resp, err := stream.Receive()
		ck.Require().NoError(err, "stream ended before delivering the queue: stdout=%v exited=%v (%v)", sawStdout, sawExited, err)
		switch {
		case strings.Contains(string(resp.GetStdout()), "final-line"):
			sawStdout = true
		case resp.GetExited() != nil:
			sawExited = true
			ck.Eq(0, resp.GetExited().GetExitCode(), "exited code")
		}
	}
}

// A Restart RPC on a script host is refused up front, whatever the request's
// spec says: a script's exit is its result. The nil-spec case is the one that
// matters structurally — "reuse the spec I hold" would otherwise re-run the
// executor-resolved script whose outcome the consumer is settling.
func TestRestartRefusesAScriptHost(t *testing.T) {
	c := assert.NewAborting(t)
	bin := testChildBinary(t, `sleep 30`)
	h := NewHost(HostOptions{Binary: bin, Spec: ChildSpec{Kind: KindScript, ExtraArgs: []string{bin}}})
	c.NoError(h.Start(), "Start")
	t.Cleanup(func() { _, _, _ = h.Shutdown(time.Second) })
	srv := NewServer(h)

	for _, tc := range []struct {
		name string
		spec *darajapb.ChildSpec
	}{
		{"nil spec (reuse what you hold)", nil},
		{"explicit script spec", &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_SCRIPT, Script: &darajapb.ScriptParams{Repo: "local", Script: "driver"}}},
		{"claude spec", &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE, Claude: &darajapb.ClaudeParams{Model: "m"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := srv.Restart(context.Background(), connect.NewRequest(&darajapb.RestartRequest{Spec: tc.spec}))
			assert.NewAborting(t).Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "Restart err = %v, want FailedPrecondition", err)
		})
	}
	// The hosted script was never signalled: it is still running.
	c.True(h.Running(), "the refused restart took the hosted script down")
}

// A hosted script's LAST output must reach the consumer BEFORE Exited: the
// host's watch drains both pumps (bounded by pumpDrainGrace) after the process
// is reaped and before emitting Exited. Without that wait, the pumps may still
// hold unread pipe data when Exited is emitted — and a relay consumer stops
// reading on Exited, so a failing script's traceback is lost from both the
// ScriptOutput events and the settle's stderr tail.
func TestScriptLastOutputArrivesBeforeExited(t *testing.T) {
	c := assert.NewAborting(t)
	// ~64 KiB on each stream, then exit — more than the OS pipe buffer, so the
	// pumps genuinely lag the reap.
	script := `head -c 65536 /dev/zero | tr '\0' 'o' >&2; head -c 65536 /dev/zero | tr '\0' 's'; exit 0`
	bin := testChildBinary(t, script)
	h := NewHost(HostOptions{Binary: bin, Spec: ChildSpec{Kind: KindScript, ExtraArgs: []string{bin}}})
	c.NoError(h.Start(), "Start")

	deadline := time.After(10 * time.Second)
	var out, errb []byte
	var sawExited bool
	for !sawExited {
		select {
		case ev, ok := <-h.Events():
			c.True(ok, "event channel closed unexpectedly")
			out = append(out, ev.Stdout...)
			errb = append(errb, ev.Stderr...)
			if ev.Exited != nil {
				sawExited = true
			}
		case <-deadline:
			t.Fatalf("timeout: out=%d err=%d exited=%v", len(out), len(errb), sawExited)
		}
	}
	// drain-on-done: after Exited the host still finishes; keep reading the
	// queue so nothing is left behind.
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return
			}
			out = append(out, ev.Stdout...)
			errb = append(errb, ev.Stderr...)
		case <-time.After(2 * time.Second):
			goto drained
		}
	}
drained:
	c.True(len(errb) >= 65536, "stderr lost bytes: %d of 65536 arrived before Exited", len(errb))
	c.True(len(out) >= 65536, "stdout lost bytes: %d of 65536 arrived before Exited", len(out))
	c.True(strings.Count(string(errb), "o") == len(errb), "stderr corrupted")
	c.True(strings.Count(string(out), "s") == len(out), "stdout corrupted")
}
