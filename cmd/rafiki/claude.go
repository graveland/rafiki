// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

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

// claudeManagedEnv are the variables this launcher sets deliberately. They are
// stripped from the inherited environment first so that launching a session
// from inside one does not silently inherit the parent's base URL, model or
// correlation header — which would land the child's turns on the parent's
// captured conversation and make a nested run look like a continuation of the
// outer one.
var claudeManagedEnv = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_CUSTOM_HEADERS",
	"ANTHROPIC_CUSTOM_MODEL_OPTION",
	"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME",
	"ANTHROPIC_MODEL",
	"CLAUDE_CODE_AUTO_COMPACT_WINDOW",
}

// claudeInvocation is the assembled environment and argument list for exec'ing
// the claude binary.
type claudeInvocation struct {
	Env  []string
	Args []string
}

// buildClaudeInvocation assembles the env and args for launching Claude Code
// against a rafiki proxy at url with the given bearer token.
//
// model may be empty (Claude Code picks its own). autoCompactWindow of 0 leaves
// Claude Code's default in place. passthrough is the user's own claude flags.
func buildClaudeInvocation(environ []string, url, token, sessionID, model string, autoCompactWindow int, passthrough []string) claudeInvocation {
	env := make([]string, 0, len(environ)+8)
	for _, e := range environ {
		k, _, _ := strings.Cut(e, "=")
		if slices.Contains(claudeManagedEnv, k) {
			continue // set explicitly below; drop the inherited copy
		}
		// The real Anthropic key must not reach a proxied child: Claude Code
		// would present it as x-api-key, which bypasses the bearer the proxy
		// authenticates on and defeats the capture the proxy exists for. The
		// OpenRouter key is the server's business and never the client's.
		if k == "ANTHROPIC_API_KEY" || k == "OPENROUTER_API_KEY" {
			continue
		}
		env = append(env, e)
	}

	env = append(env,
		"ANTHROPIC_BASE_URL="+url,
		"ANTHROPIC_AUTH_TOKEN="+token,
	)
	if sessionID != "" {
		// Correlates every turn of this session onto ONE captured
		// conversation; without it the proxy falls back to one conversation
		// per request.
		env = append(env, "ANTHROPIC_CUSTOM_HEADERS=X-Rafiki-Session: "+sessionID)
	}

	var args []string
	if model != "" {
		// Register the model as a custom /model option rather than setting
		// ANTHROPIC_MODEL or passing a bare --model. Claude Code validates
		// those against a client-side allowlist of Anthropic ids and rejects
		// anything else BEFORE a request ever leaves the client, which makes
		// every OpenRouter slash id and every <family>-latest alias
		// unreachable. A registered custom option is sent verbatim, leaving
		// resolution to the proxy, which is the only side that can do it.
		env = append(env,
			"ANTHROPIC_CUSTOM_MODEL_OPTION="+model,
			"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME=rafiki: "+model,
		)
		args = append(args, "--model", model)
		// Claude Code assumes a 200K context for a proxied model it cannot
		// verify, so it compacts too early or too late for the real window.
		if autoCompactWindow > 0 {
			env = append(env, fmt.Sprintf("CLAUDE_CODE_AUTO_COMPACT_WINDOW=%d", autoCompactWindow))
		}
	}
	args = append(args, passthrough...)
	return claudeInvocation{Env: env, Args: args}
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

// claudeCmd runs `rafiki claude [flags] [-- claude flags...]`.
func claudeCmd(args []string) error {
	fs := flag.NewFlagSet("claude", flag.ExitOnError)
	url := fs.String("url", envOr("RAFIKI_URL", "http://localhost:8035"), "rafiki proxy base URL (or RAFIKI_URL)")
	token := fs.String("token", envOr("RAFIKI_TOKEN", "dev"), "static bearer token for the proxy (or RAFIKI_TOKEN)")
	model := fs.String("model", os.Getenv("RAFIKI_MODEL"), "model id, <family>-latest alias, or OpenRouter slash id (or RAFIKI_MODEL)")
	session := fs.String("session", os.Getenv("RAFIKI_SESSION"), "X-Rafiki-Session id correlating this session's turns onto one conversation")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `usage: rafiki claude [flags] [-- claude flags...]

Launch Claude Code against a rafiki proxy, with capture, OpenRouter failover
and model resolution. Everything after -- is passed to claude verbatim.

  rafiki claude --model glm-5.2 -- --permission-mode plan

`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *url == "" {
		return errors.New("--url (or RAFIKI_URL) is required")
	}
	// Preflight so a dead proxy is a clear message here rather than an opaque
	// connection error from inside Claude Code after it has taken the TTY.
	if err := claudePreflight(*url); err != nil {
		return err
	}

	sessionID := *session
	if sessionID == "" {
		sessionID = uuid.NewString()
	}

	autoCompact := 0
	if *model != "" {
		cache, err := os.UserCacheDir()
		if err == nil {
			autoCompact = claudeAutoCompactWindow(context.Background(), *model, filepath.Join(cache, "rafiki"))
		}
	}

	inv := buildClaudeInvocation(os.Environ(), *url, *token, sessionID, *model, autoCompact, fs.Args())
	return execClaude(inv)
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
