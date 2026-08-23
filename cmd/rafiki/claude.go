// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
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
			"Everything after -- is passed to claude verbatim.\n\n" +
			"Example:\n" +
			"  rafiki claude --model glm-5.2 -- --permission-mode plan",
		RunE: runClaude,
	}
	cmd.Flags().StringP("url", "u", envOr("RAFIKI_URL", "http://localhost:8035"), "rafiki proxy base URL (or RAFIKI_URL)")
	// No default here: resolving paths.TokenFromEnv() (which falls back to
	// ~/.config/rafiki/token) at flag-construction time would read the file
	// once per process and bake in whatever existed then. Left empty and
	// resolved in runClaude instead, so a token minted after the process
	// started still works and an explicit --token still wins.
	cmd.Flags().String("token", "", "static bearer token for the proxy (or RAFIKI_TOKEN, else ~/.config/rafiki/token)")
	cmd.Flags().StringP("model", "m", os.Getenv("RAFIKI_MODEL"), "model id, <family>-latest alias, or OpenRouter slash id (or RAFIKI_MODEL)")
	cmd.Flags().String("session", os.Getenv("RAFIKI_SESSION"), "X-Rafiki-Session id correlating this session's turns onto one conversation")
	cmd.Flags().String("passthrough-auth", envOr("RAFIKI_CLAUDE_PASSTHROUGH", string(passthroughAuto)),
		"who gets billed: auto|on|off (or RAFIKI_CLAUDE_PASSTHROUGH). auto bills your own\n"+
			"Claude subscription when --model resolves to an Anthropic id (including no\n"+
			"--model at all) and the daemon's API key otherwise; on/off force the choice")
	// A bare --passthrough-auth (no "=value") keeps working as "on", matching
	// the flag's old boolean form — true/1/false/0 remain accepted aliases too,
	// see parsePassthroughMode.
	cmd.Flags().Lookup("passthrough-auth").NoOptDefVal = string(passthroughOn)
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
func claudeAutoCompactWindow(ctx context.Context, model string) int {
	ctx, cancel := context.WithTimeout(ctx, claudeCatalogBudget)
	defer cancel()

	c, err := dialDaemon(ctx)
	if err != nil {
		return 0
	}
	defer c.Close()

	resp, err := c.Request(ctx, protocol.ModelInfoRequest{
		Type: protocol.TypeCtrlModelInfo, Model: model,
	})
	if err != nil || !resp.Success {
		return 0
	}
	var data protocol.ModelInfoResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return 0
	}
	return data.AutoCompactWindow
}

// dialDaemon connects to the daemon's control endpoint, mirroring mustDial but
// returning an error instead of exiting. Used where a dead daemon is graceful
// degradation (return 0) rather than a fatal error. mustDial is exactly wrong
// here: it calls os.Exit(2) on a connection failure, and `rafiki claude` must
// keep working with the daemon down.
//
// Shares remoteDialURL (cli_helpers.go) with mustDial rather than repeating
// the client.IsRemoteURL gate inline — see that function's comment for why
// the two must never independently decide what counts as remote.
func dialDaemon(ctx context.Context) (*client.Client, error) {
	if u := remoteDialURL(); u != "" {
		return client.DialURL(ctx, u, paths.TokenFromEnv())
	}
	return client.Dial(client.DefaultSocketPath())
}

// runClaude runs `rafiki claude [flags] [-- claude flags...]`.
func runClaude(cmd *cobra.Command, args []string) error {
	url, _ := cmd.Flags().GetString("url")
	token, _ := cmd.Flags().GetString("token")
	model, _ := cmd.Flags().GetString("model")
	session, _ := cmd.Flags().GetString("session")
	passthroughFlag, _ := cmd.Flags().GetString("passthrough-auth")

	if url == "" {
		return errors.New("--url (or RAFIKI_URL) is required")
	}
	token, err := resolveClaudeToken(token)
	if err != nil {
		return err
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
	if err := claudePreflight(url); err != nil {
		return err
	}

	sessionID := session
	if sessionID == "" {
		sessionID = uuid.NewString()
	}

	autoCompact := 0
	if model != "" {
		autoCompact = claudeAutoCompactWindow(context.Background(), model)
	}

	env, modelArgs := proxyenv.Claude(os.Environ(), proxyenv.ClaudeOptions{
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
	return execClaude(claudeInvocation{Env: env, Args: append(modelArgs, args...)})
}

// passthroughMode is the parsed form of --passthrough-auth / RAFIKI_CLAUDE_PASSTHROUGH.
// A tri-state rather than a bool because "unset" and "off" are genuinely
// different requests here: unset means let --model decide, off means bill the
// daemon's key no matter what --model is. Mirrors the RTKMode
// (auto/on/off) convention already used for --rtk and --bash-rtk.
type passthroughMode string

const (
	// passthroughAuto bills your own subscription when --model resolves to an
	// Anthropic id (including no --model at all) and the daemon's key otherwise.
	passthroughAuto passthroughMode = "auto"
	// passthroughOn forces your subscription and rejects a non-Anthropic --model.
	passthroughOn passthroughMode = "on"
	// passthroughOff always bills the daemon's key.
	passthroughOff passthroughMode = "off"
)

// parsePassthroughMode parses --passthrough-auth / RAFIKI_CLAUDE_PASSTHROUGH.
// true/1 and false/0 remain accepted aliases for on/off, matching the flag's
// old boolean form. Unlike RTKMode's ParseRTKMode, an unrecognised value is a
// hard error rather than a silent fallback to auto: this switch decides who
// gets billed, and a typo must not decide that quietly.
func parsePassthroughMode(s string) (passthroughMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "auto":
		return passthroughAuto, nil
	case "on", "true", "1":
		return passthroughOn, nil
	case "off", "false", "0", "no":
		return passthroughOff, nil
	default:
		return "", fmt.Errorf("invalid --passthrough-auth %q: want auto, on, or off", s)
	}
}

// passthroughAuthFor resolves a parsed passthroughMode against model into the
// bool proxyenv.ClaudeOptions wants.
func passthroughAuthFor(mode passthroughMode, model string) bool {
	switch mode {
	case passthroughOn:
		return true
	case passthroughOff:
		return false
	default: // passthroughAuto
		return proxyenv.AnthropicModel(model)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// resolveClaudeToken resolves the proxy bearer token: flagToken (the --token
// flag's value) if it was given, else the same fallback the rest of the CLI
// uses — RAFIKI_TOKEN, then ~/.config/rafiki/token (paths.TokenFromEnv).
// Deliberately called at RunE time, not baked into the flag's default: a
// cobra flag default is computed once, at command-construction time, so
// reading the token file there would freeze in whatever token existed at
// process start and miss one minted later in the same process's lifetime.
//
// There is no literal fallback (the old default was the string "dev", which
// authenticates against nothing on a real daemon and turns a missing
// credential into a confusing 401 rather than a clear error here).
func resolveClaudeToken(flagToken string) (string, error) {
	token := flagToken
	if token == "" {
		token = paths.TokenFromEnv()
	}
	if token == "" {
		return "", errors.New("no rafiki token: pass --token, set RAFIKI_TOKEN, or run 'rafiki user create <name>' " +
			"to mint one (written to ~/.config/rafiki/token)")
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
// handing over the TTY.
func execClaude(inv claudeInvocation) error {
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
