package child

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/protocol"
)

// writeFakeClaude writes a bash script that mimics claude's stream-json stdio:
// emit system/init immediately, then for every stdin line emit an assistant
// line followed by a result line. It also appends each received stdin line to
// $CAPTURE so the test can assert the outbound encoding.
func writeFakeClaude(t *testing.T, capturePath string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "fakeclaude.sh")
	body := `#!/bin/bash
printf '%s\n' '{"type":"system","subtype":"init","session_id":"sess-int","model":"claude-opus-4-8"}'
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$CAPTURE"
  printf '%s\n' '{"type":"assistant","session_id":"sess-int","message":{"content":[{"type":"text","text":"ok"}]}}'
  printf '%s\n' '{"type":"result","subtype":"success","session_id":"sess-int"}'
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	_ = capturePath
	return script
}

func TestClaudeChild_EndToEnd(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "capture.txt")
	bin := writeFakeClaude(t, capture)

	cwd, _ := os.Getwd()
	ch, err := Spawn(context.Background(), SpawnSpec{
		ChildID:  "c_test",
		Cwd:      cwd,
		PiBinary: bin,
		Env:      []string{"CAPTURE=" + capture},
		Provider: ClaudeProvider{},
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	t.Cleanup(func() { _, _ = ch.Shutdown(time.Second, time.Second) })

	// system/init must drive spawning→idle and close Idle().
	select {
	case <-ch.Idle():
	case <-time.After(3 * time.Second):
		t.Fatal("child never became idle from system/init")
	}
	if got := ch.Metadata().SessionID; got != "sess-int" {
		t.Fatalf("session id = %q, want sess-int", got)
	}
	if st := ch.Status(); st != protocol.StatusIdle {
		t.Fatalf("status after init = %q, want idle", st)
	}

	// Send a normalized prompt; the provider must encode it as a claude user
	// envelope, and the assistant→result sequence must return the child to idle.
	if err := ch.Send([]byte(`{"type":"prompt","message":"hi"}`)); err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ch.Status() == protocol.StatusIdle {
			b, _ := os.ReadFile(capture)
			if strings.Contains(string(b), `"type":"user"`) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	b, _ := os.ReadFile(capture)
	line := strings.TrimSpace(string(b))
	if line == "" {
		t.Fatal("fake claude received no stdin frame")
	}
	var env struct {
		Type    string `json:"type"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.Split(line, "\n")[0]), &env); err != nil {
		t.Fatalf("captured frame not JSON: %v (%q)", err, line)
	}
	if env.Type != "user" || env.Message.Content != "hi" {
		t.Fatalf("outbound frame = %q, want a claude user envelope with content 'hi'", line)
	}
}
