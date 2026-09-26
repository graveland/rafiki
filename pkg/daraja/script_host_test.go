package daraja

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/darajapb"
)

// A script child's exit is its result: no respawn, one Exited event, and the
// host finishes (so the daraja process exits with it). The respawn loop
// exists for claude — a process whose value is its CONTINUING conversation —
// and must never fire for a script, whose replacement would silently restart
// work whose outcome the consumer is about to settle.
func TestScriptExitDoesNotRespawn(t *testing.T) {
	// The executor resolves the script and hands daraja the interpreter (the
	// fake child here) plus the resolved argv (script path + args) positionally,
	// which arrive in ExtraArgs.
	bin := testChildBinary(t, `echo script-ran; echo boom >&2; exit 3`)
	h := NewHost(HostOptions{
		Binary: bin,
		Spec:   ChildSpec{Kind: KindScript, ExtraArgs: []string{bin}},
	})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.After(10 * time.Second)
	var sawStdout, sawStderr, sawExited bool
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("event channel closed unexpectedly")
			}
			if len(ev.Stdout) > 0 && strings.Contains(string(ev.Stdout), "script-ran") {
				sawStdout = true
			}
			if len(ev.Stderr) > 0 && strings.Contains(string(ev.Stderr), "boom") {
				sawStderr = true
			}
			if ev.Exited != nil {
				sawExited = true
				if ev.Exited.ExitCode != 3 || ev.Exited.Signal != "" {
					t.Fatalf("exit info = %+v, want code 3, no signal", ev.Exited)
				}
			}
			if ev.Restarted != nil {
				t.Fatalf("a script child was restarted: %+v", ev.Restarted)
			}
		case <-h.Done():
			if !sawStdout || !sawStderr || !sawExited {
				t.Fatalf("host finished with missing events: stdout=%v stderr=%v exited=%v",
					sawStdout, sawStderr, sawExited)
			}
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
	h := NewHost(HostOptions{
		Binary: testChildBinary(t, `echo out-first; echo err-line >&2; sleep 30`),
		Spec:   ChildSpec{Kind: KindClaude},
	})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _, _, _ = h.Shutdown(time.Second) }()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				t.Fatal("event channel closed unexpectedly")
			}
			if len(ev.Stdout) > 0 && strings.Contains(string(ev.Stdout), "out-first") {
				return // claude's stdout seen; the loop below watches for stderr
			}
			if len(ev.Stderr) > 0 {
				t.Fatalf("claude stderr was relayed: %q", ev.Stderr)
			}
		case <-deadline:
			t.Fatal("timeout waiting for the claude child's stdout")
		}
	}
}

// SpecFromProto must map the script variant — kind only. The script's argv is
// resolved by the EXECUTOR before daraja starts; the wire spec carries names,
// not paths, and nothing that can call Restart ever calls it for a script.
func TestSpecFromProtoMapsScriptKindWithoutArgv(t *testing.T) {
	got := SpecFromProto(&darajapb.ChildSpec{
		Kind: darajapb.Kind_KIND_SCRIPT,
		Script: &darajapb.ScriptParams{
			Repo:   "local",
			Script: "driver",
			Args:   []string{"--flag"},
		},
	})
	if got.Kind != KindScript {
		t.Fatalf("kind = %q, want %q", got.Kind, KindScript)
	}
	if len(got.ExtraArgs) != 0 {
		t.Fatalf("script spec mapped to argv %v; the executor resolves the argv, not the wire", got.ExtraArgs)
	}
	// Restarting INTO a kind-only script spec must fail rather than re-run the
	// script blind: a zero-spec Restart reuses the host's own resolved spec,
	// but an explicit script spec carries no resolved argv, and the empty
	// command line is startLocked's refusal.
	if argv := got.Argv("", nil); len(argv) != 0 {
		t.Fatalf("kind-only script spec produced argv %v; want empty", argv)
	}
}

// The done-drain guarantee, over a REAL relay stream: a script that exits
// before (or while) a consumer attaches still delivers its Exited — the
// consumer's Wait is the settle input, and an exit stranded in the host's
// event queue (select picks randomly between the ready done case and the
// ready events case) leaves the daemon-side child streaming forever. This is
// the integration failure mode the drain-on-done change exists to close.
func TestRelayDeliversQueuedEventsWhenTheHostIsDone(t *testing.T) {
	bin := testChildBinary(t, `echo final-line; exit 0`)
	h := NewHost(HostOptions{Binary: bin, Spec: ChildSpec{Kind: KindScript, ExtraArgs: []string{bin}}})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
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
	if err := stream.Send(&darajapb.RelayRequest{}); err != nil {
		t.Fatalf("open stream: %v", err)
	}

	deadline := time.After(10 * time.Second)
	var sawStdout, sawExited bool
	for !sawStdout || !sawExited {
		select {
		case <-deadline:
			t.Fatalf("relay ended without the full queue: stdout=%v exited=%v", sawStdout, sawExited)
		default:
		}
		resp, err := stream.Receive()
		if err != nil {
			t.Fatalf("stream ended before delivering the queue: stdout=%v exited=%v (%v)", sawStdout, sawExited, err)
		}
		switch {
		case strings.Contains(string(resp.GetStdout()), "final-line"):
			sawStdout = true
		case resp.GetExited() != nil:
			sawExited = true
			if resp.GetExited().GetExitCode() != 0 {
				t.Errorf("exited code = %d, want 0", resp.GetExited().GetExitCode())
			}
		}
	}
}

// A Restart RPC on a script host is refused up front, whatever the request's
// spec says: a script's exit is its result. The nil-spec case is the one that
// matters structurally — "reuse the spec I hold" would otherwise re-run the
// executor-resolved script whose outcome the consumer is settling.
func TestRestartRefusesAScriptHost(t *testing.T) {
	bin := testChildBinary(t, `sleep 30`)
	h := NewHost(HostOptions{Binary: bin, Spec: ChildSpec{Kind: KindScript, ExtraArgs: []string{bin}}})
	if err := h.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
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
			if connect.CodeOf(err) != connect.CodeFailedPrecondition {
				t.Fatalf("Restart err = %v, want FailedPrecondition", err)
			}
		})
	}
	// The hosted script was never signalled: it is still running.
	if !h.Running() {
		t.Fatal("the refused restart took the hosted script down")
	}
}
