package main

import (
	"bytes"
	"io"
	"os"
	"testing"

	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// captureStdout redirects the real os.Stdout for the duration of fn and
// returns what was written to it. Needed for commands that write to os.Stdout
// directly rather than through cmd.OutOrStdout().
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	rp, wp, err := os.Pipe()
	assert.NewAborting(t).NoError(err)
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
	c := assert.NewCollecting(t)
	localProfileForTest(t, profile.Profile{})
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	cmd := newModelsCmd()
	c.Require().NoError(cmd.Flags().Set("source", "openrouter"))
	q, err := modelsQueryFromFlags(cmd)
	c.Require().NoError(err, "modelsQueryFromFlags")

	rows := modelTestRows() // builtin, openrouter and local sources
	out := captureStdout(t, func() {
		c.Require().NoError(reportModels(cmd, rows, q), "reportModels")
	})

	var ids []string
	c.Require().True(cacheRead("models-fundi", completionEndpointKey(cmd), modelCacheTTL, &ids), "models-fundi completion cache was not written")
	want := []string{"anthropic/claude-opus-5", "openrouter/openai/gpt-4o", "vmlx/qwen"}
	c.EqDiff(want, ids, "cache ids")

	// The render, by contrast, really is filtered to the openrouter row. The
	// MODEL cell shows the bare model part — the full id only returns with
	// --verbose — so presence is asserted on the bare id.
	c.StrContains(out, "openai/gpt-4o", "--source openrouter dropped the openrouter row:\n")
	for _, gone := range []string{"anthropic/claude-opus-5", "vmlx/qwen"} {
		c.NotStrContains(out, gone, "--source openrouter leaked")
	}
}
