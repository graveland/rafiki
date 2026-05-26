package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Spawn a new pi child and attach a local TUI to it",
		Long: `Spawn a new pi child via the controller, then open the pi TUI driving it.

The pic-helpers pi extension is auto-installed (or upgraded) into
~/.pi/agent/extensions/pic-helpers/ before spawning, so slash commands
like /reload work inside the TUI. Use --no-install-helpers or set
PIC_NO_AUTO_INSTALL_HELPERS=1 to skip.

When the TUI quits (Ctrl+D, /quit), pic asks whether to terminate the session
or leave it running. Use --kill-on-exit or --keep-on-exit to skip the prompt
and choose explicitly.

With --detached, pic create spawns the child and exits without attaching.
The child runs in the background; reattach later with 'pic attach <name>'.

--cwd defaults to the current directory. Specify explicitly to override.

(Note: pic create replaces the earlier ` + "`pic spawn`" + ` subcommand. For
scripting / AFK workflows, use --detached.)`,
		Args: cobra.MaximumNArgs(1),
		RunE: runCreate,
	}
	addSpawnFlags(cmd)
	cmd.Flags().Bool("detached", false, "Spawn without attaching; the child runs in the background")
	cmd.Flags().Bool("kill-on-exit", false, "Terminate the session when the TUI quits (skips exit prompt)")
	cmd.Flags().Bool("keep-on-exit", false, "Always keep the session running on exit (skips exit prompt)")
	cmd.MarkFlagsMutuallyExclusive("kill-on-exit", "keep-on-exit")
	cmd.Flags().Bool("no-install-helpers", false, "Skip the auto-install of the pic-helpers pi extension")
	return cmd
}

// addSpawnFlags registers the shared spawn-related flags on cmd.
func addSpawnFlags(cmd *cobra.Command) {
	cmd.Flags().String("cwd", "", "Working directory, must be absolute (defaults to current directory)")
	cmd.Flags().String("model", "", "Model (e.g. anthropic/claude-sonnet-4)")
	cmd.Flags().String("thinking", "", "Thinking level: off|minimal|low|medium|high|xhigh")
	cmd.Flags().Bool("no-session", false, "Run in ephemeral mode (no session file)")
	cmd.Flags().String("session", "", "Resume an existing session.jsonl by path")
	cmd.Flags().String("fork", "", "Fork from an existing session.jsonl by path")
	cmd.Flags().Bool("no-extensions", false, "Disable extension discovery")
	cmd.Flags().StringSlice("extension", nil, "Load an extension (repeatable)")
	cmd.Flags().Bool("verbose", false, "Verbose startup")
	cmd.Flags().StringSlice("extra-arg", nil, "Extra pi arg (repeatable)")

	_ = cmd.RegisterFlagCompletionFunc("cwd", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return nil, cobra.ShellCompDirectiveFilterDirs
	})
	_ = cmd.RegisterFlagCompletionFunc("session", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{"jsonl"}, cobra.ShellCompDirectiveFilterFileExt
	})
	_ = cmd.RegisterFlagCompletionFunc("fork", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return []string{"jsonl"}, cobra.ShellCompDirectiveFilterFileExt
	})
	_ = cmd.RegisterFlagCompletionFunc("thinking", cobra.FixedCompletions([]string{"off", "minimal", "low", "medium", "high", "xhigh"}, cobra.ShellCompDirectiveNoFileComp))
}

// buildSpawnRequest constructs a SpawnRequest from the spawn flags and
// positional args. Returns an error if required flags are invalid.
func buildSpawnRequest(cmd *cobra.Command, args []string) (protocol.SpawnRequest, error) {
	cwd, _ := cmd.Flags().GetString("cwd")
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return protocol.SpawnRequest{}, fmt.Errorf("cwd: %w", err)
		}
	}
	if !filepath.IsAbs(cwd) {
		return protocol.SpawnRequest{}, fmt.Errorf("--cwd must be absolute (got %q)", cwd)
	}
	model, _ := cmd.Flags().GetString("model")
	thinking, _ := cmd.Flags().GetString("thinking")
	noSession, _ := cmd.Flags().GetBool("no-session")
	resume, _ := cmd.Flags().GetString("session")
	fork, _ := cmd.Flags().GetString("fork")
	noExt, _ := cmd.Flags().GetBool("no-extensions")
	exts, _ := cmd.Flags().GetStringSlice("extension")
	verbose, _ := cmd.Flags().GetBool("verbose")
	extraArgs, _ := cmd.Flags().GetStringSlice("extra-arg")

	name := ""
	if len(args) > 0 {
		name = args[0]
	}

	return protocol.SpawnRequest{
		Type:          protocol.TypeCtrlSpawn,
		Name:          name,
		Cwd:           cwd,
		Model:         model,
		Thinking:      thinking,
		NoSession:     noSession,
		ResumeSession: resume,
		ForkSession:   fork,
		NoExtensions:  noExt,
		Extensions:    exts,
		Verbose:       verbose,
		ExtraArgs:     extraArgs,
	}, nil
}

func runCreate(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	noInstall, _ := cmd.Flags().GetBool("no-install-helpers")
	if !noInstall {
		if err := ensurePicHelpersInstalled(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: pic-helpers auto-install failed: %v\n", err)
			// proceed anyway
		}
	}

	req, err := buildSpawnRequest(cmd, args)
	if err != nil {
		return err
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_spawn: %s", client.FormatError(resp))
	}

	var data protocol.SpawnResponseData
	_ = json.Unmarshal(resp.Data, &data)
	if err := setActive(data.ChildID); err != nil {
		// Best effort — log to stderr but don't fail.
		fmt.Fprintln(os.Stderr, "warning: could not update active marker:", err)
	}

	detached, _ := cmd.Flags().GetBool("detached")
	if detached {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	}

	killOnExit, _ := cmd.Flags().GetBool("kill-on-exit")
	keepOnExit, _ := cmd.Flags().GetBool("keep-on-exit")
	return execPicAttach(data.ChildID, killOnExit, keepOnExit)
}
