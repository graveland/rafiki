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
			"With --passthrough-auth, your own Claude subscription is billed instead\n" +
			"of the daemon's API key: rafiki forwards your credential upstream and\n" +
			"still captures the conversation. Anthropic models only — an OpenRouter\n" +
			"slash id is rejected, because a subscription credential cannot buy one.\n\n" +
			"Everything after -- is passed to claude verbatim.\n\n" +
			"Example:\n" +
			"  rafiki claude --model glm-5.2 -- --permission-mode plan",
		RunE: runClaude,
	}
	cmd.Flags().String("url", envOr("RAFIKI_URL", "http://localhost:8035"), "rafiki proxy base URL (or RAFIKI_URL)")
	cmd.Flags().String("token", envOr("RAFIKI_TOKEN", "dev"), "static bearer token for the proxy (or RAFIKI_TOKEN)")
	cmd.Flags().String("model", os.Getenv("RAFIKI_MODEL"), "model id, <family>-latest alias, or OpenRouter slash id (or RAFIKI_MODEL)")
	cmd.Flags().String("session", os.Getenv("RAFIKI_SESSION"), "X-Rafiki-Session id correlating this session's turns onto one conversation")
	// "1" exactly, matching every other boolean env var in the repo
	// (RAFIKI_RECORD_REQUESTS, RAFIKI_TOOLS_WEB, RAFIKI_LSP_DISABLE). Treating
	// any non-empty value as true would make RAFIKI_CLAUDE_PASSTHROUGH=0 turn
	// billing ON for someone trying to turn it off.
	cmd.Flags().Bool("passthrough-auth", os.Getenv("RAFIKI_CLAUDE_PASSTHROUGH") == "1",
		"bill your own Claude subscription upstream instead of the daemon's API key (or RAFIKI_CLAUDE_PASSTHROUGH=1)")
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
func dialDaemon(ctx context.Context) (*client.Client, error) {
	if url := paths.Get(paths.ControlURL); url != "" {
		token := paths.ControlTokenFromEnv()
		return client.DialURL(ctx, url, token)
	}
	return client.Dial(client.DefaultSocketPath())
}

// runClaude runs `rafiki claude [flags] [-- claude flags...]`.
func runClaude(cmd *cobra.Command, args []string) error {
	url, _ := cmd.Flags().GetString("url")
	token, _ := cmd.Flags().GetString("token")
	model, _ := cmd.Flags().GetString("model")
	session, _ := cmd.Flags().GetString("session")
	passthrough, _ := cmd.Flags().GetBool("passthrough-auth")

	if url == "" {
		return errors.New("--url (or RAFIKI_URL) is required")
	}
	// Both passthrough guards run before the TTY is handed over. The proxy
	// enforces the same rules, but it can only do so on the session's first
	// turn — by which point Claude Code owns the terminal and a clear error
	// reads as a mysterious dead session.
	if passthrough {
		if token == "" {
			return errors.New("--passthrough-auth needs --token (or RAFIKI_TOKEN): rafiki's own token moves to the X-Rafiki-Token header, leaving Authorization free for yours, and without it the proxy cannot authenticate the session")
		}
		if !proxyenv.AnthropicModel(model) {
			return fmt.Errorf("--passthrough-auth bills your Claude subscription, which can only buy Anthropic models, but --model %q resolves to another provider; drop --passthrough-auth to bill the daemon's key instead", model)
		}
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

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
