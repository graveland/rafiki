package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// cliStateDir is a scratch XDG_STATE_HOME shared by every CLI invocation in
// this package. One directory rather than one per call: the tests do not
// assert on its contents, they only need it to not be the real one.
func cliStateDir() string {
	cliStateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "rafiki-cli-state-")
		if err != nil {
			panic(err)
		}
		cliState = dir
	})
	return cliState
}

var (
	cliStateOnce sync.Once
	cliState     string
)

// writeCliProfile writes a minimal profiles.toml + current-profile pointer
// naming a single local profile "it" at socketPath, under configDir's own
// "rafiki" leaf — pkg/paths.ConfigDir() is $XDG_CONFIG_HOME/rafiki, not
// $XDG_CONFIG_HOME itself, and profile.ProfilesFile()/PointerFile() resolve
// through it. This is how these tests aim the real CLI binary at a scratch
// daemon now that --socket is gone (client dialing is entirely profile-driven
// — see pkg/profile and cmd/rafiki's mustDial/newConnectEndpoint).
func writeCliProfile(t *testing.T, configDir, socketPath string) {
	t.Helper()
	dir := filepath.Join(configDir, "rafiki")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	manifest := fmt.Sprintf("[profile.it]\nsocket = %q\n", socketPath)
	if err := os.WriteFile(filepath.Join(dir, "profiles.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write profiles.toml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current-profile"), []byte("it\n"), 0o600); err != nil {
		t.Fatalf("write current-profile: %v", err)
	}
}

// cliCmd builds a rafiki invocation against d's daemon (nil for a --help-only
// call that dials nothing), with the ambient RAFIKI_URL/RAFIKI_TOKEN/etc
// BLANKED — those are retired variables the client now refuses outright (see
// profile.CheckRetiredEnv) — and XDG_CONFIG_HOME pointed at a fresh scratch
// directory holding a profile manifest for d's socket.
//
// The config dir is fresh PER CALL (t.TempDir()), not shared across a test's
// several cliCmd invocations or across tests: every test here calls
// t.Parallel(), and two daemons sharing one profiles.toml would race each
// other's writes and occasionally dial the wrong socket.
//
// Later entries win for a duplicate exec.Cmd.Env key, so appending after
// os.Environ() overrides whatever the shell exported.
func cliCmd(t *testing.T, d *daemon, args ...string) *exec.Cmd {
	t.Helper()
	configDir := t.TempDir()
	if d != nil {
		writeCliProfile(t, configDir, d.socketPath)
	}
	cmd := exec.Command(cliPath, args...)
	// XDG_STATE_HOME too: `rafiki create` records the model it spawned into
	// the client state file, so an un-isolated run writes a remembered model
	// into the DEVELOPER's real preferences from a test daemon's fixture.
	cmd.Env = append(os.Environ(),
		"RAFIKI_URL=", "RAFIKI_TOKEN=", "RAFIKI_SOCKET=",
		"RAFIKI_DEFAULT_MODEL=", "RAFIKI_DEFAULT_PRESET=", "RAFIKI_DEFAULT_LABELS=",
		// RAFIKI_PROFILE is live, not retired, but blanking it matters just as
		// much: an ambient RAFIKI_PROFILE naming a profile that doesn't exist
		// in the scratch manifest above would make mustProfile fail (and, for
		// any RunE path that calls it, os.Exit(2) — killing this whole
		// subprocess rather than just this one command).
		"RAFIKI_PROFILE=",
		"XDG_STATE_HOME="+cliStateDir(),
		"XDG_CONFIG_HOME="+configDir,
	)
	return cmd
}

// TestCLI_Status verifies that `rafiki status` returns a JSON object containing
// a "version" field.
func TestCLI_Status(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	cmd := cliCmd(t, d, "--output", "json", "status")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), `"version"`) {
		t.Fatalf("status output missing version field: %s", out)
	}
}

