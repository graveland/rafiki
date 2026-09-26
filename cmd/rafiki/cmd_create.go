package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/costfmt"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/prefill"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/proxyenv"
	"go.graveland.dev/rafiki/pkg/pymodules"
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

Kind precedence, strongest first:
  --kind (refused if it conflicts with the preset's kind)
  the named preset's kind (the preset resolves in the daemon)
  the resolved profile's kind
  fundi (default)

With --preset, the preset resolves IN THE DAEMON: it supplies the kind, model,
tools, prompt and budgets, and the client sends only the preset name and the
preset's kind. --model still overrides the preset's model; a profile or
remembered model is deliberately NOT sent -- a preset is a request, a profile
default is ambient, and sending one would make the preset's model permanently
unreachable.

Without a preset, model precedence, strongest first:
  --model
  the resolved profile's model
  the model last spawned for this kind (remembered per profile and kind)
  the daemon's default

Labels: --label flags are merged over the resolved profile's labels, with the
flags winning on a key collision.

--kind script spawns a script child: a saved pymodule run as the child's
process, no model attached. Pass --pymodule <repo>:<script> (repo "local" or
a git source's name) and put the script's argv after --:

  rafiki create -d --kind script --pymodule local:driver -- --fast 5

The child's name defaults to the script's name; a positional argument before
-- still names it. Model, tools, prompts and session flags do not apply to a
script child and are refused by the daemon.

Set these defaults on a profile, not an environment variable: see
'rafiki profile show' and 'rafiki profile add --help'.`,

		Args: validateCreateArgs,
		RunE: runCreate,
	}
	addSpawnFlags(cmd)
	cmd.Flags().BoolP("detached", "d", false, "Spawn without attaching; the child runs in the background")
	cmd.Flags().BoolP("interactive", "i", false,
		"Choose the agent's settings in a form, with model search and filtering")
	cmd.Flags().Bool("kill-on-exit", false, "Terminate the session when the TUI quits (skips exit prompt)")
	cmd.Flags().Bool("keep-on-exit", false, "Always keep the session running on exit (skips exit prompt)")
	cmd.MarkFlagsMutuallyExclusive("kill-on-exit", "keep-on-exit")
	cmd.Flags().StringP("preset", "p", "", "Apply a named preset from `rafiki preset list` (also settable via a profile's `preset` field)")
	cmd.Flags().String("pymodule", "", "--kind script only: the pymodule to run as the child, as <repo>:<script> (e.g. local:driver); repo is \"local\" or a git source's name. Everything after -- is passed to the script as argv")
	// The form has no script-spec field, and -i with --pymodule would otherwise
	// refuse through the form branch with a "pass --pymodule" the caller had
	// already passed. Refuse at parse time instead, like --prefill-files above.
	cmd.MarkFlagsMutuallyExclusive("interactive", "pymodule")
	cmd.Flags().String("prefill-files", "",
		"File listing files the child reads before its first turn (one per line; path[:N-M] or a glob; '-' for stdin, only with --detached). fundi only.")
	// The create form cannot carry a pre-fill (pkg/tui's SpawnRequest has no
	// such field), so accepting both would parse the list and then silently
	// drop it on the form path. Refuse at parse time instead.
	cmd.MarkFlagsMutuallyExclusive("interactive", "prefill-files")
	_ = cmd.RegisterFlagCompletionFunc("preset", func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Best-effort over Connect: a completion handler must never exit or
		// print, so every failure — endpoint, network, daemon — degrades to
		// "no candidates" by design.
		ep, err := newConnectEndpoint(cmd)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		ctx, cancel := context.WithTimeout(cmdCtx(cmd), completionDeadline)
		defer cancel()
		resp, err := ep.control().ListPresets(ctx, connect.NewRequest(&rafikiv1.ListPresetsRequest{}))
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		names := make([]string, 0, len(resp.Msg.GetRows()))
		for _, r := range resp.Msg.GetRows() {
			names = append(names, r.GetName())
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

// addSpawnFlags registers the shared spawn-related flags on cmd.
func addSpawnFlags(cmd *cobra.Command) {
	cmd.Flags().String("cwd", "", "Working directory, must be absolute (defaults to current directory)")
	cmd.Flags().String("kind", "", "Agent kind: fundi (default; native fundi runtime, needs a provider-qualified --model) or claude (Claude Code); also settable via a profile's `kind` field")
	cmd.Flags().String("config-dir", "", "CLAUDE_CONFIG_DIR for --kind claude ONLY; ignored by --kind fundi")
	cmd.Flags().String("append-system-prompt", "", "Append text to the agent's system prompt (any kind); the profile's append-system-prompt.md, if present, is prepended to this")
	cmd.Flags().StringP("model", "m", "", "Model (e.g. anthropic/claude-sonnet-4); also settable via a profile's `model` field")
	cmd.Flags().String("thinking", "", "Thinking level: off|low|medium|high|xhigh")
	cmd.Flags().Bool("no-session", false, "Run in ephemeral mode (no session file)")
	cmd.Flags().String("session", "", "Resume an existing session.jsonl by path")
	cmd.Flags().String("fork", "", "Fork from an existing session.jsonl by path")
	cmd.Flags().Bool("no-extensions", false, "Disable extension discovery")
	cmd.Flags().StringSlice("extension", nil, "Load an extension (repeatable)")
	cmd.Flags().Bool("verbose", false, "Verbose startup")
	cmd.Flags().StringSlice("extra-arg", nil, "Extra pi arg (repeatable)")
	cmd.Flags().StringSlice("skills-dir", nil, "Additional skills directory for --kind fundi (repeatable)")
	cmd.Flags().String("mcp-config", "", "Path to .mcp.json for --kind fundi (default: <cwd>/.mcp.json, else $RAFIKI_MCP_CONFIG or <config dir>/mcp.json)")
	cmd.Flags().StringArray("label", nil, "Label as k=v (repeatable); also see a profile's `labels` field")
	cmd.Flags().Bool("forward-env", true, "Forward the caller's environment to the pi child (merged with daemon env; caller wins on duplicates)")
	cmd.Flags().Bool("record-requests", false, "Record raw LLM API requests and responses for debugging")
	cmd.Flags().String("passthrough-auth", envOr("RAFIKI_CLAUDE_PASSTHROUGH", ""),
		"--kind claude only: who gets billed for a daraja-routed child: auto (default) bills your own\n"+
			"Claude subscription when the model resolves to Anthropic, off always bills the daemon's key,\n"+
			"on always bills the subscription (also see RAFIKI_CLAUDE_PASSTHROUGH)")
	cmd.Flags().String("parent", "", "Child id of the spawning parent (records rafiki/parent and rafiki/root)")
	cmd.Flags().Int("max-depth", -1, "how many further levels of agents this child may spawn (0 = none; default 1). Bounded absolutely by the daemon's RAFIKI_MAX_DEPTH")
	maxCostHelp := "USD budget for this child's whole subtree (unset = unlimited)"
	if cur := clientstate.LoadScoped(clientstate.Scope{}).Currency; cur != nil && cur.Rate > 0 && cur.Code != "" {
		maxCostHelp = fmt.Sprintf(
			"budget for this child's whole subtree, in %s (unset = unlimited); see `rafiki config`",
			cur.Code)
	}
	cmd.Flags().Float64("max-cost", -1, maxCostHelp)
	cmd.Flags().Int("max-children", -1, "simultaneously live agents allowed beneath this child (default 4)")
	cmd.Flags().String("executor-selector", paths.Get(paths.ExecutorSelector),
		"label selector choosing an executor from the daemon's pool to run this agent's filesystem and shell tools on (e.g. owner=brent,env=home); also see RAFIKI_EXECUTOR_SELECTOR")
	cmd.Flags().String("executor", paths.Get(paths.Executor),
		"target one specific executor by its machine name or id (e.g. greyshift); mutually exclusive with --executor-selector; also see RAFIKI_EXECUTOR")
	cmd.MarkFlagsMutuallyExclusive("executor", "executor-selector")
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
	_ = cmd.RegisterFlagCompletionFunc("thinking", cobra.FixedCompletions([]string{"off", "low", "medium", "high", "xhigh"}, cobra.ShellCompDirectiveNoFileComp))
	// Scoped to --kind: a claude child cannot resolve an OpenRouter slash id, and
	// the fundi child cannot resolve one of claude's provider-local ids. Offering
	// the union produces a child that spawns and attaches and then never
	// answers. cobra has already parsed any --kind appearing earlier on the
	// line by the time this runs, but an UNSET --kind flag does NOT mean
	// "fundi": since Task 10, the effective default comes from the resolved
	// profile's `kind` field (see resolveKind/buildSpawnRequest). Resolving the
	// same way here keeps a profile with `kind = "claude"` from being offered
	// fundi-only OpenRouter ids when --kind was never passed on the line.
	_ = cmd.RegisterFlagCompletionFunc("model", func(c *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		flagKind, _ := c.Flags().GetString("kind")
		profileKind := ""
		// Best-effort, matching --preset's completion above: a completion
		// handler must never exit or print, so a misconfigured profile
		// degrades to resolveKind("", "") -- which falls back to
		// protocol.KindFundi -- rather than failing the completion.
		if p, err := resolveProfile(c); err == nil {
			profileKind = p.Kind
		}
		kind := resolveKind(flagKind, profileKind)
		return completeModel(c, kind, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("executor", func(c *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		flagKind, _ := c.Flags().GetString("kind")
		profileKind := ""
		if p, err := resolveProfile(c); err == nil {
			profileKind = p.Kind
		}
		kind := resolveKind(flagKind, profileKind)
		return completeExecutor(c, kind, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
}

// validateCreateArgs accepts the child's optional name positional plus —
// for a script spawn — the script's argv after `--`. The strictness the old
// MaximumNArgs(1) enforced (one positional at most) is kept for the name;
// the after-`--` tail is allowed only when it can be script argv, judged as
// far as flags allow here (a kind arriving from a PROFILE is re-checked in
// buildSpawnRequest, where the profile is resolved anyway).
func validateCreateArgs(cmd *cobra.Command, args []string) error {
	nameArgs, scriptArgs := splitCreatePositionals(cmd, args)
	if len(nameArgs) > 1 {
		return fmt.Errorf("accepts at most 1 arg for the child's name, received %d", len(nameArgs))
	}
	if len(scriptArgs) > 0 {
		flagKind, _ := cmd.Flags().GetString("kind")
		if flagKind != "" && flagKind != protocol.KindScript {
			return fmt.Errorf("arguments after -- are the script child's argv and require --kind script")
		}
	}
	return nil
}

// splitCreatePositionals splits create's positional args at `--`: anything
// before it names the child (at most one, enforced by validateCreateArgs),
// anything after it is the script child's argv. No `--` means every
// positional is a name candidate.
func splitCreatePositionals(cmd *cobra.Command, args []string) (nameArgs, scriptArgs []string) {
	if dash := cmd.ArgsLenAtDash(); dash >= 0 && dash <= len(args) {
		return args[:dash], args[dash:]
	}
	return args, nil
}

// Extracted so tests exercise this resolution rather than reimplementing it —
// an inlined copy in a test passes no matter what the real command reads.
// resolvePresetName returns the preset to apply: the --preset flag if given,
// else the resolved profile's `preset` field.
// Extracted so tests exercise this resolution rather than reimplementing it —
// an inlined copy in a test passes no matter what the real command reads.
func resolvePresetName(cmd *cobra.Command) string {
	if name, _ := cmd.Flags().GetString("preset"); name != "" {
		return name
	}
	return mustProfile(cmd).Preset
}

// resolveKind picks the agent kind. Extracted so the precedence is testable
// without building a whole cobra command — an inlined copy in a test passes no
// matter what the real command reads.
func resolveKind(flagKind, profileKind string) string {
	if flagKind != "" {
		return flagKind
	}
	if profileKind != "" {
		return profileKind
	}
	return protocol.KindFundi
}

// resolveModel picks the model. See docs/plans/2026-09-04-client-profiles-plan.md
// Task 10 before reordering anything here. The named preset no longer takes
// part in this chain: it now resolves in the daemon, which applies the
// preset's model before any field the client sends.
//
// The profile default sits above the remembered model deliberately: a profile
// default is a declaration, the remembered model an inference from what
// happened last time and must never override a declaration.
func resolveModel(flagModel, profileModel, remembered string) string {
	for _, candidate := range []string{flagModel, profileModel, remembered} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// resolveSpawnModel applies the no-preset model chain (--model already on
// req, then the profile's default, then the remembered per-kind model) —
// except for a script kind: a script child has no model, and the profile's
// default and the remembered model are inferences about LLM children that
// must not ride a script spawn (sending one would be refused as `field
// "model" does not apply to kind "script"` on every spawn for a profile
// that names a default model). An explicitly typed --model still travels:
// the daemon's refusal is the honest answer to a flag the caller chose.
func resolveSpawnModel(req *protocol.SpawnRequest, p profile.Resolved, profileName string) {
	if req.Kind == protocol.KindScript {
		return
	}
	req.Model = resolveModel(req.Model, p.Model, clientstate.LastModelFor(profileName, req.Kind))
}

// resolveExecutor picks the executor reference and/or selector to send with a
// spawn. Mirrors resolveModel's shape deliberately.
//
// --executor wins outright and is returned as a ref (ExecutorRef). Otherwise
// an explicit --executor-selector (or its RAFIKI_EXECUTOR_SELECTOR default)
// is returned as a selector, untouched — a label-selector policy and a
// remembered single executor answer different questions, so the selector is
// never compared against the remembered ref. Only when NEITHER was given does
// the remembered executor apply, and only when the caller says it is still
// eligible (a Task 6 concern — this function takes that as a plain bool so it
// stays a pure precedence rule with no network round trip of its own).
func resolveExecutor(flagExecutor, flagSelector, remembered string, rememberedEligible bool) (ref, selector string) {
	if flagExecutor != "" {
		return flagExecutor, ""
	}
	if flagSelector != "" {
		return "", flagSelector
	}
	if remembered != "" && rememberedEligible {
		return remembered, ""
	}
	return "", ""
}

// buildSpawnRequest constructs a SpawnRequest from the spawn flags, the
// resolved profile's defaults, and positional args. Returns an error if
// required flags are invalid.
//
// The profile is resolved lazily here (not at process start) for test
// isolation. It supplies the kind and labels directly; the model chain also
// needs the preset and the remembered model, both known only in runCreate, so
// req.Model here carries only the --model flag and is finished off there via
// resolveModel.
func buildSpawnRequest(cmd *cobra.Command, args []string) (protocol.SpawnRequest, error) {
	p := mustProfile(cmd)
	flagKind, _ := cmd.Flags().GetString("kind")
	kind := resolveKind(flagKind, p.Kind)

	cwd, _ := cmd.Flags().GetString("cwd")
	if cwd == "" {
		// --kind claude used to fork a real subprocess ON THE DAEMON ITSELF
		// (cmd.Dir, pkg/child/runner.go), so defaulting to this process's cwd
		// only made sense against a local daemon — against a remote profile
		// that path exists on the wrong machine entirely. daraja changed
		// that: once the daemon has an executor pool, --kind claude routes
		// through whichever executor gets bound — by default the session
		// executor this same command starts below, rooted at exactly this
		// cwd on THIS machine — exactly like --kind fundi already works, and
		// for the same reason. The client's own os.Getwd() is now the right
		// default for both kinds, local daemon or remote. It only misses for
		// a remote daemon with NO executor pool at all, where claude still
		// falls back to a subprocess on the daemon's own machine — a real but
		// now-uncommon case, and the daemon's own spawn error names the
		// missing path clearly rather than failing silently.
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return protocol.SpawnRequest{}, fmt.Errorf("cwd: %w", err)
		}
	}
	if !filepath.IsAbs(cwd) {
		return protocol.SpawnRequest{}, fmt.Errorf("--cwd must be absolute (got %q)", cwd)
	}

	// The preset, the profile's default and the remembered model all come
	// later, in that order -- see runCreate and resolveModel.
	model, _ := cmd.Flags().GetString("model")

	configDir, _ := cmd.Flags().GetString("config-dir")
	flagAppendSysPrompt, _ := cmd.Flags().GetString("append-system-prompt")
	profileAppendSysPrompt, err := loadProfileAppendSystemPrompt(p.Name)
	if err != nil {
		return protocol.SpawnRequest{}, err
	}
	appendSysPrompt := mergeAppendSystemPrompt(profileAppendSysPrompt, flagAppendSysPrompt)

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

	// Profile labels are merged UNDER the --label flags, so a flag wins on a
	// key collision — the same precedence RAFIKI_DEFAULT_LABELS had.
	profileLabels := p.Labels

	flagLabelPairs, _ := cmd.Flags().GetStringArray("label")
	flagLabels, err := parseLabelPairs(flagLabelPairs)
	if err != nil {
		return protocol.SpawnRequest{}, fmt.Errorf("--label: %w", err)
	}

	// Merge order: profile defaults < explicit flags.
	labels := mergeLabels(profileLabels, flagLabels)

	nameArgs, scriptArgs := splitCreatePositionals(cmd, args)
	if len(nameArgs) > 1 {
		// Mirrors validateCreateArgs for direct callers (tests, or a code
		// path that skipped cobra's Args validation).
		return protocol.SpawnRequest{}, fmt.Errorf("accepts at most 1 arg for the child's name, received %d", len(nameArgs))
	}
	var name string
	if len(nameArgs) > 0 {
		name = nameArgs[0]
	}
	if kind != protocol.KindScript && len(scriptArgs) > 0 {
		// The profile's kind resolved non-script while the line carried a
		// `--` tail: the same refusal validateCreateArgs gives an explicit
		// --kind, now that the kind is fully resolved.
		return protocol.SpawnRequest{}, fmt.Errorf("arguments after -- are the script child's argv and require --kind script")
	}

	forwardEnv, _ := cmd.Flags().GetBool("forward-env")
	var env map[string]string
	if forwardEnv {
		env = collectCallerEnv()
	}

	recordRequests, _ := cmd.Flags().GetBool("record-requests")
	passthroughAuth, _ := cmd.Flags().GetString("passthrough-auth")
	if passthroughAuth != "" {
		if _, err := proxyenv.ParsePassthroughMode(passthroughAuth); err != nil {
			return protocol.SpawnRequest{}, err
		}
	}

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
		PassthroughAuth:    passthroughAuth,
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
		usd := costfmt.ToUSD(v, clientstate.LoadScoped(clientstate.Scope{}).Currency)
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

	// A script spawn carries its pymodule spec client-side: --pymodule is
	// REQUIRED here, because the daemon's own refusal for a script kind
	// without a spec describes a request this CLI should never have built,
	// and the after-`--` positionals are the script's argv.
	if kind == protocol.KindScript {
		spec, err := resolvePymoduleFlag(cmd)
		if err != nil {
			return protocol.SpawnRequest{}, err
		}
		spec.Args = append(spec.Args, scriptArgs...)
		req.Script = spec
		// Same default the pymodule_start tool applies: an unnamed script
		// child reads as its script in `rafiki list`, which is the honest
		// summary of what it is. Ids stay unique regardless.
		if req.Name == "" {
			req.Name = spec.Script
		}
	} else if cmd.Flags().Changed("pymodule") {
		// Fail CLOSED: --pymodule is --kind script's payload, and a caller who
		// typed it but let the kind resolve to something else (no --kind, or a
		// profile with a different kind) would otherwise get a silent FUNDI
		// child with the profile's default model attached — real spend where a
		// script was meant. Refuse and name the flag that fixes it.
		v, _ := cmd.Flags().GetString("pymodule")
		return protocol.SpawnRequest{}, fmt.Errorf("--pymodule %q requires --kind script; add --kind script to spawn the module as a script child", v)
	}

	return req, nil
}

// resolvePymoduleFlag parses --pymodule <repo>:<script> into a ScriptSpec.
// repo "local" is the spawning owner's saved modules; any other repo names a
// registered git source. The script name is validated client-side (a bare
// Python identifier, the same rule every consumer applies) so a typo fails
// before any dial; the repo name is left to the daemon, whose registered
// sources are the authority on what exists.
func resolvePymoduleFlag(cmd *cobra.Command) (*protocol.ScriptSpec, error) {
	v, _ := cmd.Flags().GetString("pymodule")
	if v == "" {
		return nil, errors.New("--kind script requires --pymodule <repo>:<script> (e.g. --pymodule local:driver)")
	}
	repo, script, ok := strings.Cut(v, ":")
	if !ok || repo == "" || script == "" {
		return nil, fmt.Errorf("--pymodule must be <repo>:<script> (got %q); repo is \"local\" or a git source's name", v)
	}
	if err := pymodules.ValidName(script); err != nil {
		return nil, fmt.Errorf("--pymodule: script: %w", err)
	}
	return &protocol.ScriptSpec{Repo: repo, Script: script}, nil
}

// applyCreatePreset shapes a spawn request to carry a named preset. The
// daemon resolves the preset's model, tools, prompt and budgets itself, so
// this only pins what the client must decide: the preset's kind (the executor
// logic below branches on req.Kind, so it overrides a profile kind or the
// fundi default), the preset name, and a model of ONLY the --model flag —
// an explicit flag outranks the preset on the daemon, but a profile or
// remembered model must never travel with a preset, or the preset's model
// would be permanently unreachable. An explicit --kind that contradicts the
// preset is refused rather than silently resolved one way or the other.
// Extracted so the shaping is testable without a daemon.
func applyCreatePreset(req *protocol.SpawnRequest, name string, rec presets.Record, kindFlag string, kindChanged bool, modelFlag string) error {
	if kindChanged && kindFlag != "" && kindFlag != rec.Kind {
		return fmt.Errorf("--kind %q conflicts with preset %q (kind %q)", kindFlag, name, rec.Kind)
	}
	req.Preset = name
	req.Kind = rec.Kind
	req.Model = modelFlag
	return nil
}

// resolvePrefillFiles reads and parses the --prefill-files flag, if set, into
// SpawnRequest entries. Extracted so the flag's failure modes are testable
// without a daemon, and so runCreate can refuse a bad list BEFORE mustDial —
// it dials nothing, and a parse error is a user-input error that must not
// open a connection first.
func resolvePrefillFiles(cmd *cobra.Command) ([]protocol.PrefillRead, error) {
	list, _ := cmd.Flags().GetString("prefill-files")
	if list == "" {
		return nil, nil
	}
	var (
		text []byte
		err  error
	)
	if list == "-" {
		detached, _ := cmd.Flags().GetBool("detached")
		if !detached {
			return nil, errors.New("--prefill-files -: stdin is only available with --detached")
		}
		text, err = io.ReadAll(os.Stdin)
	} else {
		text, err = os.ReadFile(list)
	}
	if err != nil {
		return nil, fmt.Errorf("--prefill-files %s: %w", list, err)
	}
	return prefill.Parse(string(text))
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
	// Resolve the mode before doing anything: -j and -J together is a
	// user-input error and must not spawn a child first.
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	// Read and parse --prefill-files BEFORE any dial: a bad list is a
	// user-input error and must not open a connection first.
	prefillEntries, err := resolvePrefillFiles(cmd)
	if err != nil {
		return err
	}

	c := mustDial(cmd)
	defer c.Close()

	req, err := buildSpawnRequest(cmd, args)
	if err != nil {
		return err
	}
	req.Prefill = prefillEntries

	p := mustProfile(cmd)

	// Resolve the named preset, if any, against the daemon: the daemon applies
	// the preset's model, tools, prompt and budgets itself, so the request only
	// carries the name, the preset's kind, and the --model flag value — a
	// profile or remembered model must NOT be sent, or it would outrank the
	// preset's model on the daemon side. This happens before the form branch so
	// the form is prefilled with the request that will actually spawn.
	presetName := resolvePresetName(cmd)
	if presetName != "" {
		ep, err := newConnectEndpoint(cmd)
		if err != nil {
			return err
		}
		resp, err := ep.control().GetPreset(cmdCtx(cmd),
			connect.NewRequest(&rafikiv1.GetPresetRequest{Name: presetName}))
		if err != nil {
			if connect.CodeOf(err) == connect.CodeNotFound {
				return fmt.Errorf("--preset: no preset %q (see `rafiki preset list`)", presetName)
			}
			return err
		}
		rows := resp.Msg.GetRows()
		if len(rows) == 0 {
			return fmt.Errorf("--preset: no preset %q (see `rafiki preset list`)", presetName)
		}
		rec := presets.FromProto(rows[0])
		modelFlag, _ := cmd.Flags().GetString("model")
		kindFlag, _ := cmd.Flags().GetString("kind")
		if err := applyCreatePreset(&req, presetName, rec, kindFlag, cmd.Flags().Changed("kind"), modelFlag); err != nil {
			return err
		}
	} else {
		resolveSpawnModel(&req, p, p.Name)
	}

	noLocalExecutor, _ := cmd.Flags().GetBool("no-local-executor")
	detached, _ := cmd.Flags().GetBool("detached")
	flagExecutor, _ := cmd.Flags().GetString("executor")

	if wantsCreateForm(cmd, args, isStdinTTY()) {
		if req.Kind == protocol.KindScript {
			// Reachable only for a profile with `kind = "script"` and a bare
			// create: --pymodule and --kind are shaping flags, so a spelled
			// one takes the direct path above the form. The form has no
			// script-spec field, so letting it open would spawn a request
			// the daemon refuses for the least legible reason.
			return errors.New("the create form cannot spawn a script child; pass --pymodule <repo>:<script> (e.g. --pymodule local:driver)")
		}
		// The form resolves the executor interactively (its own field, the
		// picker, and the daemon's auto-resolve), so the flag -- only reachable
		// here with -i, since it suppresses the form on its own -- is what the
		// form is PREFILLED with, not what the spawn quietly does differently.
		req.ExecutorRef = flagExecutor
		return runCreateForm(cmd, c, req, noLocalExecutor)
	}

	// A kind that must be LAUNCHED (anything but fundi) can never be served by
	// the local session executor below -- it never advertises any
	// LaunchKinds, which is the bug this whole plan exists to fix. For such a
	// kind, resolve via the daemon's live executor catalog BEFORE ever
	// touching --executor-selector's precedence, unless the caller already
	// gave an explicit --executor or --executor-selector of their own. See
	// docs/plans/2026-09-06-executor-selection-design.md §1 and §5.
	if req.Kind != protocol.KindFundi && flagExecutor == "" && req.ExecutorSelector == "" {
		if req.Kind == protocol.KindScript {
			// A script child does not NEED an executor: the daemon hosts it
			// locally when none can launch it, and a daemon with no pool at
			// all hosts locally by design — so a lister that cannot answer
			// is not a reason to refuse the spawn. Leave the field blank
			// and let Spawn's own routing decide, exactly like the
			// zero-eligible case below. AMBIGUITY is not tolerated: several
			// eligible executors with no --executor would let the daemon
			// silently pick one, the exact thing this pre-flight exists to
			// prevent.
			auto, ambiguous, lerr := resolveLaunchExecutorDetailed(cmdCtx(cmd), cmd, p.Name, req.Kind)
			if ambiguous != "" {
				return errors.New(ambiguous)
			}
			_ = lerr // tolerated: see above
			flagExecutor = auto
		} else {
			auto, err := resolveLaunchExecutor(cmdCtx(cmd), cmd, p.Name, req.Kind)
			if err != nil {
				return err
			}
			// auto == "" means zero executors support this kind; leave it
			// blank so Spawn's own clear refusal explains why, rather than
			// duplicating that message here.
			flagExecutor = auto
		}
	}

	// By this point flagExecutor is already fully resolved for a
	// launch-required kind (resolveLaunchExecutor above vetted a remembered
	// ref's live eligibility itself before returning it); passing "" here for
	// `remembered` is deliberate, not an omission -- resolveExecutor's own
	// remembered-fallback branch exists for a plain fundi spawn, where
	// blindly trusting a remembered ref with no live check would silently
	// skip the session-executor fallback this task must not change.
	ref, selector := resolveExecutor(flagExecutor, req.ExecutorSelector, "", false)
	req.ExecutorRef, req.ExecutorSelector = ref, selector

	if req.ExecutorRef == "" && req.ExecutorSelector == "" && req.Kind == protocol.KindFundi && !noLocalExecutor {
		selector, stop, err := startSessionExecutor(cmdCtx(cmd), c, req.Cwd, p)
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
	clientstate.RememberModel(p.Name, req.Kind, req.Model)
	if ref := req.ExecutorRef; ref != "" {
		clientstate.RememberExecutor(p.Name, req.Kind, ref)
	}
	if err := setActive(p.Name, data.ChildID); err != nil {
		// Best effort — log to stderr but don't fail.
		fmt.Fprintln(os.Stderr, "warning: could not update active marker:", err)
	}

	if detached {
		return renderCreateSummary(os.Stdout, data, mode)
	}

	killOnExit, _ := cmd.Flags().GetBool("kill-on-exit")
	keepOnExit, _ := cmd.Flags().GetBool("keep-on-exit")
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	return attachAndDecide(cmd, ep, data.ChildID, killOnExit, keepOnExit)
}

// renderCreateSummary writes the detached spawn record in the requested mode:
// pretty JSON (byte-identical to the pre-tables output — test/integration
// unmarshals it), one compact JSONL line, or one `key: value` line per field
// the record actually carries, skipping empty ones. A non-detached create
// attaches instead of printing and is unaffected by the mode.
func renderCreateSummary(w io.Writer, data protocol.SpawnResponseData, mode outputMode) error {
	switch mode {
	case outputJSONL:
		return writeJSONL(w, []any{data})
	case outputJSON:
		return writeJSON(w, data)
	default:
		for _, line := range spawnRecordLines(data) {
			if _, err := fmt.Fprintln(w, line); err != nil {
				return err
			}
		}
		return nil
	}
}

// spawnRecordLines renders the record's fields as `key: value` text lines in
// struct order. A field the record does not carry (json omitempty) or carries
// empty is skipped — the text view reports what got spawned, nothing else.
// stalled appears only when true: false is the field's zero value, not news.
func spawnRecordLines(data protocol.SpawnResponseData) []string {
	var lines []string
	add := func(key, value string) {
		if value != "" {
			lines = append(lines, key+": "+value)
		}
	}
	add("childId", data.ChildID)
	add("sessionId", data.SessionID)
	add("sessionFile", data.SessionFile)
	add("model", data.Model)
	if data.Stalled {
		lines = append(lines, "stalled: true")
	}
	return lines
}
