package main

import (
	"bytes"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/profile"
)

// captureStdout redirects the real os.Stdout for the duration of fn and
// returns what was written to it. Needed for commands that write to os.Stdout
// directly rather than through cmd.OutOrStdout().
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = wp
	fn()
	wp.Close()
	os.Stdout = old

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rp); err != nil {
		t.Fatal(err)
	}
	rp.Close()
	return buf.String()
}

// TestModelsCacheRewriteIgnoresFilters pins the models cache contract: the
// completion cache is rewritten from the FULL row set the daemon returned,
// whatever the display flags filter out of the table. --source narrows what
// `rafiki models` SHOWS; it must never narrow what completion OFFERS next.
func TestModelsCacheRewriteIgnoresFilters(t *testing.T) {
	localProfileForTest(t, profile.Profile{})
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	cmd := newModelsCmd()
	if err := cmd.Flags().Set("source", "openrouter"); err != nil {
		t.Fatal(err)
	}
	q, err := modelsQueryFromFlags(cmd)
	if err != nil {
		t.Fatalf("modelsQueryFromFlags: %v", err)
	}

	rows := modelTestRows() // builtin, openrouter and local sources
	out := captureStdout(t, func() {
		if err := reportModels(cmd, rows, q); err != nil {
			t.Fatalf("reportModels: %v", err)
		}
	})

	var ids []string
	if !cacheRead("models-fundi", completionEndpointKey(cmd), modelCacheTTL, &ids) {
		t.Fatal("models-fundi completion cache was not written")
	}
	want := []string{"anthropic/claude-opus-5", "openrouter/openai/gpt-4o", "vmlx/qwen"}
	if !slices.Equal(ids, want) {
		t.Errorf("cache ids = %v, want the FULL row set %v", ids, want)
	}

	// The render, by contrast, really is filtered to the openrouter row. The
	// MODEL cell shows the bare model part — the full id only returns with
	// --verbose — so presence is asserted on the bare id.
	if !strings.Contains(out, "openai/gpt-4o") {
		t.Errorf("--source openrouter dropped the openrouter row:\n%s", out)
	}
	for _, gone := range []string{"anthropic/claude-opus-5", "vmlx/qwen"} {
		if strings.Contains(out, gone) {
			t.Errorf("--source openrouter leaked %q into the table:\n%s", gone, out)
		}
	}
}