// TestCLI_CreateListKillForget exercises the core child lifecycle via the CLI:
// create --detached → list (child present) → stop → poll for exited (still
// listed) → close (gone). `stop` no longer auto-closes on a clean exit — that
// composition moved to `close` — so the two steps are asserted separately.
func TestCLI_CreateListKillForget(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	// create --detached
	var createStderr bytes.Buffer
	createCmd := cliCmd(t, d,
		"--output", "json",
		"create", "smoke",
		"--cwd", "/tmp",
		"--no-session",
		"--model", "anthropic/claude-sonnet-4-5",
		"--no-extensions",
		"--no-local-executor",
		"--detached",
	)
	createCmd.Stderr = &createStderr
	out, err := createCmd.Output() // stdout only
	if err != nil {
		t.Fatalf("create --detached failed: %v\nstderr: %s", err, createStderr.String())
	}

	var spawnResp struct {
		ChildID string `json:"childId"`
	}
	if err := json.Unmarshal(out, &spawnResp); err != nil {
		t.Fatalf("decode create response: %v\n%s", err, out)
	}
	childID := spawnResp.ChildID
	if childID == "" {
		t.Fatalf("create --detached returned empty childId; output: %s", out)
	}

	// list — child should be present
	listCmd := cliCmd(t, d, "--output", "json", "list")
	out, err = listCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), childID) {
		t.Fatalf("list missing childId %s: %s", childID, out)
	}

	// stop
	stopCmd := cliCmd(t, d, "stop", "smoke")
	out, err = stopCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}

	// poll until status=exited (up to 5 seconds)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		getCmd := cliCmd(t, d, "--output", "json", "get", "smoke")
		out, _ = getCmd.CombinedOutput()
		if strings.Contains(string(out), `"status":"exited"`) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// `rafiki stop` no longer closes: `smoke` must still be listed, exited.
	// These gets ask for JSON explicitly, which renders indented — hence the space.
	getCmd := cliCmd(t, d, "--output", "json", "get", "smoke")
	out, err = getCmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"status": "exited"`) {
		t.Fatalf("expected smoke to still be listed as exited after stop; get output: %s (err=%v)", out, err)
	}

	// close finalizes it.
	closeCmd := cliCmd(t, d, "close", "smoke")
	out, err = closeCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("close failed: %v\n%s", err, out)
	}
	getCmd = cliCmd(t, d, "get", "smoke")
	out, _ = getCmd.CombinedOutput()
	if !strings.Contains(string(out), "no child matches") {
		t.Fatalf("expected child to be gone after close; get output: %s", out)
	}
}

// MANUAL SMOKE PROCEDURE (not run in CI):
//
//	# Boot daemon
//	./bin/rafikid &
//	sleep 1
//
//	# Spawn a child interactively (attaches a TUI)
//	./bin/rafiki create demo --cwd /tmp --no-extensions \
//	    --model anthropic/claude-haiku-4-5
//
//	# In the TUI:
//	- Verify the pi welcome screen renders
//	- Type "Say hello" and press Enter
//	- Verify the assistant streams a response
//	- Press Ctrl+D to detach
//
//	# In another shell, verify the child is still alive:
//	./bin/rafiki list
//	# status should be "idle" (the agent finished its turn)
//
//	# Reattach
//	./bin/rafiki attach demo
//	# Verify the TUI re-renders the conversation history
//
//	# Quit with native semantics
//	./bin/rafiki attach demo --kill-on-exit
//	# Press Ctrl+D
//	./bin/rafiki list
//	# demo should now be "exited"
//
//	./bin/rafiki forget demo

// TestCLI_CreateDetached verifies that `rafiki create --detached` spawns a child
// and returns JSON containing a childId, then confirms the child appears in
// `rafiki list`. Cleans up via stop + close (stop no longer auto-closes).
func TestCLI_CreateDetached(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	// create --detached: should spawn the child and print JSON without attaching.
	// The explicit --output json is the wave-4 contract: auto on a pipe now
	// means table, so a consumer unmarshaling stdout must ask for JSON.
	// Capture stdout and stderr separately — rafiki may emit a best-effort warning
	// on stderr (e.g. active-marker directory not found) that we don't want to
	// confuse with the JSON payload on stdout.
	createCmd := cliCmd(t, d,
		"--output", "json",
		"create", "test-detached",
		"--cwd", "/tmp",
		"--no-session",
		"--no-extensions",
		"--model", "anthropic/claude-sonnet-4-5",
		"--no-local-executor",
		"--detached",
	)
	var createStderr bytes.Buffer
	createCmd.Stderr = &createStderr
	out, err := createCmd.Output() // stdout only
	if err != nil {
		t.Fatalf("create --detached failed: %v\nstderr: %s", err, createStderr.String())
	}

	var createResp struct {
		ChildID string `json:"childId"`
	}
	if err := json.Unmarshal(out, &createResp); err != nil {
		t.Fatalf("decode create response: %v\noutput: %s", err, out)
	}
	childID := createResp.ChildID
	if childID == "" {
		t.Fatalf("create --detached returned empty childId; output: %s", out)
	}

	// list — child should appear.
	listCmd := cliCmd(t, d, "--output", "json", "list")
	out, err = listCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), childID) {
		t.Fatalf("list missing childId %s: %s", childID, out)
	}

	// stop
	stopCmd := cliCmd(t, d, "stop", "test-detached")
	out, err = stopCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("stop failed: %v\n%s", err, out)
	}

	// poll until status=exited (up to 5 seconds).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		getCmd := cliCmd(t, d, "--output", "json", "get", "test-detached")
		out, _ = getCmd.CombinedOutput()
		if strings.Contains(string(out), `"status":"exited"`) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// `rafiki stop` no longer closes: the child must still be listed, exited.
	// These gets ask for JSON explicitly, which renders indented — hence the space.
	getCmd := cliCmd(t, d, "--output", "json", "get", "test-detached")
	out, err = getCmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"status": "exited"`) {
		t.Fatalf("expected test-detached to still be listed as exited after stop; get output: %s (err=%v)", out, err)
	}

	// close finalizes it.
	closeCmd := cliCmd(t, d, "close", "test-detached")
	out, err = closeCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("close failed: %v\n%s", err, out)
	}
	getCmd = cliCmd(t, d, "get", "test-detached")
	out, _ = getCmd.CombinedOutput()
	if !strings.Contains(string(out), "no child matches") {
		t.Fatalf("expected child to be gone after close; get output: %s", out)
	}
}

