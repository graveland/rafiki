// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/proxyenv"
	"go.graveland.dev/rafiki/pkg/routing"
)

const (
	claudeCatalogTTL     = 24 * time.Hour
	claudeCatalogTimeout = 2 * time.Second
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
			"Everything after -- is passed to claude verbatim.\n\n" +
			"Example:\n" +
			"  rafiki claude --model glm-5.2 -- --permission-mode plan",
		RunE: runClaude,
	}
	cmd.Flags().String("url", envOr("RAFIKI_URL", "http://localhost:8035"), "rafiki proxy base URL (or RAFIKI_URL)")
	cmd.Flags().String("token", envOr("RAFIKI_TOKEN", "dev"), "static bearer token for the proxy (or RAFIKI_TOKEN)")
	cmd.Flags().String("model", os.Getenv("RAFIKI_MODEL"), "model id, <family>-latest alias, or OpenRouter slash id (or RAFIKI_MODEL)")
	cmd.Flags().String("session", os.Getenv("RAFIKI_SESSION"), "X-Rafiki-Session id correlating this session's turns onto one conversation")
	return cmd
}

// claudeInvocation is the assembled environment and argument list for exec'ing
// the claude binary. The environment itself is built by pkg/proxyenv, shared
// with the daemon's --kind claude children so the two cannot drift.
type claudeInvocation struct {
	Env  []string
	Args []string
}

// claudeAutoCompactWindow resolves model's real context window from the
// disk-cached OpenRouter catalog and returns the CLAUDE_CODE_AUTO_COMPACT_WINDOW
// threshold for it, or 0 to leave Claude Code's default alone.
//
// Best-effort by design: an unreachable catalog or an unknown model returns 0
// rather than failing the launch, and the whole lookup is bounded so a slow
// network cannot delay starting a session.
func claudeAutoCompactWindow(ctx context.Context, model string, cacheDir string) int {
	store := routing.FileSnapshotStore{Path: filepath.Join(cacheDir, "openrouter_catalog.json")}
	cat := routing.NewModelCatalog(
		&http.Client{Timeout: claudeCatalogTimeout},
		claudeCatalogTTL,
		slog.New(slog.DiscardHandler),
	).WithCache(store)

	ctx, cancel := context.WithTimeout(ctx, claudeCatalogBudget)
	defer cancel()
	done := make(chan struct{})
	go func() { cat.Warm(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		return 0 // catalog too slow; don't delay the launch
	}

	ctxLen, maxComp, ok := cat.ContextWindow(model)
	if !ok {
		return 0
	}
	return routing.AutoCompactWindow(ctxLen, maxComp)
}

// runClaude runs `rafiki claude [flags] [-- claude flags...]`.
func runClaude(cmd *cobra.Command, args []string) error {
	url, _ := cmd.Flags().GetString("url")
	token, _ := cmd.Flags().GetString("token")
	model, _ := cmd.Flags().GetString("model")
	session, _ := cmd.Flags().GetString("session")

	if url == "" {
		return errors.New("--url (or RAFIKI_URL) is required")
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
		cache, err := os.UserCacheDir()
		if err == nil {
			autoCompact = claudeAutoCompactWindow(context.Background(), model, filepath.Join(cache, "rafiki"))
		}
	}

	env, modelArgs := proxyenv.Claude(os.Environ(), proxyenv.ClaudeOptions{
		URL:               url,
		Token:             token,
		Model:             model,
		AutoCompactWindow: autoCompact,
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
