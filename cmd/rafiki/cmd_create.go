package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/costfmt"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "create [name]",
		Aliases: []string{"cr"},
		Short:   "Spawn a new child and attach the rafiki TUI to it",
		Long: `Spawn a new child via the controller, then open the rafiki TUI driving it.

When the TUI quits (Ctrl+C, Ctrl+D), rafiki asks whether to terminate the
session or leave it running. Use --kill-on-exit or --keep-on-exit to skip the
prompt and choose explicitly.

With --detached, rafiki create spawns the child and exits without attaching.
The child runs in the background; reattach later with 'rafiki attach <name>'.

--cwd defaults to the current directory. Specify explicitly to override.

Run with no arguments, rafiki create opens a form: pick the kind, search models
by name, and filter or sort them by cost, context, capability and benchmark
score (^S). The form is prefilled with exactly what a bare create would have
spawned, so pressing enter is the same as not using it. Pass anything that
shapes the child -- a name, --model, --kind, --cwd, --preset, -d -- and create
spawns directly instead; -i opens the form anyway, prefilled.

Model precedence, strongest first:
  --model
  RAFIKI_DEFAULT_MODEL
  --preset's model
  the model last spawned for this kind (remembered per kind)
  the daemon's default

The remembered model makes RAFIKI_DEFAULT_MODEL optional rather than obsolete:
the variable is something you configured and still wins, so setting it behaves
exactly as before.

Environment variable defaults (applied before explicit flags; lowest priority):
  RAFIKI_DEFAULT_PRESET  preset name from <config dir>/presets.json (see 'rafiki presets')
  RAFIKI_DEFAULT_MODEL   fallback model string
  RAFIKI_DEFAULT_LABELS  comma-separated k=v label defaults`,

		Args: cobra.MaximumNArgs(1),
		RunE: runCreate,
	}
	addSpawnFlags(cmd)
	cmd.Flags().BoolP("detached", "d", false, "Spawn without attaching; the child runs in the background")
	cmd.Flags().BoolP("interactive", "i", false,
		"Choose the agent's settings in a form, with model search and filtering")
	cmd.Flags().Bool("kill-on-exit", false, "Terminate the session when the TUI quits (skips exit prompt)")
	cmd.Flags().Bool("keep-on-exit", false, "Always keep the session running on exit (skips exit prompt)")
	cmd.MarkFlagsMutuallyExclusive("kill-on-exit", "keep-on-exit")
	cmd.Flags().StringP("preset", "p", "", "Apply a named preset from <config dir>/presets.json (also settable via RAFIKI_DEFAULT_PRESET)")
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
	cmd.Flags().String("kind", protocol.KindFundi, "Agent kind: fundi (default; native fundi runtime, needs a provider-qualified --model) or claude (Claude Code)")
	cmd.Flags().String("config-dir", "", "CLAUDE_CONFIG_DIR for --kind claude ONLY; ignored by --kind fundi")
	cmd.Flags().String("append-system-prompt", "", "Append text to the agent's system prompt, e.g. \"$(cat ~/.claude-prompt.md)\" (applies to claude)")
	cmd.Flags().StringP("model", "m", "", "Model (e.g. anthropic/claude-sonnet-4); also settable via RAFIKI_DEFAULT_MODEL")
	cmd.Flags().String("thinking", "", "Thinking level: off|minimal|low|medium|high|xhigh")
	cmd.Flags().Bool("no-session", false, "Run in ephemeral mode (no session file)")
	cmd.Flags().String("session", "", "Resume an existing session.jsonl by path")
	cmd.Flags().String("fork", "", "Fork from an existing session.jsonl by path")
	cmd.Flags().Bool("no-extensions", false, "Disable extension discovery")
	cmd.Flags().StringSlice("extension", nil, "Load an extension (repeatable)")
	cmd.Flags().Bool("verbose", false, "Verbose startup")
	cmd.Flags().StringSlice("extra-arg", nil, "Extra pi arg (repeatable)")
	cmd.Flags().StringSlice("skills-dir", nil, "Additional skills directory for --kind fundi (repeatable)")
	cmd.Flags().String("mcp-config", "", "Path to .mcp.json for --kind fundi (default: <cwd>/.mcp.json, else $RAFIKI_MCP_CONFIG or <config dir>/mcp.json)")
	cmd.Flags().StringArray("label", nil, "Label as k=v (repeatable); also see RAFIKI_DEFAULT_LABELS")
	cmd.Flags().Bool("forward-env", true, "Forward the caller's environment to the pi child (merged with daemon env; caller wins on duplicates)")
	cmd.Flags().Bool("record-requests", false, "Record raw LLM API requests and responses for debugging")
	cmd.Flags().String("parent", "", "Child id of the spawning parent (records rafiki/parent and rafiki/root)")
	cmd.Flags().Int("max-depth", -1, "how many further levels of agents this child may spawn (0 = none; default 1). Bounded absolutely by the daemon's RAFIKI_MAX_DEPTH")
	maxCostHelp := "USD budget for this child's whole subtree (unset = unlimited)"
	if cur := clientstate.Load().Currency; cur != nil && cur.Rate > 0 && cur.Code != "" {
		maxCostHelp = fmt.Sprintf(
			"budget for this child's whole subtree, in %s (unset = unlimited); see `rafiki config`",
			cur.Code)
	}
	cmd.Flags().Float64("max-cost", -1, maxCostHelp)
	cmd.Flags().Int("max-children", -1, "simultaneously live agents allowed beneath this child (default 4)")
	cmd.Flags().String("executor-selector", paths.Get(paths.ExecutorSelector),
		"label selector choosing an executor from the daemon's pool to run this agent's filesystem and shell tools on (e.g. owner=brent,env=home); also see RAFIKI_EXECUTOR_SELECTOR")
	cmd.Flags().Bool("no-local-executor", false,
		"do not offer this machine as a workspace; nothing here joins the daemon's executor pool for this session")

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
	// Scoped to --kind: a claude child cannot resolve an OpenRouter slash id, and
	// the fundi child cannot resolve one of claude's provider-local ids. Offering
	// the union produces a child that spawns and attaches and then never
	// answers. cobra has already parsed any --kind appearing earlier on the
	// line by the time this runs; an unset flag yields its default, "fundi".
	_ = cmd.RegisterFlagCompletionFunc("model", func(c *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		kind, _ := c.Flags().GetString("kind")
		return completeModel(c, kind, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
}

// resolvePresetName returns the preset to apply: the --preset flag if given,
// else $RAFIKI_DEFAULT_PRESET.
// Extracted so tests exercise this resolution rather than reimplementing it —
// an inlined copy in a test passes no matter what the real command reads.
func resolvePresetName(cmd *cobra.Command) string {
	if name, _ := cmd.Flags().GetString("preset"); name != "" {
		return name
	}
	return paths.Get(paths.DefaultPreset)
}

// buildSpawnRequest constructs a SpawnRequest from the spawn flags, env-var
// defaults, and positional args. Returns an error if required flags are invalid.
//
// Env-var defaults are read lazily here (not at process start) for test isolation:
//   - RAFIKI_DEFAULT_MODEL: used when --model is not set
//   - RAFIKI_DEFAULT_LABELS: comma-separated k=v pairs merged before --label flags
func buildSpawnRequest(cmd *cobra.Command, args []string) (protocol.SpawnRequest, error) {
	kind, _ := cmd.Flags().GetString("kind")

	cwd, _ := cmd.Flags().GetString("cwd")
	if cwd == "" {
		// For --kind claude, cwd names a directory the DAEMON itself must
		// fork a real subprocess in (cmd.Dir, pkg/child/runner.go) — defaulting
		// to this process's cwd only makes sense against the local daemon; for
		// a remote RAFIKI_URL that's a different machine entirely, and left
		// unchecked this silently ships a path that exists on the client and
		// fails server-side with a "no such file or directory" that gives no
		// hint the path was ever local.
		//
		// --kind fundi never forks a daemon-local process: its filesystem
		// access, if any, goes through whichever executor gets bound — by
		// default the session executor this same command starts below,
		// rooted at exactly this cwd on THIS machine. So the client's own
		// os.Getwd() is always the right default for fundi, local daemon or
		// remote.
		if kind != protocol.KindFundi && remoteDialURL() != "" {
			return protocol.SpawnRequest{}, errors.New("--cwd is required when RAFIKI_URL names a remote daemon (there is no local directory to default to on that machine)")
		}
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return protocol.SpawnRequest{}, fmt.Errorf("cwd: %w", err)
		}
	}
	if !filepath.IsAbs(cwd) {
		return protocol.SpawnRequest{}, fmt.Errorf("--cwd must be absolute (got %q)", cwd)
	}

	// RAFIKI_DEFAULT_MODEL: fallback when --model not given. The preset and the
	// remembered model come later, in that order -- see runCreate.
	model, _ := cmd.Flags().GetString("model")
	if model == "" {
		model = paths.Get(paths.DefaultModel)
	}

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
	skillsDirs, _ := cmd.Flags().GetStringSlice("skills-dir")
	mcpConfig, _ := cmd.Flags().GetString("mcp-config")

	// RAFIKI_DEFAULT_LABELS: parsed lazily and merged before --label flags.
	envLabels, err := parseEnvLabels(paths.Get(paths.DefaultLabels))
	if err != nil {
		return protocol.SpawnRequest{}, fmt.Errorf("RAFIKI_DEFAULT_LABELS: %w", err)
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

	recordRequests, _ := cmd.Flags().GetBool("record-requests")

	parent, _ := cmd.Flags().GetString("parent")

	req := protocol.SpawnRequest{
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
		SkillsDirs:         skillsDirs,
		MCPConfig:          mcpConfig,
		Labels:             labels,
		ParentChildID:      parent,
		Env:                env,
		RecordRequests:     recordRequests,
		// EnvOverride=false: daemon's env (launchd-set HOME/PATH) is the base;
		// caller-forwarded vars win on duplicate keys.  This is what users
		// usually want — SSH_AUTH_SOCK, *_API_KEY, GOOGLE_APPLICATION_CREDENTIALS,
		// and the caller's PATH (often richer than launchd's) all override the
		// daemon's minimal defaults.
		EnvOverride: false,
	}

	if cmd.Flags().Changed("max-depth") {
		v, _ := cmd.Flags().GetInt("max-depth")
		req.MaxDepth = &v
	}
	if cmd.Flags().Changed("max-cost") {
		v, _ := cmd.Flags().GetFloat64("max-cost")
		usd := costfmt.ToUSD(v, clientstate.Load().Currency)
		req.MaxCost = &usd
	}
	if cmd.Flags().Changed("max-children") {
		v, _ := cmd.Flags().GetInt("max-children")
		req.MaxChildren = &v
	}
	// Read unconditionally rather than behind Flags().Changed(): the flag
	// default already carries RAFIKI_EXECUTOR_SELECTOR, and Changed() reports
	// whether the user typed the flag, not whether the value is meaningful — so
	// gating on it makes a computed default unreachable.
	req.ExecutorSelector, _ = cmd.Flags().GetString("executor-selector")

	return req, nil
}

// collectCallerEnv snapshots the calling process's environment for inclusion
// in a SpawnRequest. Reserved keys are stripped so they can't override what the
// daemon injects per-child — notably the socket and child id, which the child
// trusts to identify itself and to call home.
//
// Which keys count as reserved is paths.IsReservedEnvKey — shared with the MCP
// host, which strips the same set from the daemon's own environment before
// exec'ing a third-party server. The reasons differ and the set does not, and a
// second copy of the list here is exactly the drift this repo keeps finding.
//
// For this caller the reasons are: a stale export under RAFIKI_*, FUNDI_* or
// PI_CONTROLLER_* has no business reaching a spawned child even though paths.Get
// no longer reads the retired spellings; and forwarding the caller's API keys
// would override the daemon's own and, for claude children, defeat the proxy's
// capture path.
func collectCallerEnv() map[string]string {
	environ := os.Environ()
	out := make(map[string]string, len(environ))
	for _, kv := range environ {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		if k := kv[:eq]; !paths.IsReservedEnvKey(k) {
			out[k] = kv[eq+1:]
		}
	}
	return out
}

func runCreate(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

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
		// Preset model is the fallback when neither flag nor RAFIKI_DEFAULT_MODEL set it.
		if req.Model == "" && preset.Model != "" {
			req.Model = preset.Model
		}
		// Merge preset labels under existing req.Labels (existing labels win).
		if len(preset.Labels) > 0 {
			req.Labels = mergeLabels(preset.Labels, req.Labels)
		}
	}

	// The remembered model is the LAST fallback before the daemon's default,
	// below the preset as well as the environment variable.
	//
	// Both of those are DECLARATIONS -- someone wrote them down -- while this
	// is an inference from what happened last time, and an inference must
	// never override a declaration. Applying it earlier is what broke
	// TestPreset_MergeOrder: the preset's model stopped being reachable at all.
	if req.Model == "" {
		req.Model = clientstate.LastModelFor(req.Kind)
	}

	noLocalExecutor, _ := cmd.Flags().GetBool("no-local-executor")
	detached, _ := cmd.Flags().GetBool("detached")

	if wantsCreateForm(cmd, args, isStdinTTY()) {
		return runCreateForm(cmd, c, req, noLocalExecutor)
	}

	// Only when the caller named no executor of their own. An explicit
	// --executor-selector means "work over there", and standing up a local
	// executor as well would offer this machine to a pool for a session that
	// is not going to use it.
	if req.ExecutorSelector == "" && !noLocalExecutor {
		selector, stop, err := startSessionExecutor(cmdCtx(cmd), c, req.Cwd)
		if err != nil {
			return fmt.Errorf("this machine could not join as a workspace: %w", err)
		}
		req.ExecutorSelector = selector
		// A detached spawn returns right after this, ending the process; a
		// deferred stop would kill the executor before the child has used it.
		// The executor dies with the process — a headless child has no client
		// to serve it, and attach is the way that child acquires one later.
		if !detached {
			defer stop()
		}
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_spawn: %s", client.FormatError(resp))
	}

	// The child set changed, so whatever a TAB answered a moment ago is stale.
	dropChildCompletionCache(cmd)

	var data protocol.SpawnResponseData
	_ = json.Unmarshal(resp.Data, &data)
	// Remember what actually got spawned, not what was asked for: a preset or
	// an alias may have supplied it, and replaying the resolved choice is what
	// makes the next bare create land on the same model.
	clientstate.RememberModel(req.Kind, req.Model)
	if err := setActive(data.ChildID); err != nil {
		// Best effort — log to stderr but don't fail.
		fmt.Fprintln(os.Stderr, "warning: could not update active marker:", err)
	}

	if detached {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	}

	killOnExit, _ := cmd.Flags().GetBool("kill-on-exit")
	keepOnExit, _ := cmd.Flags().GetBool("keep-on-exit")
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	return attachAndDecide(cmd, ep, data.ChildID, killOnExit, keepOnExit)
}
