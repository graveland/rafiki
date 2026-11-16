package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/multigres/testkit/assert"
)

// daraja is a SUBCOMMAND of rafiki, not a third binary: this repo ships exactly
// two artifacts and cmd/rafiki-executor was deleted to keep it that way.
func TestDarajaIsRegisteredOnRoot(t *testing.T) {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if strings.HasPrefix(c.Use, "daraja") {
			return
		}
	}
	t.Fatal("daraja is not registered on the root command")
}

func TestDarajaServeRequiresConnectAndBinary(t *testing.T) {
	ck := assert.NewCollecting(t)
	cmd := newDarajaCmd()
	var serve *cobra.Command
	for _, c := range cmd.Commands() {
		if c.Use == "serve" {
			serve = c
			break
		}
	}
	ck.Require().NotNil(serve, "darja has no `serve` subcommand")
	for _, flag := range []string{"connect-socket", "binary", "child-id", "append-system-prompt", "append-system-prompt-file"} {
		ck.NotNil(serve.Flags().Lookup(flag), "daraja serve is missing the --%s flag", flag)
	}
	// The old --socket flag must be gone.
	ck.Nil(serve.Flags().Lookup("socket"), "daraja serve still has the deprecated --socket flag")
}

// resolveAppendPrompt picks the appendix from whichever flag was given — the
// executor stages it to a file and passes --append-system-prompt-file so the
// text never rides the host process's ps-visible argv; a hand-run
// `rafiki daraja serve` may still pass it inline. Each row fails if its branch
// is deleted.
func TestResolveAppendPrompt(t *testing.T) {
	c := assert.NewCollecting(t)
	file := filepath.Join(t.TempDir(), "appendix.md")
	c.Require().NoError(os.WriteFile(file, []byte("from a file"), 0o600), "seed file")

	tests := []struct {
		name         string
		inline, file string
		want         string
	}{
		{"inline only", "from argv", "", "from argv"},
		{"file only", "", file, "from a file"},
		{"both empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveAppendPrompt(tt.inline, tt.file)
			c.Require().NoError(err, "resolveAppendPrompt")
			c.Eq(tt.want, got, "resolveAppendPrompt")
		})
	}

	t.Run("file missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent.md")
		_, err := resolveAppendPrompt("", missing)
		c.Require().Error(err, "resolveAppendPrompt")
		c.StrContains(err.Error(), missing, "the error must name the unreadable path")
	})
}

// The two append flags are mutually exclusive at the flag layer — the executor
// stages the text and passes the file, a human passes the inline text, but both
// together would leave daraja guessing which won.
func TestDarajaServeRejectsBothAppendPromptFlags(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newDarajaServeCmd()
	cmd.SetArgs([]string{"--append-system-prompt", "inline", "--append-system-prompt-file", "appendix.md"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	c.Require().Error(err, "both --append-system-prompt and --append-system-prompt-file")
	c.StrContains(err.Error(), "append-system-prompt", "the error must name the conflicting flags")
}

// The executor's Launch appends a bare "--" before the spec's ExtraArgs (see
// TestLaunchCarriesAppendSystemPromptAndExtraArgs). pflag must treat that
// separator as end-of-flags and hand the rest back as POSITIONAL args — a
// flag-shaped extra parsed as a serve flag would mangle the flags Launch maps
// or die on an unknown one, which is exactly what the separator exists to
// prevent. pflag (v1.0.9) drops the separator itself, so Flags().Args() is
// exactly the positionals.
func TestDarajaServeDropsTheSeparatorAndKeepsThePositionals(t *testing.T) {
	c := assert.NewAborting(t)
	cmd := newDarajaServeCmd()
	c.NoError(cmd.ParseFlags([]string{"--", "--foo", "bar"}), "ParseFlags")
	got := cmd.Flags().Args()
	c.EqDiff([]string{"--foo", "bar"}, got, "Flags().Args()")
}

// resolveWorkspaceTools is the one place --workspace-tools becomes the two
// things it shapes, and each fails silently if dropped: the spec's mode is ARGV
// (rebuilt on every Restart, carrying the disallow list and the workspace
// paragraph), while eagerTools is the daraja process's fixed
// ENABLE_TOOL_SEARCH=false. A value claudeargv does not know must be refused
// here — Build would ignore it and launch a native child.
func TestResolveWorkspaceTools(t *testing.T) {
	c := assert.NewCollecting(t)
	tests := []struct {
		raw   string
		mode  string
		eager bool
	}{
		{"", "", false},
		{"native", "", false},
		{"rafiki", "rafiki", true},
	}
	for _, tt := range tests {
		mode, eager, err := resolveWorkspaceTools(tt.raw)
		c.Require().NoError(err, "resolveWorkspaceTools(%q)", tt.raw)
		c.Eq(tt.mode, mode, "resolveWorkspaceTools(%q): mode", tt.raw)
		c.Eq(tt.eager, eager, "resolveWorkspaceTools(%q): eagerTools", tt.raw)
	}

	_, _, err := resolveWorkspaceTools("claude-native")
	c.Require().Error(err, "resolveWorkspaceTools(unknown)")
	c.StrContains(err.Error(), "claude-native", "the refusal must name the value it rejected")
}
