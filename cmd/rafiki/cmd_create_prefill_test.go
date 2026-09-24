// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/protocol"
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
// calls BEFORE mustDial, so a bad list is refused without opening a
// connection. The helper dials nothing, which is what makes these subtests
// possible without a daemon.
func TestCreatePrefillFlag(t *testing.T) {
	t.Run("file becomes entries", func(t *testing.T) {
		dir := t.TempDir()
		list := filepath.Join(dir, "prefill.txt")
		content := "# context for the task\n" +
			"CLAUDE.md:10-40\n" +
			"\n" +
			"  src/**/*.rs  \n" +
			"docs/design.md:200-\n"
		if err := os.WriteFile(list, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := newPrefillTestCmd()
		if err := cmd.Flags().Set("prefill-files", list); err != nil {
			t.Fatal(err)
		}

		got, err := resolvePrefillFiles(cmd)
		if err != nil {
			t.Fatalf("resolvePrefillFiles: %v", err)
		}
		want := []protocol.PrefillRead{
			{Path: "CLAUDE.md", Start: 10, End: 40},
			{Path: "src/**/*.rs"},
			{Path: "docs/design.md", Start: 200},
		}
		if !slices.Equal(got, want) {
			t.Errorf("entries = %+v, want %+v", got, want)
		}
	})

	t.Run("unset flag is nil", func(t *testing.T) {
		cmd := newPrefillTestCmd()
		got, err := resolvePrefillFiles(cmd)
		if err != nil {
			t.Fatalf("resolvePrefillFiles: %v", err)
		}
		if got != nil {
			t.Errorf("entries = %+v, want nil", got)
		}
	})

	t.Run("stdin without detached errors", func(t *testing.T) {
		cmd := newPrefillTestCmd()
		if err := cmd.Flags().Set("prefill-files", "-"); err != nil {
			t.Fatal(err)
		}

		_, err := resolvePrefillFiles(cmd)
		if err == nil {
			t.Fatal("want an error for '-' without --detached, got none")
		}
		if got := err.Error(); got != "--prefill-files -: stdin is only available with --detached" {
			t.Errorf("error = %q, want the exact refused-stdin message", got)
		}
	})

	t.Run("stdin with detached reads stdin", func(t *testing.T) {
		cmd := newPrefillTestCmd()
		if err := cmd.Flags().Set("prefill-files", "-"); err != nil {
			t.Fatal(err)
		}
		if err := cmd.Flags().Set("detached", "true"); err != nil {
			t.Fatal(err)
		}

		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.WriteString("README.md:1-20\n"); err != nil {
			t.Fatal(err)
		}
		w.Close()
		oldStdin := os.Stdin
		os.Stdin = r
		defer func() { os.Stdin = oldStdin }()

		got, err := resolvePrefillFiles(cmd)
		if err != nil {
			t.Fatalf("resolvePrefillFiles: %v", err)
		}
		want := []protocol.PrefillRead{{Path: "README.md", Start: 1, End: 20}}
		if !slices.Equal(got, want) {
			t.Errorf("entries = %+v, want %+v", got, want)
		}
	})

	t.Run("parse error surfaces", func(t *testing.T) {
		dir := t.TempDir()
		list := filepath.Join(dir, "bad.txt")
		if err := os.WriteFile(list, []byte("notes.txt:5-2\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := newPrefillTestCmd()
		if err := cmd.Flags().Set("prefill-files", list); err != nil {
			t.Fatal(err)
		}

		_, err := resolvePrefillFiles(cmd)
		if err == nil {
			t.Fatal("want a parse error for a reversed range, got none")
		}
		if !strings.Contains(err.Error(), "prefill: line 1") {
			t.Errorf("error = %q, want the parser's line-1 diagnostic", err)
		}
	})

	t.Run("missing file names the flag", func(t *testing.T) {
		cmd := newPrefillTestCmd()
		if err := cmd.Flags().Set("prefill-files", filepath.Join(t.TempDir(), "absent.txt")); err != nil {
			t.Fatal(err)
		}

		_, err := resolvePrefillFiles(cmd)
		if err == nil {
			t.Fatal("want an error for a missing list file, got none")
		}
		if !strings.HasPrefix(err.Error(), "--prefill-files ") {
			t.Errorf("error = %q, want it to name the flag", err)
		}
	})
}
