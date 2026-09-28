// SPDX-License-Identifier: Apache-2.0

package child

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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

// R1/R2 regressions on the bounded fragment hand-off.
//
// R1: a newline-free run of ≥4096 continuation bytes (0x80–0xBF) has no rune
// boundary within 3 bytes of the cut. The step-back must be capped there and
// never deliver an empty fragment — uncapped, the loop cut to 0, delivered
// nothing, left pending untouched and spun forever (744k hook calls in 1 s,
// unbounded empty units, the script blocked on write).
func TestScriptOutputFragmentsOfContinuationBytesDoNotSpin(t *testing.T) {
	ck := assert.NewCollecting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: this test needs a real process")
	}

	var mu sync.Mutex
	var calls int
	var total int

	dir := t.TempDir()
	blob := bytes.Repeat([]byte{0x80}, 5000)
	blobPath := filepath.Join(dir, "blob")
	ck.NoError(os.WriteFile(blobPath, blob, 0o600), "write blob fixture")
	c, err := Spawn(context.Background(), SpawnSpec{
		ChildID:  "c_script_contspin",
		Cwd:      dir,
		PiBinary: "/bin/sh",
		Argv:     []string{"-c", "cat " + blobPath + " >&2"},
		Provider: ScriptProvider{},
		OnScriptOutput: func(stream, line string, terminated bool) {
			mu.Lock()
			calls++
			total += len(line)
			mu.Unlock()
		},
	})
	ck.Require().NoError(err, "Spawn")
	t.Cleanup(func() { _, _ = c.Shutdown(time.Second, time.Second) })

	// Wait for the whole blob to drain through the hook — a spinning reader
	// would never get there.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := total >= len(blob)
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	ck.True(total >= len(blob), "the drain stalled (spin): %d of %d bytes delivered after 10 s, %d calls", total, len(blob), calls)
	ck.False(calls > 1000, "hook fired %d times for %d bytes: empty-fragment spin", calls, len(blob))
}

// R2: the rune cut must check the byte AT the cut (the coalescer's rule), so
// valid multi-byte stderr is never split mid-rune: every fragment is valid
// UTF-8 and the fragments concatenate back to the input.
func TestScriptOutputFragmentsKeepMultiByteRunesIntact(t *testing.T) {
	ck := assert.NewCollecting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: this test needs a real process")
	}

	var mu sync.Mutex
	var frags []string
	var termFrags int

	dir := t.TempDir()
	// "ab" + 3000 × 中 = 9002 bytes, no newline: the "ab" prefix puts a 3-byte
	// rune's lead at cut-2 (4094) with a continuation at the exact bound, which
	// an only-last-lead guard misses. Two full fragments plus an unterminated
	// tail (which arrives terminated=true at EOF).
	blob := "ab" + strings.Repeat("中", 3000)
	blobPath := filepath.Join(dir, "blob")
	ck.NoError(os.WriteFile(blobPath, []byte(blob), 0o600), "write blob fixture")
	c, err := Spawn(context.Background(), SpawnSpec{
		ChildID:  "c_script_cjkfrag",
		Cwd:      dir,
		PiBinary: "/bin/sh",
		Argv:     []string{"-c", "cat " + blobPath + " >&2"},
		Provider: ScriptProvider{},
		OnScriptOutput: func(stream, line string, terminated bool) {
			mu.Lock()
			frags = append(frags, line)
			if terminated {
				termFrags++
			}
			mu.Unlock()
		},
	})
	ck.Require().NoError(err, "Spawn")
	t.Cleanup(func() { _, _ = c.Shutdown(time.Second, time.Second) })

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(frags) > 0 && len(strings.Join(frags, "")) >= len(blob)
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	ck.Require().True(len(frags) >= 2, "got %d fragments for %d bytes", len(frags), len(blob))
	var joined strings.Builder
	for i, f := range frags {
		joined.WriteString(f)
		ck.True(utf8.ValidString(f), "fragment %d is invalid UTF-8 (cut mid-rune): len %d, head=% x tail=% x", i, len(f), f[:min(6, len(f))], f[max(0, len(f)-6):])
	}
	ck.Eq(blob, joined.String(), "fragments do not concatenate back to the input")
	ck.True(termFrags >= 1, "the trailing partial fragment was never delivered terminated")
}

// N2: a 4-byte rune's lead sitting at cut-1 with continuations across the
// exact bound must not split mid-rune either — a 1-byte emoji prefix puts a
// lead at 4093. Every fragment is valid UTF-8 and the fragments concatenate
// back to the input.
func TestScriptOutputFragmentsKeepEmojiRunesIntact(t *testing.T) {
	ck := assert.NewCollecting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: this test needs a real process")
	}

	var mu sync.Mutex
	var frags []string
	var termFrags int

	dir := t.TempDir()
	// "a" + 2000 × 😀 = 8001 bytes, no newline: the lead of the emoji that
	// straddles the bound sits at 4093, two continuations from the cut.
	blob := "a" + strings.Repeat("\U0001F600", 2000)
	blobPath := filepath.Join(dir, "blob")
	ck.NoError(os.WriteFile(blobPath, []byte(blob), 0o600), "write blob fixture")
	c, err := Spawn(context.Background(), SpawnSpec{
		ChildID:  "c_script_emojifrag",
		Cwd:      dir,
		PiBinary: "/bin/sh",
		Argv:     []string{"-c", "cat " + blobPath + " >&2"},
		Provider: ScriptProvider{},
		OnScriptOutput: func(stream, line string, terminated bool) {
			mu.Lock()
			frags = append(frags, line)
			if terminated {
				termFrags++
			}
			mu.Unlock()
		},
	})
	ck.Require().NoError(err, "Spawn")
	t.Cleanup(func() { _, _ = c.Shutdown(time.Second, time.Second) })

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(frags) > 0 && len(strings.Join(frags, "")) >= len(blob)
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	ck.Require().True(len(frags) >= 2, "got %d fragments for %d bytes", len(frags), len(blob))
	var joined strings.Builder
	for i, f := range frags {
		joined.WriteString(f)
		ck.True(utf8.ValidString(f), "fragment %d is invalid UTF-8 (cut mid-rune): len %d, head=% x tail=% x", i, len(f), f[:min(6, len(f))], f[max(0, len(f)-6):])
	}
	ck.Eq(blob, joined.String(), "fragments do not concatenate back to the input")
	ck.True(termFrags >= 1, "the trailing partial fragment was never delivered terminated")
}
