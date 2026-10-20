// SPDX-License-Identifier: Apache-2.0

package integration_test

// The notification path end to end: a third party (or the operator's own CLI)
// names ONE top-level agent by name, the daemon resolves that name against the
// caller's live top-level children, renders the notification, and queues it in
// the agent's DURABLE INBOX before answering.
//
// The observation is the inbox row itself (`conversations.agent_inbox`), read
// through openPool. That is the delivery the whole feature promises: PushDurable
// persists the rendered fragment before Notify returns, so the row exists the
// moment the CLI exits 0 — no polling, no live model call, and no dependence on
// the child actually running a turn (the fundi engine would emit a user_message
// for the same text, but only after dequeuing it, which this suite's
// noRealProviderEnv daemons make an unnecessary 401 round trip). The rows are
// swept on cleanup with the existing dropAutoBgInboxRows, because agent_inbox
// carries no daemon_id for dropDaemonRows to reach.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"
)

// rafikidUser runs a `rafikid user ...` verb against the SAME DSN the daemon
// under test uses (RAFIKI_DB, which bootDaemonDB sets to RAFIKI_TEST_DSN) and
// returns its stdout. The verb opens the DSN directly — no daemon is contacted —
// so it is the only way to mint a NARROWED token: a capability set is populated
// from the token row alone and never from a request field, so there is no
// Connect call that could ask for one.
func rafikidUser(t *testing.T, dsn string, args ...string) string {
	t.Helper()
	cmd := exec.Command(daemonBinary(), append([]string{"user"}, args...)...)
	cmd.Env = subprocessEnv("RAFIKI_DB=" + dsn)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	assert.NewAborting(t).NoError(err, "rafikid user %v: %v\nstderr: %s", args, err, stderr.String())
	return string(out)
}

// tokenLine pulls the plaintext out of a user verb's "token: <plaintext>" line.
// Both `user create --token` and `user token mint` print it there, and nowhere
// else.
func tokenLine(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "token: "); ok && v != "" {
			return v
		}
	}
	t.Fatalf("no \"token: ...\" line in:\n%s", out)
	return ""
}

// writeProfileToken drops a credential into a scratch profile's token file —
// the same path `rafiki user create` writes, and the one pkg/profile's
// ReadToken reads (profile.TokenFile).
func writeProfileToken(t *testing.T, configDir, token string) {
	t.Helper()
	dir := filepath.Join(configDir, "rafiki", "profiles", "it")
	c := assert.NewAborting(t)
	c.NoError(os.MkdirAll(dir, 0o700), "mkdir %s", dir)
	c.NoError(os.WriteFile(filepath.Join(dir, "token"), []byte(token+"\n"), 0o600), "write token file")
}

// notifyInboxBody returns the body of the newest durable inbox row notify
// wrote for childID under source and key, failing the test when there is none.
// Keyed by the per-run-unique child id, so it can only ever read this run's row.
func notifyInboxBody(t *testing.T, pool *pgxpool.Pool, childID, source, key string) string {
	t.Helper()
	c := assert.NewAborting(t)
	var body, mode string
	err := pool.QueryRow(context.Background(), `
		SELECT body, mode
		  FROM conversations.agent_inbox
		 WHERE child_id = $1 AND source = $2 AND coalesce_key = $3
		 ORDER BY accepted_at DESC, id DESC
		 LIMIT 1`, childID, "notify:"+source, key).Scan(&body, &mode)
	c.NoError(err, "no durable inbox row for child %s under notify:%s/%s — notify answered success without storing the fragment", childID, source, key)
	c.Eq("prompt", mode, "a notification is a PROMPT-mode row, got %q", mode)
	return body
}

// spawnNamedAgent creates one top-level fundi agent through the real CLI, under
// configDir ("" = a fresh anonymous profile), and returns its id. The model is
// never actually called: these tests exercise name resolution and the durable
// inbox, so the throwaway model string only has to let the engine construct.
func spawnNamedAgent(t *testing.T, d *daemon, dsn, configDir, name string) string {
	t.Helper()
	c := assert.NewAborting(t)
	var stderr bytes.Buffer
	cmd := cliCmdIn(t, d, configDir, "--output", "json", "create", name,
		"--kind", "fundi",
		"--cwd", t.TempDir(),
		"--model", "anthropic/claude-sonnet-4-5",
		"--no-session",
		"--no-extensions",
		"--no-local-executor",
		"--detached",
	)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	c.NoError(err, "create %s: %v\nstderr: %s", name, err, stderr.String())
	var resp struct {
		ChildID string `json:"childId"`
	}
	c.Require().NoError(json.Unmarshal(out, &resp), "decode create output %q", out)
	c.NotEq("", resp.ChildID, "create %s returned an empty childId; output: %s", name, out)
	// The inbox rows notify writes outlive the child, and dropDaemonRows
	// (armed by bootDaemonDB) only reaches conversations.child.
	dropAutoBgInboxRows(t, dsn, resp.ChildID)
	return resp.ChildID
}

