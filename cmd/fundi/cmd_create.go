package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"git.graveland.dev/brent/fundi/client"
	"git.graveland.dev/brent/fundi/internal/envvar"
	"git.graveland.dev/brent/fundi/protocol"
)

func newCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Spawn a new pi child and attach a local TUI to it",
		Long: `Spawn a new pi child via the controller, then open the pi TUI driving it.

The fundi-helpers pi extension is auto-installed (or upgraded) into
~/.pi/agent/extensions/fundi-helpers/ before spawning, so slash commands
like /reload work inside the TUI. Use --no-install-helpers or set
FUNDI_NO_AUTO_INSTALL_HELPERS=1 to skip.

When the TUI quits (Ctrl+D, /quit), fundi asks whether to terminate the session
or leave it running. Use --kill-on-exit or --keep-on-exit to skip the prompt
and choose explicitly.

With --detached, fundi create spawns the child and exits without attaching.
The child runs in the background; reattach later with 'fundi attach <name>'.

--cwd defaults to the current directory. Specify explicitly to override.

Environment variable defaults (applied before explicit flags; lowest priority):
  FUNDI_DEFAULT_PRESET  preset name from ~/.pi/agent/fundi-presets.json
  FUNDI_DEFAULT_MODEL   fallback model string
  FUNDI_DEFAULT_LABELS  comma-separated k=v label defaults

(Note: fundi create replaces the earlier ` + "`fundi spawn`" + ` subcommand. For
scripting / AFK workflows, use --detached.)`,
		Args: cobra.MaximumNArgs(1),
		RunE: runCreate,
	}
	addSpawnFlags(cmd)
	cmd.Flags().Bool("detached", false, "Spawn without attaching; the child runs in the background")
	cmd.Flags().Bool("kill-on-exit", false, "Terminate the session when the TUI quits (skips exit prompt)")
	cmd.Flags().Bool("keep-on-exit", false, "Always keep the session running on exit (skips exit prompt)")
	cmd.MarkFlagsMutuallyExclusive("kill-on-exit", "keep-on-exit")
	cmd.Flags().Bool("no-install-helpers", false, "Skip the auto-install of the fundi-helpers pi extension")
	cmd.Flags().String("preset", "", "Apply a named preset from ~/.pi/agent/fundi-presets.json (also settable via FUNDI_DEFAULT_PRESET)")
	_ = cmd.RegisterFlagCompletionFunc("preset", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		// Best-effort: silently empty list when presets file is missing or malformed.
		pf, err := loadPresets()
		if err != nil || pf == nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		names := make([]string, 0, len(pf.Presets))
		for name := range pf.Presets {
			names = append(names, name)
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

// addSpawnFlags registers the shared spawn-related flags on cmd.
func addSpawnFlags(cmd *cobra.Command) {
	cmd.Flags().String("cwd", "", "Working directory, must be absolute (defaults to current directory)")
	cmd.Flags().String("kind", "pi", "Agent kind: pi (default), agent (native fundi runtime; needs a provider-qualified --model), or claude (Claude Code)")
	cmd.Flags().String("config-dir", "", "For --kind claude: CLAUDE_CONFIG_DIR selecting the claude profile (plugins/hooks/MCP/settings)")
	cmd.Flags().String("append-system-prompt", "", "Append text to the agent's system prompt, e.g. \"$(cat ~/.claude-prompt.md)\" (applies to pi and claude)")
	cmd.Flags().String("model", "", "Model (e.g. anthropic/claude-sonnet-4); also settable via FUNDI_DEFAULT_MODEL")
	cmd.Flags().String("thinking", "", "Thinking level: off|minimal|low|medium|high|xhigh")
	cmd.Flags().Bool("no-session", false, "Run in ephemeral mode (no session file)")
	cmd.Flags().String("session", "", "Resume an existing session.jsonl by path")
	cmd.Flags().String("fork", "", "Fork from an existing session.jsonl by path")
	cmd.Flags().Bool("no-extensions", false, "Disable extension discovery")
	cmd.Flags().StringSlice("extension", nil, "Load an extension (repeatable)")
	cmd.Flags().Bool("verbose", false, "Verbose startup")
	cmd.Flags().StringSlice("extra-arg", nil, "Extra pi arg (repeatable)")
	cmd.Flags().StringArray("label", nil, "Label as k=v (repeatable); also see FUNDI_DEFAULT_LABELS")
	cmd.Flags().Bool("forward-env", true, "Forward the caller's environment to the pi child (merged with daemon env; caller wins on duplicates)")

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

// resolvePresetName returns the preset to apply: the --preset flag if given,
// else $FUNDI_DEFAULT_PRESET (the pre-rename PIC_DEFAULT_PRESET still works).
// Extracted so tests exercise this resolution rather than reimplementing it —
// an inlined copy in a test passes no matter what the real command reads.
func resolvePresetName(cmd *cobra.Command) string {
	if name, _ := cmd.Flags().GetString("preset"); name != "" {
		return name
	}
	return envvar.Get(envvar.DefaultPreset)
}

// buildSpawnRequest constructs a SpawnRequest from the spawn flags, env-var
// defaults, and positional args. Returns an error if required flags are invalid.
//
// Env-var defaults are read lazily here (not at process start) for test isolation:
//   - FUNDI_DEFAULT_MODEL: used when --model is not set
//   - FUNDI_DEFAULT_LABELS: comma-separated k=v pairs merged before --label flags
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

	// FUNDI_DEFAULT_MODEL: fallback when --model not given.
	model, _ := cmd.Flags().GetString("model")
	if model == "" {
		model = envvar.Get(envvar.DefaultModel)
	}

	kind, _ := cmd.Flags().GetString("kind")
	configDir, _ := cmd.Flags().GetString("config-dir")
	appendSysPrompt, _ := cmd.Flags().GetString("append-system-prompt")

	thinking, _ := cmd.Flags().GetString("thinking")
	noSession, _ := cmd.Flags().GetBool("no-session")
	resume, _ := cmd.Flags().GetString("session")
	fork, _ := cmd.Flags().GetString("fork")
	noExt, _ := cmd.Flags().GetBool("no-extensions")
	exts, _ := cmd.Flags().GetStringSlice("extension")
	verbose, _ := cmd.Flags().GetBool("verbose")
	extraArgs, _ := cmd.Flags().GetStringSlice("extra-arg")

	// FUNDI_DEFAULT_LABELS: parsed lazily and merged before --label flags.
	envLabels, err := parseEnvLabels(envvar.Get(envvar.DefaultLabels))
	if err != nil {
		return protocol.SpawnRequest{}, fmt.Errorf("FUNDI_DEFAULT_LABELS: %w", err)
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

	forwardEnv, _ := cmd.Flags().GetBool("forward-env")
	var env map[string]string
	if forwardEnv {
		env = collectCallerEnv()
	}

	return protocol.SpawnRequest{
		Type:               protocol.TypeCtrlSpawn,
		Name:               name,
		Cwd:                cwd,
		Kind:               kind,
		ConfigDir:          configDir,
		AppendSystemPrompt: appendSysPrompt,
		Model:              model,
		Thinking:           thinking,
		NoSession:          noSession,
		ResumeSession:      resume,
		ForkSession:        fork,
		NoExtensions:       noExt,
		Extensions:         exts,
		Verbose:            verbose,
		ExtraArgs:          extraArgs,
		Labels:             labels,
		Env:                env,
		// EnvOverride=false: daemon's env (launchd-set HOME/PATH) is the base;
		// caller-forwarded vars win on duplicate keys.  This is what users
		// usually want — SSH_AUTH_SOCK, *_API_KEY, GOOGLE_APPLICATION_CREDENTIALS,
		// and the caller's PATH (often richer than launchd's) all override the
		// daemon's minimal defaults.
		EnvOverride: false,
	}, nil
}

// collectCallerEnv snapshots the calling process's environment for inclusion
// in a SpawnRequest. Reserved keys are stripped so they can't override what the
// daemon injects per-child — notably the socket and child id, which the child
// trusts to identify itself and to call home.
//
// BOTH prefixes are stripped: the FUNDI_* names and the PI_CONTROLLER_* ones
// they replaced. Dropping the old prefix while envvar.Get still honours it as a
// fallback would reopen exactly the override this guards against.
func collectCallerEnv() map[string]string {
	environ := os.Environ()
	out := make(map[string]string, len(environ))
	for _, kv := range environ {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		k := kv[:eq]
		if strings.HasPrefix(k, "FUNDI_") || strings.HasPrefix(k, "PI_CONTROLLER_") {
			continue
		}
		out[k] = kv[eq+1:]
	}
	return out
}

func runCreate(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	// claude children have no pi extension system, so the fundi-helpers pi
	// extension is meaningless for them (the helper frame would just be
	// dropped). Skip the auto-install entirely for --kind claude.
	kind, _ := cmd.Flags().GetString("kind")
	noInstall, _ := cmd.Flags().GetBool("no-install-helpers")
	if !noInstall && kind != "claude" {
		if err := ensureHelpersInstalled(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: fundi-helpers auto-install failed: %v\n", err)
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
	presetName := resolvePresetName(cmd)
	if presetName != "" {
		pf, err := loadPresets()
		if err != nil {
			return fmt.Errorf("--preset: %w", err)
		}
		preset, ok := pf.Presets[presetName]
		if !ok {
			return fmt.Errorf("--preset: unknown preset %q (available: %s)", presetName, availablePresets(pf))
		}
		// Preset model is the fallback when neither flag nor FUNDI_DEFAULT_MODEL set it.
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
