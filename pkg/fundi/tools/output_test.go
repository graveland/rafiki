package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// TestClipSpillsAndElides is the brief's Step 1 test: an over-budget result
// is clipped to head+marker+tail, the marker names the spill path, and the
// FULL original output lands on disk at that path.
func TestClipSpillsAndElides(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := OutputPolicy{Budget: 1000, SpillDir: dir}
	long := strings.Repeat("A", 500) + strings.Repeat("B", 2000) + "VERDICT: fail\n"
	got := p.Clip(long, "tu_9")
	c.LessOrEqual(1200, len(got), "clip too long")
	if !strings.HasPrefix(got, "AAAA") {
		t.Fatal("head missing")
	}
	c.StrContains(got, "VERDICT: fail", "tail (the verdict) missing")
	c.StrContains(got, filepath.Join(dir, "tu_9"), "spill path missing from marker")
	full, err := os.ReadFile(filepath.Join(dir, "tu_9"))
	c.False(err != nil || string(full) != long, "full output not spilled")
}

// TestClipUnderBudgetReturnsUnchanged verifies the "spill, never destroy"
// policy doesn't spill (or mutate) output that already fits the budget.
func TestClipUnderBudgetReturnsUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := OutputPolicy{Budget: 1000, SpillDir: dir}
	s := "short output\n"
	got := p.Clip(s, "tu_1")
	c.Eq(s, got, "expected unchanged output, got")
	_, err := os.Stat(filepath.Join(dir, "tu_1"))
	c.True(os.IsNotExist(err), "expected no spill file for under-budget output, stat err = %v", err)
}

// TestClipZeroBudgetUsesDefault checks the documented default of 30_000
// bytes applies when Budget is left at its zero value.
func TestClipZeroBudgetUsesDefault(t *testing.T) {
	dir := t.TempDir()
	p := OutputPolicy{SpillDir: dir}
	s := strings.Repeat("z", 1000)
	got := p.Clip(s, "tu_2")
	assert.NewAborting(t).Eq(s, got, "expected output under the 30_000 default budget to pass through unchanged, got %d bytes", len(got))
}

// TestClipExactBudgetPassesThrough checks the boundary: output exactly at
// budget must not be clipped (the check is "unchanged when within budget").
func TestClipExactBudgetPassesThrough(t *testing.T) {
	dir := t.TempDir()
	p := OutputPolicy{Budget: 100, SpillDir: dir}
	s := strings.Repeat("x", 100)
	got := p.Clip(s, "tu_3")
	assert.NewAborting(t).Eq(s, got, "expected exact-budget output unchanged, got %d bytes", len(got))
}

// TestClipMarkerFormat pins the exact marker string from the brief, since
// downstream tooling (or an agent grepping its own output) may match on it.
func TestClipMarkerFormat(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := OutputPolicy{Budget: 100, SpillDir: dir}
	s := strings.Repeat("q", 500)
	got := p.Clip(s, "tu_4")
	wantPath := filepath.Join(dir, "tu_4")
	wantPrefix := "\n[... elided "
	c.StrContains(got, wantPrefix, "marker prefix missing, got")
	wantSuffix := " bytes: full output at " + wantPath + " ...]\n"
	c.StrContains(got, wantSuffix, "marker suffix missing, got")
}

// TestClipNameCannotEscapeSpillDir checks the spill name is confined to
// SpillDir. name comes from agentloop.ToolCallID — a provider-supplied
// string — so a value containing "/" or ".." must not let a tool result be
// written outside the spill directory.
func TestClipNameCannotEscapeSpillDir(t *testing.T) {
	c := assert.NewAborting(t)
	root := t.TempDir()
	dir := filepath.Join(root, "spill")
	p := OutputPolicy{Budget: 100, SpillDir: dir}
	s := strings.Repeat("e", 500)

	got := p.Clip(s, "../../escaped")

	safePath := filepath.Join(dir, "escaped")
	c.StrContains(got, safePath, "marker should point inside the spill dir, got")
	if _, err := os.Stat(safePath); err != nil {
		t.Fatalf("full output not spilled inside the spill dir: %v", err)
	}
	for _, escaped := range []string{
		filepath.Join(root, "escaped"),
		filepath.Join(filepath.Dir(root), "escaped"),
	} {
		_, err := os.Stat(escaped)
		c.True(os.IsNotExist(err), "output escaped the spill dir to %s (stat err = %v)", escaped, err)
	}
}