// TestNotifyEndToEnd is the whole path a notification travels: the operator's
// own CLI, a notify-scoped token that can do nothing but Notify, an unknown
// target, and the name a live agent already holds.
func TestNotifyEndToEnd(t *testing.T) {
	t.Parallel()

	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	d := bootDaemonDB(t, nextDaemonID())
	pool := openPool(t, dsn)
	defer pool.Close()

	// Per-run unique in BOTH parts, and the suite shares one database across
	// runs: a name from an earlier run that was never closed would still hold
	// the target name this run's notify must resolve (and the name name_taken
	// expects to be held).
	unique := uniqueSuffix()
	name := "n-" + unique

	t.Run("operator_cli", func(t *testing.T) {
		c := assert.NewAborting(t)
		childID := spawnNamedAgent(t, d, dsn, "", name)

		var stderr bytes.Buffer
		cmd := cliCmd(t, d, "notify", name, "--source", "e2e", "--key", "k1", "status=failed")
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		c.NoError(err, "notify %s: %v\nstderr: %s", name, err, stderr.String())
		c.StrContains(string(out), childID, "notify must print the notified child's id; stdout = %q", out)

		body := notifyInboxBody(t, pool, childID, "e2e", "k1")
		c.StrContains(body, `<notification source="e2e" key="k1"`,
			"the stored fragment is not the notification's opening element: %s", body)
		c.StrContains(body, `"status":"failed"`, "the stored fragment lost its fields: %s", body)
	})

	// An agent owned by a DIFFERENT user, for the notify-scoped token: a
	// notification is addressed at the CALLER'S owner, so a token minted for
	// another user must not reach the anonymous (socket-local) agent above.
	t.Run("notify_token_confined", func(t *testing.T) {
		c := assert.NewAborting(t)
		username := "notify-" + unique
		// The user and its full-authority token come from the direct-DSN CLI,
		// against the daemon's own database, so the daemon authenticates it on
		// the next request with no reload.
		fullToken := tokenLine(t, rafikidUser(t, dsn, "create", username, "--token"))

		// The narrowed token: exactly {notify}. It names the same user, so the
		// notification still lands on that user's agents — it just cannot do
		// anything else.
		minted := rafikidUser(t, dsn, "token", "mint", username,
			"--capability", "notify", "--name", "notify-e2e-"+unique)
		notifyToken := tokenLine(t, minted)
		c.StrContains(minted, "capabilities: notify", "the mint did not describe a narrow token:\n%s", minted)

		// Two scratch profiles, each its own XDG_CONFIG_HOME: the agent is
		// spawned with the user's full credential, and every notifying call
		// uses the notify-scoped one — a profile pointed at the daemon that
		// cannot list children is the whole point.
		ownerDir := t.TempDir()
		writeCliProfile(t, ownerDir, d.socketPath)
		writeProfileToken(t, ownerDir, fullToken)
		notifyDir := t.TempDir()
		writeCliProfile(t, notifyDir, d.socketPath)
		writeProfileToken(t, notifyDir, notifyToken)

		targetName := "n-confined-" + unique
		childID := spawnNamedAgent(t, d, dsn, ownerDir, targetName)

		var notifyStderr bytes.Buffer
		notifyCmd := cliCmdIn(t, d, notifyDir, "notify", targetName, "--source", "e2e", "--key", "confined", "ok=true")
		notifyCmd.Stderr = &notifyStderr
		out, err := notifyCmd.Output()
		c.NoError(err, "notify with a notify-scoped token must succeed: %v\nstderr: %s", err, notifyStderr.String())
		c.StrContains(string(out), childID, "notify under the narrowed token printed %q", out)
		body := notifyInboxBody(t, pool, childID, "e2e", "confined")
		c.StrContains(body, `<notification source="e2e" key="confined"`, "the narrowed token's fragment: %s", body)

		// Everything else is closed to it — the read-only ListChildren a full
		// user credential would be admitted to included.
		var listStderr bytes.Buffer
		listCmd := cliCmdIn(t, d, notifyDir, "ls")
		listCmd.Stderr = &listStderr
		lout, err := listCmd.Output()
		c.Error(err, "`rafiki ls` with a notify-scoped token must fail; stdout: %s", lout)
		c.StrContains(listStderr.String(), "permission_denied",
			"a notify-scoped token's ListChildren must be refused as a permission error; stderr: %s", listStderr.String())
		c.StrContains(listStderr.String(), "may only call Notify", "the refusal should name what the token may do; stderr: %s", listStderr.String())

		// The same confinement on the LLM proxy face, which is not Connect and
		// so is enforced by the token middleware rather than the policy gate.
		c.NotEq("", d.proxyURL, "the daemon never announced its proxy face; nothing can post to /v1/messages")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.proxyURL+"/v1/messages", http.NoBody)
		c.Require().NoError(err, "build POST /v1/messages")
		req.Header.Set("Authorization", "Bearer "+notifyToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		c.Require().NoError(err, "POST %s/v1/messages", d.proxyURL)
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		c.Eq(http.StatusForbidden, resp.StatusCode,
			"POST /v1/messages with a notify-scoped bearer: body = %s", raw)
	})

	t.Run("unknown_target", func(t *testing.T) {
		c := assert.NewAborting(t)
		var stderr bytes.Buffer
		cmd := cliCmd(t, d, "notify", "does-not-exist-"+unique, "--source", "e2e", "x=y")
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		c.Error(err, "notify to an unknown agent must exit non-zero; stdout: %s", out)
		c.StrContains(stderr.String(), "no live top-level agent named",
			"the not-found refusal must name the missing target; stderr: %s", stderr.String())
	})

	// A live sibling holds the name operator_cli's agent was given, so a second
	// spawn of the same name is refused with the holder named — not suffixed
	// (suffixing is the daemon's own TOOL spawners' rule, never a user's
	// explicit name) and never silently landed on a second child.
	t.Run("name_taken", func(t *testing.T) {
		c := assert.NewAborting(t)
		var stderr bytes.Buffer
		cmd := cliCmd(t, d, "create", name,
			"--kind", "fundi",
			"--cwd", t.TempDir(),
			"--model", "anthropic/claude-sonnet-4-5",
			"--no-session",
			"--no-extensions",
			"--no-local-executor",
			"--detached",
		)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		c.Error(err, "create with a held name must fail; stdout: %s", out)
		c.StrContains(stderr.String(), "is held by", "the refusal must name the holder; stderr: %s", stderr.String())
	})
}
