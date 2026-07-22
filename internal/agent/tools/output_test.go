package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestClipSpillsAndElides is the brief's Step 1 test: an over-budget result
// is clipped to head+marker+tail, the marker names the spill path, and the
// FULL original output lands on disk at that path.
func TestClipSpillsAndElides(t *testing.T) {
	dir := t.TempDir()
	p := OutputPolicy{Budget: 1000, SpillDir: dir}
	long := strings.Repeat("A", 500) + strings.Repeat("B", 2000) + "VERDICT: fail\n"
	got := p.Clip(long, "tu_9")
	if len(got) > 1200 {
		t.Fatalf("clip too long: %d", len(got))
	}
	if !strings.HasPrefix(got, "AAAA") {
		t.Fatal("head missing")
	}
	if !strings.Contains(got, "VERDICT: fail") {
		t.Fatal("tail (the verdict) missing")
	}
	if !strings.Contains(got, filepath.Join(dir, "tu_9")) {
		t.Fatal("spill path missing from marker")
	}
	full, err := os.ReadFile(filepath.Join(dir, "tu_9"))
	if err != nil || string(full) != long {
		t.Fatal("full output not spilled")
	}
}

// TestClipUnderBudgetReturnsUnchanged verifies the "spill, never destroy"
// policy doesn't spill (or mutate) output that already fits the budget.
func TestClipUnderBudgetReturnsUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := OutputPolicy{Budget: 1000, SpillDir: dir}
	s := "short output\n"
	got := p.Clip(s, "tu_1")
	if got != s {
		t.Fatalf("expected unchanged output, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "tu_1")); !os.IsNotExist(err) {
		t.Fatalf("expected no spill file for under-budget output, stat err = %v", err)
	}
}

// TestClipZeroBudgetUsesDefault checks the documented default of 30_000
// bytes applies when Budget is left at its zero value.
func TestClipZeroBudgetUsesDefault(t *testing.T) {
	dir := t.TempDir()
	p := OutputPolicy{SpillDir: dir}
	s := strings.Repeat("z", 1000)
	got := p.Clip(s, "tu_2")
	if got != s {
		t.Fatalf("expected output under the 30_000 default budget to pass through unchanged, got %d bytes", len(got))
	}
}

// TestClipExactBudgetPassesThrough checks the boundary: output exactly at
// budget must not be clipped (the check is "unchanged when within budget").
func TestClipExactBudgetPassesThrough(t *testing.T) {
	dir := t.TempDir()
	p := OutputPolicy{Budget: 100, SpillDir: dir}
	s := strings.Repeat("x", 100)
	got := p.Clip(s, "tu_3")
	if got != s {
		t.Fatalf("expected exact-budget output unchanged, got %d bytes", len(got))
	}
}

// TestClipMarkerFormat pins the exact marker string from the brief, since
// downstream tooling (or an agent grepping its own output) may match on it.
func TestClipMarkerFormat(t *testing.T) {
	dir := t.TempDir()
	p := OutputPolicy{Budget: 100, SpillDir: dir}
	s := strings.Repeat("q", 500)
	got := p.Clip(s, "tu_4")
	wantPath := filepath.Join(dir, "tu_4")
	wantPrefix := "\n[... elided "
	if !strings.Contains(got, wantPrefix) {
		t.Fatalf("marker prefix missing, got %q", got)
	}
	wantSuffix := " bytes: full output at " + wantPath + " ...]\n"
	if !strings.Contains(got, wantSuffix) {
		t.Fatalf("marker suffix missing, got %q", got)
	}
}
