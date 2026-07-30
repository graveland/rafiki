package inproc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.graveland.dev/brent/fundi/internal/agent"
)

// sampleEndTurn is one scripted assistant message: a completed turn whose text
// the test asserts on. Mirrors internal/agent/engine_test.go's fixture of the
// same name — that one is an unexported const in package agent, so it cannot be
// imported here.
const sampleEndTurn = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x",` +
	`"stop_reason":"end_turn","content":[{"type":"text","text":"the fake reply"}],` +
	`"usage":{"input_tokens":4,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`

// writeFakeTurns writes scripted assistant messages as one ndjson file and
// returns its path.
//
// Config.FakeTurns is a PATH to a newline-delimited-JSON file of
// anthropic.Message values (loaded by agent.LoadFakeSender) — NOT literal reply
// text. package agent has its own writeFakeTurns helper, but it is an
// unexported test helper in another package, so this test needs its own.
func writeFakeTurns(t *testing.T, bodies ...string) string {
	t.Helper()
	lines := make([]string, 0, len(bodies))
	for _, b := range bodies {
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(b)); err != nil {
			t.Fatalf("compact scripted body: %v", err)
		}
		lines = append(lines, compact.String())
	}
	path := filepath.Join(t.TempDir(), "turns.ndjson")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write scripted turns: %v", err)
	}
	return path
}

// readFramesUntil reads newline-delimited JSON frames from r until it sees one
// whose "type" equals want, or until r hits EOF. Returns every frame read.
func readFramesUntil(t *testing.T, r io.Reader, want string) []string {
	t.Helper()
	var out []string
	dec := json.NewDecoder(r)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return out
		}
		out = append(out, string(raw))
		var hdr struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &hdr); err != nil {
			continue // unparseable, but still recorded above
		}
		if hdr.Type == want {
			return out
		}
	}
}

// TestRunnerDrivesAFakeTurn proves a prompt written to the runner's stdin
// produces a real frame sequence on its stdout, with no subprocess involved.
//
// It asserts on the intermediate frames, not just the terminal one. This repo
// shipped empty message_update frames twice with a fully green suite, both
// times because every assertion targeted message_end.
func TestRunnerDrivesAFakeTurn(t *testing.T) {
	r := New(Options{
		ChildID: "c_inproc",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{
			Model:          "anthropic/claude-sonnet-4-5",
			Cwd:            t.TempDir(),
			Ref:            "c_inproc",
			SpillDir:       t.TempDir(),
			FakeTurns:      writeFakeTurns(t, sampleEndTurn),
			NoSkills:       true,
			NoContextFiles: true,
		},
	})

	stdin, stdout, stderr, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// stderr must be a live reader at EOF, never nil: readStderr would panic.
	if stderr == nil {
		t.Fatal("Start returned a nil stderr")
	}
	if _, err := io.ReadAll(stderr); err != nil {
		t.Errorf("stderr read: %v", err)
	}

	if _, err := stdin.Write([]byte(`{"type":"prompt","message":"hello"}` + "\n")); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	done := make(chan []string, 1)
	go func() { done <- readFramesUntil(t, stdout, "agent_end") }()

	var frames []string
	select {
	case frames = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("no agent_end frame within 15s")
	}

	joined := strings.Join(frames, "\n")
	for _, want := range []string{"agent_start", "message_end", "agent_end"} {
		if !strings.Contains(joined, want) {
			t.Errorf("frame stream missing %q; got:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "the fake reply") {
		t.Errorf("frame stream missing the fake turn text; got:\n%s", joined)
	}

	if err := stdin.Close(); err != nil {
		t.Errorf("close stdin: %v", err)
	}
	code, sig := r.Wait()
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if sig != "" {
		t.Errorf("signal = %q, want empty for an in-process runner", sig)
	}
	if r.PID() != 0 {
		t.Errorf("PID() = %d, want 0", r.PID())
	}
}

// TestRunnerContainsPanic is the load-bearing test of this phase. A panic in
// one conversation must become that child's exit, not the daemon's. Without the
// recover in run(), this test crashes the test binary rather than failing.
func TestRunnerContainsPanic(t *testing.T) {
	r := New(Options{
		ChildID: "c_panic",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{Cwd: t.TempDir()},
		Build: func(context.Context, *agent.Frontend, agent.RuntimeOptions) (*agent.Engine, func(), error) {
			panic("boom from the engine builder")
		},
	})

	_, stdout, _, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The panic must close stdout so the daemon's readStdout sees an ordinary
	// EOF and the normal child-exit path runs.
	if _, err := io.ReadAll(stdout); err != nil {
		t.Errorf("stdout should EOF cleanly after a contained panic, got: %v", err)
	}

	code, _ := r.Wait()
	if code == 0 {
		t.Error("exit code = 0 after a panic, want non-zero")
	}
}

// TestRunnerBuildErrorIsAnExit proves a failed build is reported as a non-zero
// exit rather than hanging the daemon's supervise loop forever.
func TestRunnerBuildErrorIsAnExit(t *testing.T) {
	r := New(Options{
		ChildID: "c_builderr",
		Parent:  t.Context(),
		Runtime: agent.RuntimeOptions{Cwd: "relative/path"}, // BuildRuntime rejects this
	})
	if _, stdout, _, err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	} else if _, rerr := io.ReadAll(stdout); rerr != nil {
		t.Errorf("stdout read: %v", rerr)
	}
	if code, _ := r.Wait(); code == 0 {
		t.Error("exit code = 0 after a build failure, want non-zero")
	}
}