// TestCLI_AttachHelp verifies that `rafiki attach --help` exits cleanly and
// documents the --kill-on-exit flag.
func TestCLI_AttachHelp(t *testing.T) {
	t.Parallel()

	cmd := cliCmd(t, nil, "attach", "--help")
	out, err := cmd.CombinedOutput()
	// cobra exits 0 for --help.
	if err != nil {
		t.Fatalf("attach --help failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "--kill-on-exit") {
		t.Fatalf("attach --help missing --kill-on-exit flag; output: %s", out)
	}
}

// TestCLI_CreateHelp verifies that `rafiki create --help` exits cleanly and
// documents both --detached and --kill-on-exit flags.
func TestCLI_CreateHelp(t *testing.T) {
	t.Parallel()

	cmd := cliCmd(t, nil, "create", "--help")
	out, err := cmd.CombinedOutput()
	// cobra exits 0 for --help.
	if err != nil {
		t.Fatalf("create --help failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "--detached") {
		t.Fatalf("create --help missing --detached flag; output: %s", out)
	}
	if !strings.Contains(string(out), "--kill-on-exit") {
		t.Fatalf("create --help missing --kill-on-exit flag; output: %s", out)
	}
}

// TestCLI_ResolveByPrefix verifies that a child can be addressed by a prefix
// of its name (e.g. "afk" resolves "afk-impl").
func TestCLI_ResolveByPrefix(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	var createStderr bytes.Buffer
	createCmd := cliCmd(t, d,
		"--output", "json",
		"create", "afk-impl",
		"--cwd", "/tmp",
		"--no-session",
		"--no-extensions",
		"--model", "anthropic/claude-sonnet-4-5",
		"--no-local-executor",
		"--detached",
	)
	createCmd.Stderr = &createStderr
	if _, err := createCmd.Output(); err != nil { // stdout only
		t.Fatalf("create --detached failed: %v\nstderr: %s", err, createStderr.String())
	}

	// resolve by prefix "afk"
	getCmd := cliCmd(t, d, "--output", "json", "get", "afk")
	out, err := getCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("get with prefix failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "afk-impl") {
		t.Fatalf("expected afk-impl in get output: %s", out)
	}

	// cleanup: kill before test exits to avoid leftover processes
	killCmd := cliCmd(t, d, "kill", "afk-impl")
	_, _ = killCmd.CombinedOutput()
}

// TestCLI_BudgetSet exercises `rafiki budget set` end to end against a real
// daemon: create a detached child, set a cap, read it back through `get`'s
// ChildSummary.max_cost, then clear it with --unlimited (0/unlimited is an
// accepted, intentional value on the operator path).
func TestCLI_BudgetSet(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	// create --detached — no LLM call, just a child row to budget against.
	var createStderr bytes.Buffer
	createCmd := cliCmd(t, d,
		"--output", "json",
		"create", "budget-smoke",
		"--cwd", "/tmp",
		"--no-session",
		"--no-extensions",
		"--model", "anthropic/claude-sonnet-4-5",
		"--no-local-executor",
		"--detached",
	)
	createCmd.Stderr = &createStderr
	out, err := createCmd.Output() // stdout only
	if err != nil {
		t.Fatalf("create --detached failed: %v\nstderr: %s", err, createStderr.String())
	}

	var createResp struct {
		ChildID string `json:"childId"`
	}
	if err := json.Unmarshal(out, &createResp); err != nil {
		t.Fatalf("decode create response: %v\noutput: %s", err, out)
	}
	childID := createResp.ChildID
	if childID == "" {
		t.Fatalf("create --detached returned empty childId; output: %s", out)
	}

	// budget set 5.00
	var setStderr bytes.Buffer
	setCmd := cliCmd(t, d, "budget", "set", childID, "5.00")
	setCmd.Stderr = &setStderr
	setOut, err := setCmd.Output()
	if err != nil {
		t.Fatalf("budget set: %v (stderr: %s)", err, setStderr.String())
	}
	if !strings.Contains(string(setOut), "5.00") {
		t.Fatalf("budget set output %q does not mention the new amount", setOut)
	}

	// read the cap back through get — asked for JSON explicitly, which renders
	// indented.
	getCmd := cliCmd(t, d, "--output", "json", "get", childID)
	getOut, err := getCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("get: %v\n%s", err, getOut)
	}
	var got struct {
		MaxCost *float64 `json:"max_cost"`
	}
	if err := json.Unmarshal(getOut, &got); err != nil {
		t.Fatalf("decode get output %q: %v", getOut, err)
	}
	if got.MaxCost == nil || *got.MaxCost != 5.00 {
		t.Fatalf("get after budget set: max_cost = %v, want 5.00", got.MaxCost)
	}

	// budget set --unlimited clears the cap.
	var unlimitedStderr bytes.Buffer
	unlimitedCmd := cliCmd(t, d, "budget", "set", childID, "--unlimited")
	unlimitedCmd.Stderr = &unlimitedStderr
	unlimitedOut, err := unlimitedCmd.Output()
	if err != nil {
		t.Fatalf("budget set --unlimited: %v (stderr: %s)", err, unlimitedStderr.String())
	}
	if !strings.Contains(string(unlimitedOut), "unlimited") {
		t.Fatalf("budget set --unlimited output %q does not confirm the clear", unlimitedOut)
	}

	// cleanup: kill to avoid leftover processes
	killCmd := cliCmd(t, d, "kill", "budget-smoke")
	_, _ = killCmd.CombinedOutput()
}

// jsonlTestLabel is the label every child this test creates carries, so the
// list assertions below are immune to children OTHER tests' daemons create
// concurrently: the test daemons share one database and ctrl_list is not
// daemon-scoped (childstoredb's listSQL has no WHERE beyond deleted_at), so
// an unfiltered `rafiki list` sees the whole shared set and can change between
// two invocations as parallel tests spawn and exit their own children.
const jsonlTestLabel = "cli-tables=jsonl"

// splitJSONLines splits writeJSONL output into one string per record. A
// zero-row payload yields no lines (writeJSONL emits nothing, not "null").
func splitJSONLines(t *testing.T, out []byte) []string {
	t.Helper()
	trimmed := strings.TrimSuffix(string(out), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// TestCLI_JSONLAndTextModes pins the other two faces of the three-way output
// contract end to end against a real daemon:
//
//   - -J (JSONL) emits one compact, UNWRAPPED record per line for every
//     list-shaped verb (list, models, get), and its row count agrees with the
//     same verb's -o json envelope;
//   - the DEFAULT mode with piped stdout is now a TABLE, not JSON — the
//     wave-4 flip — proven by `rafiki tasks` with no flags, which no longer
//     parses as JSON and draws pkg/table's border instead;
//   - -j and -J together are a user-input error with an exact message.
//
// Every CLI stdout here is JSON only because a flag or shorthand asked for
// it: auto on a pipe must never be assumed machine-readable again.
func TestCLI_JSONLAndTextModes(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	// Two detached children, both carrying jsonlTestLabel. `--output json` is
	// itself part of the contract under test: the record must be asked for.
	names := []string{"jsonl-a", "jsonl-b"}
	for _, name := range names {
		var stderr bytes.Buffer
		cmd := cliCmd(t, d,
			"--output", "json",
			"create", name,
			"--cwd", "/tmp",
			"--no-session",
			"--no-extensions",
			"--model", "anthropic/claude-sonnet-4-5",
			"--no-local-executor",
			"--detached",
			"--label", jsonlTestLabel,
		)
		cmd.Stderr = &stderr
		out, err := cmd.Output() // stdout only
		if err != nil {
			t.Fatalf("create %s failed: %v\nstderr: %s", name, err, stderr.String())
		}
		var resp struct {
			ChildID string `json:"childId"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("decode create response for %s: %v\noutput: %s", name, err, out)
		}
		if resp.ChildID == "" {
			t.Fatalf("create %s returned empty childId; output: %s", name, out)
		}
	}
	t.Cleanup(func() {
		for _, name := range names {
			cmd := cliCmd(t, d, "kill", name)
			_, _ = cmd.CombinedOutput()
		}
	})

	// ── list: -J line count == -o json children count ─────────────────────
	jsonOut, err := cliCmd(t, d, "--output", "json", "list", "--label", jsonlTestLabel).Output()
	if err != nil {
		t.Fatalf("list -o json failed: %v", err)
	}
	var envelope struct {
		Children []protocol.ChildSummary `json:"children"`
	}
	if err := json.Unmarshal(jsonOut, &envelope); err != nil {
		t.Fatalf("decode list -o json envelope: %v\noutput: %s", err, jsonOut)
	}

	jsonlOut, err := cliCmd(t, d, "-J", "list", "--label", jsonlTestLabel).Output()
	if err != nil {
		t.Fatalf("list -J failed: %v", err)
	}
	lines := splitJSONLines(t, jsonlOut)
	if len(lines) != len(envelope.Children) {
		t.Fatalf("list -J emitted %d lines for %d children in -o json's envelope; JSONL must carry every row",
			len(lines), len(envelope.Children))
	}
	for i, line := range lines {
		var ch protocol.ChildSummary
		if err := json.Unmarshal([]byte(line), &ch); err != nil {
			t.Fatalf("list -J line %d is not a bare ChildSummary object (JSONL must unwrap the envelope): %v\nline: %s", i, err, line)
		}
		if ch.ChildID == "" {
			t.Fatalf("list -J line %d has an empty childId: %s", i, line)
		}
	}

	// ── models: -J emits one ModelRow per line, none wrapped ─────────────
	modelsOut, err := cliCmd(t, d, "-J", "models").Output()
	if err != nil {
		t.Fatalf("models -J failed: %v", err)
	}
	modelLines := splitJSONLines(t, modelsOut)
	if len(modelLines) == 0 {
		t.Fatalf("models -J emitted no rows; the builtin source must always serve at least one model")
	}
	for i, line := range modelLines {
		var row struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("models -J line %d is not a ModelRow object: %v\nline: %s", i, err, line)
		}
		if row.ID == "" {
			t.Fatalf("models -J line %d has an empty id: %s", i, line)
		}
	}

	// ── get: -J with two targets emits two lines ─────────────────────────
	getOut, err := cliCmd(t, d, "-J", "get", names[0], names[1]).Output()
	if err != nil {
		t.Fatalf("get -J with two targets failed: %v", err)
	}
	getLines := splitJSONLines(t, getOut)
	if len(getLines) != 2 {
		t.Fatalf("get -J with two targets emitted %d lines, want 2\noutput: %s", len(getLines), getOut)
	}
	for i, line := range getLines {
		var ch protocol.ChildSummary
		if err := json.Unmarshal([]byte(line), &ch); err != nil {
			t.Fatalf("get -J line %d is not a bare ChildSummary object: %v\nline: %s", i, err, line)
		}
	}

	// ── get: a failing target never reaches stdout ───────────────────────
	// stdout and stderr must be captured separately — the point is which
	// stream carries the failure. The good sibling's row is still emitted
	// (data is never withheld because a target failed), the failed target is
	// named only on stderr, and the command exits nonzero.
	var badOut, badErrBuf bytes.Buffer
	badCmd := cliCmd(t, d, "-J", "get", names[0], "no-such-child-xyz")
	badCmd.Stdout = &badOut
	badCmd.Stderr = &badErrBuf
	if err := badCmd.Run(); err == nil {
		t.Fatalf("get with a failing target must exit nonzero; stdout: %s", badOut.String())
	}
	if badOut.Len() == 0 || !strings.Contains(badOut.String(), names[0]) {
		t.Fatalf("get -J should still emit the good sibling's row on stdout; got: %q", badOut.String())
	}
	if strings.Contains(badOut.String(), "no-such-child-xyz") {
		t.Fatalf("failing target leaked onto stdout: %s", badOut.String())
	}
	if !strings.Contains(badErrBuf.String(), "no-such-child-xyz") {
		t.Fatalf("failing target's diagnostic missing from stderr: %s", badErrBuf.String())
	}

	// ── tasks with NO flags: the default flip, end to end ─────────────────
	// auto on a pipe used to mean JSON; it now means table. A consumer that
	// pipes `rafiki tasks` and unmarshals must fail — and read a table border.
	tasksOut, err := cliCmd(t, d, "tasks").Output()
	if err != nil {
		t.Fatalf("tasks failed: %v", err)
	}
	var probe any
	if err := json.Unmarshal(tasksOut, &probe); err == nil {
		t.Fatalf("rafiki tasks with no flags must NOT parse as JSON after the default flip; output: %s", tasksOut)
	}
	firstLine := string(tasksOut)
	if i := strings.IndexByte(firstLine, '\n'); i >= 0 {
		firstLine = firstLine[:i]
	}
	if !strings.HasPrefix(firstLine, "┌") {
		t.Fatalf("rafiki tasks text output should start with a table border; first line: %q", firstLine)
	}

	// ── -j and -J together are refused, with the exact message ────────────
	var bothErrBuf bytes.Buffer
	bothCmd := cliCmd(t, d, "-j", "-J", "list")
	bothCmd.Stderr = &bothErrBuf
	if err := bothCmd.Run(); err == nil {
		t.Fatalf("combining -j and -J must fail; output: %s", bothErrBuf.String())
	}
	if !strings.Contains(bothErrBuf.String(), "cannot combine -j and -J") {
		t.Fatalf("-j -J error text changed; got: %s", bothErrBuf.String())
	}
}
