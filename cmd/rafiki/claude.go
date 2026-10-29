// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/claudeargv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/promptfile"
	"go.graveland.dev/rafiki/pkg/proxyenv"
)

const (
	// claudeCatalogBudget caps how long the launch waits on the context-window
	// lookup. Missing the pin costs a suboptimal compaction point; making the
	// user wait costs every launch.
	claudeCatalogBudget = 2 * time.Second
)

// newClaudeCmd builds the `rafiki claude` launcher. Flag parsing is plain
// cobra/pflag: pflag already stops at a bare "--" and hands everything after
// it back as positional args (see (*pflag.FlagSet).Parse), which is exactly
// how the claude-side flags in the Example below reach execClaude unparsed.
func newClaudeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "claude [-- claude-args...]",
		Aliases: []string{"cl"},
		Short:   "Launch Claude Code pointed at the rafiki proxy",
		Long: "Resolves the proxy URL and token, sets the environment Claude Code needs,\n" +
			"and execs your own claude binary. Not a daemon child — this runs in your\n" +
			"terminal and is not supervised, listed, or attachable.\n\n" +
			"--passthrough-auth controls who gets billed: auto (default) bills your\n" +
			"own Claude subscription when --model resolves to an Anthropic id\n" +
			"(including no --model at all) and the daemon's API key otherwise; on\n" +
			"forces your subscription and rejects a non-Anthropic --model outright;\n" +
			"off always bills the daemon's key. rafiki forwards your credential\n" +
			"upstream and still captures the conversation.\n\n" +
			"--limits prints your Anthropic subscription's current rate-limit\n" +
			"status (5h/7d utilization) instead of launching a session; it only\n" +
			"has data once a passthrough call has actually happened.\n\n" +
			"Everything after -- is passed to claude verbatim.\n\n" +
			"Example:\n" +
			"  rafiki claude --model openrouter/z-ai/glm-5.2 -- --permission-mode plan",
		RunE: runClaude,
	}
	cmd.Flags().StringP("url", "u", "", "rafiki proxy base URL (default: the profile's `proxy`)")
	// No default here: resolving the profile's token at flag-construction time
	// would read it once per process and bake in whatever existed then. Left
	// empty and resolved in runClaude instead (from the profile mustProfile
	// resolves), so a token minted after the process started still works and
	// an explicit --token still wins.
	cmd.Flags().String("token", "", "static bearer token for the proxy (default: the resolved profile's token)")
	cmd.Flags().StringP("model", "m", os.Getenv("RAFIKI_MODEL"), "model id, one of your model aliases, a <family>-latest alias, or an OpenRouter slash id (or RAFIKI_MODEL)")
	cmd.Flags().String("session", os.Getenv("RAFIKI_SESSION"), "X-Rafiki-Session id correlating this session's turns onto one conversation")
	cmd.Flags().String("passthrough-auth", envOr("RAFIKI_CLAUDE_PASSTHROUGH", string(passthroughAuto)),
		"who gets billed: auto|on|off (or RAFIKI_CLAUDE_PASSTHROUGH). auto bills your own\n"+
			"Claude subscription when --model resolves to an Anthropic id (including no\n"+
			"--model at all) and the daemon's API key otherwise; on/off force the choice")
	// A bare --passthrough-auth (no "=value") keeps working as "on", matching
	// the flag's old boolean form — true/1/false/0 remain accepted aliases too,
	// see parsePassthroughMode.
	cmd.Flags().Lookup("passthrough-auth").NoOptDefVal = string(passthroughOn)
	cmd.Flags().Bool("limits", false,
		"print your Anthropic subscription's current rate-limit status and exit, instead of launching a session")
	cmd.Flags().Bool("turns", false,
		"run a scripted headless session (stream-json) instead of an interactive one;\n"+
			"supply prompts with --turn and/or --turn-file")
	cmd.Flags().String("script", "",
		"TOML script of several claude runs (each its own process, one can resume another\n"+
			"or be killed mid-tool-call); exclusive with --turn and --turn-file")
	cmd.Flags().StringArray("turn", nil,
		"one prompt for a scripted session (repeatable, sent in order)")
	cmd.Flags().String("turn-file", "",
		"file of prompts for a scripted session, one per line ('-' for stdin;\n"+
			"blank lines and lines starting with # are skipped)")
	cmd.Flags().String("system-prompt", claudeCaptureSystemPrompt,
		"system prompt for a scripted session (default: one line, so a capture stays small)")
	cmd.Flags().String("tools", claudeCaptureTools,
		"built-in tools a scripted session may use (default: Bash only; \"\" for none)")
	cmd.Flags().Duration("turn-timeout", claudeTurnTimeout,
		"ceiling on ONE scripted turn; a wedged session fails instead of hanging")
	// --model asks the DAEMON for claude-kind ids (see completeModel): an
	// OpenRouter id on a claude child produces one that spawns, attaches and
	// never answers.
	_ = cmd.RegisterFlagCompletionFunc("model", func(c *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeModel(c, "claude", toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	// --passthrough-auth is a closed set: the file's own passthrough constants
	// (parsePassthroughMode additionally tolerates true/1/false/0/no aliases,
	// but those are undocumented compatibility spellings, not the values a
	// user should be offered).
	_ = cmd.RegisterFlagCompletionFunc("passthrough-auth", cobra.FixedCompletions(
		[]string{string(passthroughAuto), string(passthroughOn), string(passthroughOff)},
		cobra.ShellCompDirectiveNoFileComp,
	))
	return cmd
}

// claudeInvocation is the assembled environment and argument list for exec'ing
// the claude binary. The environment itself is built by pkg/proxyenv, shared
// with the daemon's --kind claude children so the two cannot drift.
type claudeInvocation struct {
	Env  []string
	Args []string
}

// claudeAutoCompactWindow asks the daemon for the CLAUDE_CODE_AUTO_COMPACT_WINDOW
// threshold for model, or returns 0 to leave Claude Code's default alone.
//
// Best-effort by design, and this is the load-bearing part: an unreachable
// daemon, a daemon with no catalog, and an unknown model all return 0 and the
// session starts normally. `rafiki claude` must keep working with the daemon
// down — that is the difference between "the daemon is down" and "I cannot
// start a coding session".
func claudeAutoCompactWindow(ctx context.Context, cmd *cobra.Command, model string) int {
	ctx, cancel := context.WithTimeout(ctx, claudeCatalogBudget)
	defer cancel()

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return 0
	}
	resp, err := ep.control().ModelInfo(ctx, connect.NewRequest(&rafikiv1.ModelInfoRequest{Model: model}))
	if err != nil {
		return 0
	}
	return int(resp.Msg.GetAutoCompactWindow())
}

// runClaude runs `rafiki claude [flags] [-- claude flags...]`.
func runClaude(cmd *cobra.Command, args []string) error {
	if limits, _ := cmd.Flags().GetBool("limits"); limits {
		return runClaudeLimits(cmd)
	}
	token, _ := cmd.Flags().GetString("token")
	model, _ := cmd.Flags().GetString("model")
	session, _ := cmd.Flags().GetString("session")
	passthroughFlag, _ := cmd.Flags().GetString("passthrough-auth")

	script, err := claudeScriptFromFlags(cmd)
	if err != nil {
		return err
	}
	// Scripted mode is selected by supplying turns or a script: --turn,
	// --turn-file and --script imply it, and --turns makes the requirement
	// explicit (and errors when it is empty).
	scripted := script != nil

	// A scripted run resolves NO profile. It is a capture, not a daemon
	// session: it must work on a host with no rafiki config, no daemon and no
	// token minted for anything, which is exactly the host `rafikid proxy
	// record` exists for. Everything it needs comes from flags or the
	// environment.
	var p profile.Resolved
	if !scripted {
		p = mustProfile(cmd)
	}

	flagURL, _ := cmd.Flags().GetString("url")
	url := resolveClaudeURL(flagURL, scripted, os.Getenv("ANTHROPIC_BASE_URL"), p.Proxy)
	if url == "" {
		if scripted {
			return errors.New("no endpoint: pass --url, or run under `rafikid proxy record --exec`, " +
				"which sets ANTHROPIC_BASE_URL to its own listener")
		}
		return fmt.Errorf("profile %q has no proxy URL; pass --url, or set `proxy` in %s",
			p.Name, profile.ProfilesFile())
	}
	if scripted {
		// The shell's own Anthropic credential, if there is one. A scripted run
		// resolves no profile, so there is no profile token to fall back to — and
		// a missing credential is NOT an error: Claude Code's own OAuth login is
		// a perfectly good way to authenticate a capture.
		if token == "" {
			token = envFirst("ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY")
		}
	} else {
		token, err = resolveClaudeToken(token, p.Token)
		if err != nil {
			return err
		}
	}
	passthroughMode, err := parsePassthroughMode(passthroughFlag)
	if err != nil {
		return err
	}
	passthrough := passthroughAuthFor(passthroughMode, model)
	// This guard runs before the TTY is handed over. The proxy enforces the
	// same rule, but it can only do so on the session's first turn — by which
	// point Claude Code owns the terminal and a clear error reads as a
	// mysterious dead session. (The old "needs a token" half of this guard is
	// gone: resolveClaudeToken above already guarantees a non-empty token or
	// this function has already returned.) auto never trips this: it derives
	// passthrough FROM proxyenv.AnthropicModel(model), so the two can't disagree.
	if passthrough && !proxyenv.AnthropicModel(model) {
		return fmt.Errorf("--passthrough-auth=on bills your Claude subscription, which can only buy Anthropic models, but --model %q resolves to another provider; use --passthrough-auth=off (or auto) to bill the daemon's key instead", model)
	}
	// Preflight so a dead proxy is a clear message here rather than an opaque
	// connection error from inside Claude Code after it has taken the TTY.
	if scripted {
		// Passthrough omits the bearer so Claude Code prefers OAuth; a capture
		// wants the credential it was handed to reach the upstream verbatim, and
		// the endpoint need not be a rafiki proxy at all (so no /healthz
		// preflight either).
		passthrough = false
	} else if err := claudePreflight(url); err != nil {
		return err
	}

	if scripted {
		timeout, _ := cmd.Flags().GetDuration("turn-timeout")
		systemPrompt, _ := cmd.Flags().GetString("system-prompt")
		tools, _ := cmd.Flags().GetString("tools")
		plans := script.plan(claudeScriptDefaults{
			Model: model, SystemPrompt: systemPrompt, Tools: tools, Timeout: timeout, SessionID: session,
		})
		for _, plan := range plans {
			opts, err := scriptedRunOptions(cmd, plan, url, token)
			if err != nil {
				return err
			}
			if err := runClaudeTurnsFn(opts); err != nil {
				if plan.Name != "" {
					return fmt.Errorf("run %q: %w", plan.Name, err)
				}
				return err
			}
		}
		return nil
	}

	sessionID := session
	if sessionID == "" {
		sessionID = uuid.NewString()
	}

	autoCompact := 0
	if model != "" {
		autoCompact = claudeAutoCompactWindow(context.Background(), cmd, model)
	}

	env, vals := proxyenv.ClaudeEnv(os.Environ(), proxyenv.ClaudeOptions{
		URL:               url,
		Token:             token,
		Model:             model,
		AutoCompactWindow: autoCompact,
		PassthroughAuth:   passthrough,
		Headers: map[string]string{
			// Correlates every turn of this session onto ONE captured
			// conversation; without it the proxy falls back to one
			// conversation per request.
			"X-Rafiki-Session": sessionID,
			"X-Rafiki-Source":  "rafiki-claude",
		},
	})
	// ModeInteractive is what keeps this path free of -p, stream-json,
	// --dangerously-skip-permissions and --disallowedTools: a human owns this
	// TTY and answers permission prompts, and AskUserQuestion has a renderer
	// here and must stay callable.
	//
	// The MCP config is staged to a file immediately before Build, so its JSON —
	// which carries the ${RAFIKI_MCP_TOKEN} placeholder, never an expanded token
	// — does not ride argv where `ps` shows it.
	staged, err := claudeargv.Stage(claudeargv.Params{
		Mode:      claudeargv.ModeInteractive,
		Model:     model,
		MCPConfig: vals.MCPConfig,
		ModelArgs: vals.ModelArgs,
		UserArgs:  args,
	}, promptfile.Dir())
	if err != nil {
		return err
	}
	argv := claudeargv.Build(staged)
	return execClaude(claudeInvocation{Env: env, Args: argv})
}

// passthroughMode aliases proxyenv.PassthroughMode: the parsing/resolution
// logic lives there now, shared with rafikid's daraja launch path, so the two
// auth decisions cannot drift apart. Kept as a local alias (rather than
// rewriting every call site to proxyenv.X) purely to avoid a mechanical
// rename across this file and its tests.
type passthroughMode = proxyenv.PassthroughMode

const (
	passthroughAuto = proxyenv.PassthroughAuto
	passthroughOn   = proxyenv.PassthroughOn
	passthroughOff  = proxyenv.PassthroughOff
)

func parsePassthroughMode(s string) (passthroughMode, error) { return proxyenv.ParsePassthroughMode(s) }

func passthroughAuthFor(mode passthroughMode, model string) bool {
	return proxyenv.PassthroughAuthFor(mode, model)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// resolveClaudeToken resolves the proxy bearer token: flagToken (the --token
// flag's value) if it was given, else profileToken — the resolved profile's
// token (pkg/profile, from the profile's own token file, written by `rafiki
// user create`). Deliberately called at RunE time, not baked into the flag's
// default: a cobra flag default is computed once, at command-construction
// time, so resolving the profile there would freeze in whatever token existed
// at process start and miss one minted later in the same process's lifetime.
//
// There is no literal fallback (the old default was the string "dev", which
// authenticates against nothing on a real daemon and turns a missing
// credential into a confusing 401 rather than a clear error here).
func resolveClaudeToken(flagToken, profileToken string) (string, error) {
	token := flagToken
	if token == "" {
		token = profileToken
	}
	if token == "" {
		return "", errors.New("no rafiki token: pass --token, or run 'rafiki user create <name>' " +
			"to mint one for the current profile (see `rafiki profile show`)")
	}
	return token, nil
}

func claudePreflight(url string) error {
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(strings.TrimSuffix(url, "/") + "/healthz")
	if err != nil {
		return fmt.Errorf("rafiki is not answering at %s — start it with 'make run' first: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close is not actionable
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rafiki at %s returned %s from /healthz", url, resp.Status)
	}
	return nil
}

// execClaude runs the local claude binary with the assembled env and args,
// handing over the TTY. A variable for the same reason runOSCmd is: tests
// capture the assembled invocation instead of spawning a real claude binary.
var execClaude = func(inv claudeInvocation) error {
	return launchClaude(inv)
}

func launchClaude(inv claudeInvocation) error {
	cmd := exec.Command("claude", inv.Args...) //nolint:gosec // launching the user's own claude
	cmd.Env = inv.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Claude Code handles Ctrl-C itself while it owns the TTY. Ignoring SIGINT
	// here stops this process dying out from under it and leaving the terminal
	// in claude's raw mode; the deferred reset keeps Ctrl-C working afterwards.
	signal.Ignore(syscall.SIGINT)
	defer signal.Reset(syscall.SIGINT)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("claude: start (is it on PATH?): %w", err)
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("claude: %w", err)
	}
	return nil
}

// runClaudeTurnsFn is a variable for the same reason execClaude is: tests
// capture the assembled scripted session instead of spawning a real claude.
var runClaudeTurnsFn = runClaudeTurns

// resolveClaudeURL is the endpoint precedence for `rafiki claude`: --url, then
// (scripted runs only) an inherited ANTHROPIC_BASE_URL, then the profile's
// proxy.
//
// The env var outranks the profile for a scripted run because it is an explicit
// instruction from the process that started us: `rafikid proxy record --exec`
// sets it to its own listener, and letting a profile's proxy win would send the
// capture to the daemon — a session that looks like it worked, records nothing,
// and leaves an empty directory behind.
func resolveClaudeURL(flagURL string, scripted bool, envURL, profileProxy string) string {
	if flagURL != "" {
		return flagURL
	}
	if scripted && envURL != "" {
		return envURL
	}
	return profileProxy
}

// claudeCaptureSystemPrompt is the whole system prompt a scripted capture gets.
// Claude Code's own is long, and a capture exists to show what the model did,
// not to reproduce the assistant's personality.
const claudeCaptureSystemPrompt = "You are a minimal capture session. Answer in one short sentence, and use a tool when asked to."

// claudeCaptureTools is the built-in tool set a scripted capture may use. One
// tool is enough to record a tool_use/tool_result round trip, and every schema
// left in the request is kilobytes of noise in every recorded file.
const claudeCaptureTools = "Bash"

// scriptedClaudeMinimalArgs keeps a captured session as small and as
// reproducible as it can be.
//
// --safe-mode is the load-bearing one: no CLAUDE.md, skills, plugins, hooks,
// MCP servers, custom agents or output styles, so the request carries the
// conversation and nothing else. It leaves auth alone, which is why it works
// the same with an API key and with a Claude Code login. --bare would go
// further but reads ANTHROPIC_API_KEY only and never OAuth, so a capture on a
// subscription machine could not use it.
//
// The MCP pair removes every server rather than inheriting the operator's;
// --strict-mcp-config alone would still load them from settings. The system
// prompt is only replaced when the caller supplies one: Claude Code's own prompt
// carries the tool-use instructions, and dropping it is a good way to get a
// session that cannot call a tool. With the default prompt,
// --exclude-dynamic-system-prompt-sections moves the per-machine sections out of
// the system prompt, which shrinks it and keeps it stable across runs.
func scriptedClaudeMinimalArgs(systemPrompt, tools string, persist bool) []string {
	args := []string{
		// --safe-mode disables every customization Claude Code would otherwise
		// load (CLAUDE.md, skills, plugins, hooks, MCP servers, custom commands
		// and agents, output styles, workflows) while leaving auth, the built-in
		// tools and permissions alone.
		"--safe-mode",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
		// The built-in tool schemas dominate a Claude Code request; a capture
		// that only needs to prove tool traffic happened asks for one of them.
		"--tools", tools,
	}
	// Nothing is written outside the record directory unless a later run has to
	// resume this session.
	if !persist {
		args = append(args, "--no-session-persistence")
	}
	if systemPrompt != "" {
		return append(args, "--system-prompt", systemPrompt)
	}
	return append(args, "--exclude-dynamic-system-prompt-sections")
}

// scriptedRunOptions assembles one run's claude invocation. The environment is
// built per run because the session header and the model are per run: a
// resumed run carries its predecessor's session id so both land on one
// conversation at the proxy.
func scriptedRunOptions(cmd *cobra.Command, plan claudeRunPlan, url, token string) (claudeTurnsOptions, error) {
	// A scripted run never asks for passthrough auth: a capture sends the
	// credential it was handed verbatim.
	env, vals := proxyenv.ClaudeEnv(os.Environ(), proxyenv.ClaudeOptions{
		URL:   url,
		Token: token,
		Model: plan.Model,
		Headers: map[string]string{
			"X-Rafiki-Session": plan.SessionID,
			"X-Rafiki-Source":  "rafiki-claude",
		},
	})
	params := claudeargv.Params{
		Mode:      claudeargv.ModeHeadless,
		Model:     plan.Model,
		ModelArgs: vals.ModelArgs,
	}
	if plan.Resume {
		params.ResumeSession = plan.SessionID
	}
	staged, err := claudeargv.Stage(params, promptfile.Dir())
	if err != nil {
		return claudeTurnsOptions{}, err
	}
	args := append(claudeargv.Build(staged), scriptedClaudeMinimalArgs(plan.SystemPrompt, plan.Tools, plan.Persist)...)
	if plan.Persist && !plan.Resume {
		args = append(args, "--session-id", plan.SessionID)
	}
	return claudeTurnsOptions{
		Env:       scriptedClaudeEnv(env, token),
		Args:      append(args, plan.Args...),
		Turns:     plan.Turns,
		Out:       cmd.OutOrStdout(),
		Err:       cmd.ErrOrStderr(),
		Timeout:   plan.Timeout,
		Interrupt: plan.Interrupt,
	}, nil
}

// scriptedClaudeEnv makes the credential explicit and nothing else.
//
// Both variables are removed first, unconditionally: pkg/proxyenv populates
// ANTHROPIC_AUTH_TOKEN with a "rafiki" placeholder when there is no token, and
// that placeholder is exactly what stops Claude Code using its own OAuth login.
// With a token, the bearer form is what Claude Code sends — it accepts an API
// key there as readily as an OAuth token, so one path serves both.
func scriptedClaudeEnv(env []string, token string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") || strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			continue
		}
		out = append(out, kv)
	}
	if token == "" {
		return out
	}
	return append(out, "ANTHROPIC_AUTH_TOKEN="+token)
}