// TestClipDegenerateNameStillSpills checks a name that reduces to no file at
// all ("", ".", "..") still lands on a real file instead of targeting — and
// failing to write — the spill directory itself.
func TestClipDegenerateNameStillSpills(t *testing.T) {
	for _, name := range []string{"", ".", "..", "/"} {
		t.Run(fmt.Sprintf("name=%q", name), func(t *testing.T) {
			c := assert.NewAborting(t)
			dir := t.TempDir()
			p := OutputPolicy{Budget: 100, SpillDir: dir}
			s := strings.Repeat("d", 500)
			got := p.Clip(s, name)
			spillPath := filepath.Join(dir, "spill")
			c.StrContains(got, spillPath, "marker should name a real file, got")
			full, err := os.ReadFile(spillPath)
			c.NoError(err, "full output not spilled")
			c.Eq(s, string(full), "spilled content differs from input")
		})
	}
}

// TestClipZeroSpillDirFallsBackToTempDir checks a zero-value SpillDir does
// not degrade "spill, never destroy" into "destroy and log about it":
// os.MkdirAll("") fails, so without a fallback the write is skipped and the
// marker points at a bare filename that does not exist.
func TestClipZeroSpillDirFallsBackToTempDir(t *testing.T) {
	c := assert.NewAborting(t)
	name := fmt.Sprintf("fundi_clip_test_%d", time.Now().UnixNano())
	spillPath := filepath.Join(os.TempDir(), name)
	t.Cleanup(func() {
		if err := os.Remove(spillPath); err != nil && !os.IsNotExist(err) {
			t.Logf("cleanup: remove %s: %v", spillPath, err)
		}
	})

	p := OutputPolicy{Budget: 100}
	s := strings.Repeat("t", 500)
	got := p.Clip(s, name)

	c.StrContains(got, spillPath, "marker should point at the temp-dir fallback")
	full, err := os.ReadFile(spillPath)
	c.NoError(err, "full output not spilled with a zero-value SpillDir")
	c.Eq(s, string(full), "spilled content differs from input")
}

func TestClipBudgetTruncatesLongLines(t *testing.T) {
	c := assert.NewAborting(t)
	p := OutputPolicy{SpillDir: t.TempDir()}
	in := strings.Repeat("x", 100) + "\nshort\n"
	got := p.ClipBudget(in, "t", Budget{MaxLineChars: 10})
	for _, line := range strings.Split(got, "\n") {
		c.LessOrEqual(10+len(lineTruncSuffix), len(line), "line exceeds per-line cap: %q", line)
	}
	c.StrContains(got, "short", "short line was altered")
}

func TestClipBudgetCapsLines(t *testing.T) {
	c := assert.NewAborting(t)
	p := OutputPolicy{SpillDir: t.TempDir()}
	in := strings.Repeat("content\n", 50)
	got := p.ClipBudget(in, "t", Budget{MaxLines: 5})

	// Count content lines specifically. The previous version counted
	// occurrences of "line" across the whole result, so any wording in a
	// truncation marker was silently counted as a content line too.
	n := strings.Count(got, "content")
	c.Eq(5, n, "got %d content lines, want 5:\n%q", n, got)
	// The line cap must announce itself. A silent cut leaves the model
	// reasoning confidently about output it only partly saw — the rule
	// every other truncation path in this package follows.
	c.StrContains(got, linesTruncSuffix, "MaxLines truncation must be marked in the output, got:\n")
}

func TestClipBudgetZeroBudgetMatchesClip(t *testing.T) {
	p := OutputPolicy{SpillDir: t.TempDir()}
	in := strings.Repeat("y", defaultOutputBudget*2)
	assert.NewAborting(t).Eq(p.Clip(in, "t"), p.ClipBudget(in, "t", Budget{}), "zero Budget must behave exactly like Clip")
}
