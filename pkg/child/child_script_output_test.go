// SPDX-License-Identifier: Apache-2.0

package child

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// A script child's output hook sees BOTH streams: every stdout line through
// handleFrame's script_output ParsedEvents, every stderr line split out of the
// stderr drain — while StderrSnapshot keeps buffering the raw bytes for the
// settle's tail. Non-script kinds leave the hook unset, so nothing else is
// affected; a claude child with the hook unset produces no hook calls at all
// (nothing to assert beyond the hook never firing — the claude suites cover
// those paths).
func TestScriptOutputHookSeesBothStreams(t *testing.T) {
	ck := assert.NewCollecting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: this test needs a real process")
	}

	var mu sync.Mutex
	var got []string

	c, err := Spawn(context.Background(), SpawnSpec{
		ChildID:  "c_script_hook",
		Cwd:      t.TempDir(),
		PiBinary: "/bin/sh",
		Argv:     []string{"-c", "echo out-1; echo err-1 >&2; echo out-2"},
		Provider: ScriptProvider{},
		OnScriptOutput: func(stream, line string, terminated bool) {
			mu.Lock()
			got = append(got, stream+"|"+line)
			mu.Unlock()
		},
	})
	ck.Require().NoError(err, "Spawn")
	t.Cleanup(func() { _, _ = c.Shutdown(time.Second, time.Second) })

	// readStderr is a goroutine of its own whose completion Done() does not
	// imply (see abandon) — poll for the lines rather than assuming order.
	waitForScriptLines(t, &mu, &got, []string{
		"stdout|out-1",
		"stdout|out-2",
		"stderr|err-1",
	})

	// The raw stderr buffer is untouched — the settle's tail still reads it.
	ck.StrContains(string(c.StderrSnapshot()), "err-1", "stderr buffer lost the raw line")
}

// A trailing stderr fragment without a newline is still delivered, mirroring
// FrameReader's partial-frame-at-EOF behavior on the stdout side.
func TestScriptOutputHookDeliversTrailingFragment(t *testing.T) {
	ck := assert.NewCollecting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: this test needs a real process")
	}

	var mu sync.Mutex
	var got []string

	c, err := Spawn(context.Background(), SpawnSpec{
		ChildID:  "c_script_frag",
		Cwd:      t.TempDir(),
		PiBinary: "/bin/sh",
		Argv:     []string{"-c", "printf 'no-newline-tail' >&2"},
		Provider: ScriptProvider{},
		OnScriptOutput: func(stream, line string, terminated bool) {
			mu.Lock()
			got = append(got, stream+"|"+line)
			mu.Unlock()
		},
	})
	ck.Require().NoError(err, "Spawn")
	t.Cleanup(func() { _, _ = c.Shutdown(time.Second, time.Second) })

	waitForScriptLines(t, &mu, &got, []string{"stderr|no-newline-tail"})
}

// waitForScriptLines polls until every wanted entry has been recorded.
func waitForScriptLines(t *testing.T, mu *sync.Mutex, got *[]string, want []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		joined := strings.Join(*got, "\n")
		mu.Unlock()
		missing := false
		for _, w := range want {
			if !strings.Contains(joined, w) {
				missing = true
				break
			}
		}
		if !missing {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("timed out waiting for hook lines %v; got %v", want, *got)
}
