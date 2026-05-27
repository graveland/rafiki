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
	cmd.Flags().String("preset", "", "Apply a named preset from ~/.pi/agent/pic-presets.json")
	return cmd
}

// addSpawnFlags registers the shared spawn-related flags on cmd.
func addSpawnFlags(cmd *cobra.Command) {
	cmd.Flags().String("cwd", "", "Working directory, must be absolute (defaults to current directory)")
	cmd.Flags().String("model", "", "Model (e.g. anthropic/claude-sonnet-4); also settable via PIC_DEFAULT_MODEL")
	cmd.Flags().String("thinking", "", "Thinking level: off|minimal|low|medium|high|xhigh")
	cmd.Flags().Bool("no-session", false, "Run in ephemeral mode (no session file)")
	cmd.Flags().String("session", "", "Resume an existing session.jsonl by path")
	cmd.Flags().String("fork", "", "Fork from an existing session.jsonl by path")
	cmd.Flags().Bool("no-extensions", false, "Disable extension discovery")
	cmd.Flags().StringSlice("extension", nil, "Load an extension (repeatable)")
	cmd.Flags().Bool("verbose", false, "Verbose startup")
	cmd.Flags().StringSlice("extra-arg", nil, "Extra pi arg (repeatable)")
	cmd.Flags().StringArray("label", nil, "Label as k=v (repeatable); also see PIC_DEFAULT_LABELS")

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
	_ = cmd.RegisterFlagCompletionFunc("model", func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeModel(toComplete), cobra.ShellCompDirectiveNoFileComp
	})
}

// buildSpawnRequest constructs a SpawnRequest from the spawn flags, env-var
// defaults, and positional args. Returns an error if required flags are invalid.
//
// Env-var defaults are read lazily here (not at process start) for test isolation:
//   - PIC_DEFAULT_MODEL: used when --model is not set
//   - PIC_DEFAULT_LABELS: comma-separated k=v pairs merged before --label flags
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

	// PIC_DEFAULT_MODEL: fallback when --model not given.
	model, _ := cmd.Flags().GetString("model")
	if model == "" {
		model = os.Getenv("PIC_DEFAULT_MODEL")
	}

	thinking, _ := cmd.Flags().GetString("thinking")
	noSession, _ := cmd.Flags().GetBool("no-session")
	resume, _ := cmd.Flags().GetString("session")
	fork, _ := cmd.Flags().GetString("fork")
	noExt, _ := cmd.Flags().GetBool("no-extensions")
	exts, _ := cmd.Flags().GetStringSlice("extension")
	verbose, _ := cmd.Flags().GetBool("verbose")
	extraArgs, _ := cmd.Flags().GetStringSlice("extra-arg")

	// PIC_DEFAULT_LABELS: parsed lazily and merged before --label flags.
	envLabels, err := parseEnvLabels(os.Getenv("PIC_DEFAULT_LABELS"))
	if err != nil {
		return protocol.SpawnRequest{}, fmt.Errorf("PIC_DEFAULT_LABELS: %w", err)
	}

	flagLabelPairs, _ := cmd.Flags().GetStringArray("label")
	flagLabels, err := parseLabelPairs(flagLabelPairs)
	if err != nil {
		return protocol.SpawnRequest{}, fmt.Errorf("--label: %w", err)
	}

	// Merge order: env-var defaults < explicit flags.
	labels := mergeLabels(envLabels, flagLabels)

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
		Labels:        labels,
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

	// Apply preset (lowest priority: preset < env-var defaults < explicit flags).
	// buildSpawnRequest has already merged env-var defaults and --label flags;
	// preset fills in any keys/model that weren't set by higher-priority sources.
	presetName, _ := cmd.Flags().GetString("preset")
	if presetName != "" {
		pf, err := loadPresets()
		if err != nil {
			return fmt.Errorf("--preset: %w", err)
		}
		preset, ok := pf.Presets[presetName]
		if !ok {
			return fmt.Errorf("--preset: unknown preset %q (available: %s)", presetName, availablePresets(pf))
		}
		// Preset model is the fallback when neither flag nor PIC_DEFAULT_MODEL set it.
		if req.Model == "" && preset.Model != "" {
			req.Model = preset.Model
		}
		// Merge preset labels under existing req.Labels (existing labels win).
		if len(preset.Labels) > 0 {
			req.Labels = mergeLabels(preset.Labels, req.Labels)
		}
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
	return attachAndDecide(cmd, data.ChildID, killOnExit, keepOnExit)
}
