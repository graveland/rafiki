// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// newPrefillTestCmd returns a create-shaped command with the two flags
// resolvePrefillFiles reads. newTestCreateCmd covers the spawn flags; --prefill-files
// and --detached live on the create command itself.
func newPrefillTestCmd() *cobra.Command {
	cmd := newTestCreateCmd()
	cmd.Flags().BoolP("detached", "d", false, "Spawn without attaching; the child runs in the background")
	cmd.Flags().String("prefill-files", "", "")
	return cmd
}

// TestCreatePrefillFlag exercises resolvePrefillFiles — the helper runCreate
// calls BEFORE any connection, so a bad list is refused without opening one.
// The helper dials nothing, which is what makes these subtests possible
// without a daemon.
func TestCreatePrefillFlag(t *testing.T) {
	t.Run("file becomes entries", func(t *testing.T) {
		c := assert.NewCollecting(t)
		dir := t.TempDir()
		list := filepath.Join(dir, "prefill.txt")
		content := "# context for the task\n" +
			"CLAUDE.md:10-40\n" +
			"\n" +
			"  src/**/*.rs  \n" +
			"docs/design.md:200-\n"
		c.Require().NoError(os.WriteFile(list, []byte(content), 0o644))
		cmd := newPrefillTestCmd()
		c.Require().NoError(cmd.Flags().Set("prefill-files", list))

		got, err := resolvePrefillFiles(cmd)
		c.Require().NoError(err, "resolvePrefillFiles")
		want := []protocol.PrefillRead{
			{Path: "CLAUDE.md", Start: 10, End: 40},
			{Path: "src/**/*.rs"},
			{Path: "docs/design.md", Start: 200},
		}
		c.EqDiff(want, got, "entries")
	})

	t.Run("unset flag is nil", func(t *testing.T) {
		c := assert.NewCollecting(t)
		cmd := newPrefillTestCmd()
		got, err := resolvePrefillFiles(cmd)
		c.Require().NoError(err, "resolvePrefillFiles")
		c.Nil(got, "entries")
	})

	t.Run("stdin without detached errors", func(t *testing.T) {
		c := assert.NewCollecting(t)
		cmd := newPrefillTestCmd()
		c.Require().NoError(cmd.Flags().Set("prefill-files", "-"))

		_, err := resolvePrefillFiles(cmd)
		c.Require().Error(err, "want an error for '-' without --detached, got none")
		c.Eq("--prefill-files -: stdin is only available with --detached", err.Error(), "error")
	})

	t.Run("stdin with detached reads stdin", func(t *testing.T) {
		c := assert.NewCollecting(t)
		cmd := newPrefillTestCmd()
		c.Require().NoError(cmd.Flags().Set("prefill-files", "-"))
		c.Require().NoError(cmd.Flags().Set("detached", "true"))

		r, w, err := os.Pipe()
		c.Require().NoError(err)
		if _, err := w.WriteString("README.md:1-20\n"); err != nil {
			t.Fatal(err)
		}
		w.Close()
		oldStdin := os.Stdin
		os.Stdin = r
		defer func() { os.Stdin = oldStdin }()

		got, err := resolvePrefillFiles(cmd)
		c.Require().NoError(err, "resolvePrefillFiles")
		want := []protocol.PrefillRead{{Path: "README.md", Start: 1, End: 20}}
		c.EqDiff(want, got, "entries")
	})

	t.Run("parse error surfaces", func(t *testing.T) {
		c := assert.NewCollecting(t)
		dir := t.TempDir()
		list := filepath.Join(dir, "bad.txt")
		c.Require().NoError(os.WriteFile(list, []byte("notes.txt:5-2\n"), 0o644))
		cmd := newPrefillTestCmd()
		c.Require().NoError(cmd.Flags().Set("prefill-files", list))

		_, err := resolvePrefillFiles(cmd)
		c.Require().Error(err, "want a parse error for a reversed range, got none")
		c.StrContains(err.Error(), "prefill: line 1", "error = %q, want the parser's line-1 diagnostic", err)
	})

	t.Run("missing file names the flag", func(t *testing.T) {
		c := assert.NewAborting(t)
		cmd := newPrefillTestCmd()
		c.NoError(cmd.Flags().Set("prefill-files", filepath.Join(t.TempDir(), "absent.txt")))

		_, err := resolvePrefillFiles(cmd)
		c.Error(err, "want an error for a missing list file, got none")
		if !strings.HasPrefix(err.Error(), "--prefill-files ") {
			t.Errorf("error = %q, want it to name the flag", err)
		}
	})
}

// -i opens the create form, and the form cannot carry a pre-fill (pkg/tui's
// SpawnRequest has no such field) — so the combination used to parse the list
// and then silently drop it. The flags are mutually exclusive at parse time.
func TestPrefillFilesInteractiveExclusive(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newCreateCmd()
	c.Require().NoError(cmd.Flags().Set("interactive", "true"))
	c.Require().NoError(cmd.Flags().Set("prefill-files", "list.txt"))

	err := cmd.ValidateFlagGroups()
	c.Require().Error(err, "want -i and --prefill-files to be rejected together, got none")
	c.False(!strings.Contains(err.Error(), "interactive") || !strings.Contains(err.Error(), "prefill-files"), "error = %q, want it to name both flags", err)
}
