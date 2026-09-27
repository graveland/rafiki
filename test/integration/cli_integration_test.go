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

	"google.golang.org/protobuf/encoding/protojson"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
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
// — see pkg/profile and cmd/rafiki's newConnectEndpoint).
func writeCliProfile(t *testing.T, configDir, socketPath string) {
	t.Helper()
	c := assert.NewAborting(t)
	dir := filepath.Join(configDir, "rafiki")
	c.NoError(os.MkdirAll(dir, 0o700), "mkdir %s", dir)
	manifest := fmt.Sprintf("[profile.it]\nsocket = %q\n", socketPath)
	c.NoError(os.WriteFile(filepath.Join(dir, "profiles.toml"), []byte(manifest), 0o600), "write profiles.toml")
	c.NoError(os.WriteFile(filepath.Join(dir, "current-profile"), []byte("it\n"), 0o600), "write current-profile")
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
	return cliCmdIn(t, d, "", args...)
}

// cliCmdIn is cliCmd with a caller-supplied config dir: for the tests that
// need SEVERAL invocations to share one profile directory — a token file
// written by `rafiki user create` has to survive to the preset/create
// invocations that must run as that user. An empty dir gets a fresh temp dir,
// exactly like cliCmd.
func cliCmdIn(t *testing.T, d *daemon, configDir string, args ...string) *exec.Cmd {
	t.Helper()
	if configDir == "" {
		configDir = t.TempDir()
	}
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
	c := assert.NewAborting(t)
	d := bootDaemon(t)

	cmd := cliCmd(t, d, "--output", "json", "status")
	out, err := cmd.CombinedOutput()
	c.NoError(err, "status failed: %v\noutput: %s", err, out)
	c.StrContains(string(out), `"version"`, "status output missing version field: %s", out)
}

// TestCLI_CreateListKillForget exercises the core child lifecycle via the CLI:
// create --detached → list (child present) → stop → poll for exited (still
// listed) → close (gone). `stop` no longer auto-closes on a clean exit — that
// composition moved to `close` — so the two steps are asserted separately.
func TestCLI_CreateListKillForget(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
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
	c.NoError(err, "create --detached failed: %v\nstderr: %s", err, createStderr.String())

	var spawnResp struct {
		ChildID string `json:"childId"`
	}
	if err := json.Unmarshal(out, &spawnResp); err != nil {
		t.Fatalf("decode create response: %v\n%s", err, out)
	}
	childID := spawnResp.ChildID
	c.NotEq("", childID, "create --detached returned empty childId; output: %s", out)

	// list — child should be present
	listCmd := cliCmd(t, d, "--output", "json", "list")
	out, err = listCmd.CombinedOutput()
	c.NoError(err, "list failed: %v\n%s", err, out)
	c.StrContains(string(out), childID, "list missing childId %s: %s", childID, out)

	// stop
	stopCmd := cliCmd(t, d, "stop", "smoke")
	out, err = stopCmd.CombinedOutput()
	c.NoError(err, "stop failed: %v\n%s", err, out)

	// poll until status=exited (up to 5 seconds). `get` renders indented
	// protojson, so the needle carries the space after the colon.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		getCmd := cliCmd(t, d, "--output", "json", "get", "smoke")
		out, _ = getCmd.CombinedOutput()
		if strings.Contains(string(out), `"status": "exited"`) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// `rafiki stop` no longer closes: `smoke` must still be listed, exited.
	// These gets ask for JSON explicitly, which renders indented — hence the space.
	getCmd := cliCmd(t, d, "--output", "json", "get", "smoke")
	out, err = getCmd.CombinedOutput()
	c.False(err != nil || !strings.Contains(string(out), `"status": "exited"`), "expected smoke to still be listed as exited after stop; get output: %s (err=%v)", out, err)

	// close finalizes it.
	closeCmd := cliCmd(t, d, "close", "smoke")
	out, err = closeCmd.CombinedOutput()
	c.NoError(err, "close failed: %v\n%s", err, out)
	getCmd = cliCmd(t, d, "get", "smoke")
	out, _ = getCmd.CombinedOutput()
	c.StrContains(string(out), "no child matches", "expected child to be gone after close; get output: %s", out)

	// close on a LIVE child: stop-first-then-close in one verb. Whatever state
	// the child is in by the time close's stop lands (spawning or streaming),
	// the close must end with the child gone.
	liveCreate := cliCmd(t, d,
		"--output", "json",
		"create", "smoke-live",
		"--cwd", "/tmp",
		"--no-session",
		"--no-extensions",
		"--model", "anthropic/claude-sonnet-4-5",
		"--no-local-executor",
		"--detached",
	)
	if _, err := liveCreate.Output(); err != nil {
		t.Fatalf("create smoke-live failed: %v", err)
	}
	closeLive := cliCmd(t, d, "close", "smoke-live")
	if out, err = closeLive.CombinedOutput(); err != nil {
		t.Fatalf("close of a live child failed: %v\n%s", err, out)
	}
	getLive := cliCmd(t, d, "get", "smoke-live")
	out, _ = getLive.CombinedOutput()
	c.StrContains(string(out), "no child matches", "expected live child to be gone after close; get output: %s", out)
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
	c := assert.NewAborting(t)
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
	c.NoError(err, "create --detached failed: %v\nstderr: %s", err, createStderr.String())

	var createResp struct {
		ChildID string `json:"childId"`
	}
	if err := json.Unmarshal(out, &createResp); err != nil {
		t.Fatalf("decode create response: %v\noutput: %s", err, out)
	}
	childID := createResp.ChildID
	c.NotEq("", childID, "create --detached returned empty childId; output: %s", out)

	// list — child should appear.
	listCmd := cliCmd(t, d, "--output", "json", "list")
	out, err = listCmd.CombinedOutput()
	c.NoError(err, "list failed: %v\n%s", err, out)
	c.StrContains(string(out), childID, "list missing childId %s: %s", childID, out)

	// stop
	stopCmd := cliCmd(t, d, "stop", "test-detached")
	out, err = stopCmd.CombinedOutput()
	c.NoError(err, "stop failed: %v\n%s", err, out)

	// poll until status=exited (up to 5 seconds).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		getCmd := cliCmd(t, d, "--output", "json", "get", "test-detached")
		out, _ = getCmd.CombinedOutput()
		if strings.Contains(string(out), `"status": "exited"`) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// `rafiki stop` no longer closes: the child must still be listed, exited.
	// These gets ask for JSON explicitly, which renders indented — hence the space.
	getCmd := cliCmd(t, d, "--output", "json", "get", "test-detached")
	out, err = getCmd.CombinedOutput()
	c.False(err != nil || !strings.Contains(string(out), `"status": "exited"`), "expected test-detached to still be listed as exited after stop; get output: %s (err=%v)", out, err)

	// close finalizes it.
	closeCmd := cliCmd(t, d, "close", "test-detached")
	out, err = closeCmd.CombinedOutput()
	c.NoError(err, "close failed: %v\n%s", err, out)
	getCmd = cliCmd(t, d, "get", "test-detached")
	out, _ = getCmd.CombinedOutput()
	c.StrContains(string(out), "no child matches", "expected child to be gone after close; get output: %s", out)
}

// TestCLI_AttachHelp verifies that `rafiki attach --help` exits cleanly and
// documents the --kill-on-exit flag.
func TestCLI_AttachHelp(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	cmd := cliCmd(t, nil, "attach", "--help")
	out, err := cmd.CombinedOutput()
	// cobra exits 0 for --help.
	c.NoError(err, "attach --help failed: %v\noutput: %s", err, out)
	c.StrContains(string(out), "--kill-on-exit", "attach --help missing --kill-on-exit flag; output: %s", out)
}

// TestCLI_CreateHelp verifies that `rafiki create --help` exits cleanly and
// documents both --detached and --kill-on-exit flags.
func TestCLI_CreateHelp(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	cmd := cliCmd(t, nil, "create", "--help")
	out, err := cmd.CombinedOutput()
	// cobra exits 0 for --help.
	c.NoError(err, "create --help failed: %v\noutput: %s", err, out)
	c.StrContains(string(out), "--detached", "create --help missing --detached flag; output: %s", out)
	c.StrContains(string(out), "--kill-on-exit", "create --help missing --kill-on-exit flag; output: %s", out)
}

// TestCLI_ResolveByPrefix verifies that a child can be addressed by a prefix
// of its name (e.g. "afk" resolves "afk-impl").
func TestCLI_ResolveByPrefix(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
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
	c.NoError(err, "get with prefix failed: %v\n%s", err, out)
	c.StrContains(string(out), "afk-impl", "expected afk-impl in get output: %s", out)

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
	c := assert.NewAborting(t)
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
	c.NoError(err, "create --detached failed: %v\nstderr: %s", err, createStderr.String())

	var createResp struct {
		ChildID string `json:"childId"`
	}
	if err := json.Unmarshal(out, &createResp); err != nil {
		t.Fatalf("decode create response: %v\noutput: %s", err, out)
	}
	childID := createResp.ChildID
	c.NotEq("", childID, "create --detached returned empty childId; output: %s", out)

	// budget set 5.00
	var setStderr bytes.Buffer
	setCmd := cliCmd(t, d, "budget", "set", childID, "5.00")
	setCmd.Stderr = &setStderr
	setOut, err := setCmd.Output()
	c.NoError(err, "budget set: %v (stderr: %s)", err, setStderr.String())
	c.StrContains(string(setOut), "5.00", "budget set output %q does not mention the new amount", setOut)

	// read the cap back through get — asked for JSON explicitly, which renders
	// indented.
	getCmd := cliCmd(t, d, "--output", "json", "get", childID)
	getOut, err := getCmd.CombinedOutput()
	c.NoError(err, "get: %v\n%s", err, getOut)
	var got struct {
		MaxCost *float64 `json:"maxCost"`
	}
	c.NoError(json.Unmarshal(getOut, &got), "decode get output %q", getOut)
	c.False(got.MaxCost == nil || *got.MaxCost != 5.00, "get after budget set: max_cost = %v, want 5.00", got.MaxCost)

	// budget set --unlimited clears the cap.
	var unlimitedStderr bytes.Buffer
	unlimitedCmd := cliCmd(t, d, "budget", "set", childID, "--unlimited")
	unlimitedCmd.Stderr = &unlimitedStderr
	unlimitedOut, err := unlimitedCmd.Output()
	c.NoError(err, "budget set --unlimited: %v (stderr: %s)", err, unlimitedStderr.String())
	c.StrContains(string(unlimitedOut), "unlimited", "budget set --unlimited output %q does not confirm the clear", unlimitedOut)

	// cleanup: kill to avoid leftover processes
	killCmd := cliCmd(t, d, "kill", "budget-smoke")
	_, _ = killCmd.CombinedOutput()
}

// jsonlTestLabel is the label every child this test creates carries, so the
// list assertions below are immune to children OTHER tests' daemons create
// concurrently: the test daemons share one database and the list is not
// daemon-scoped (childstoredb's listSQL has no WHERE beyond closed_at), so
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
	c := assert.NewAborting(t)
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
		c.NoError(err, "create %s failed: %v\nstderr: %s", name, err, stderr.String())
		var resp struct {
			ChildID string `json:"childId"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("decode create response for %s: %v\noutput: %s", name, err, out)
		}
		c.NotEq("", resp.ChildID, "create %s returned empty childId; output: %s", name, out)
	}
	t.Cleanup(func() {
		for _, name := range names {
			cmd := cliCmd(t, d, "kill", name)
			_, _ = cmd.CombinedOutput()
		}
	})

	// ── list: -J line count == -o json children count ─────────────────────
	// Both faces are the canonical protojson now (camelCase; int64 fields as
	// strings), decoded here with protojson — encoding/json would refuse
	// string-rendered int64s.
	jsonOut, err := cliCmd(t, d, "--output", "json", "list", "--label", jsonlTestLabel).Output()
	c.NoError(err, "list -o json failed")
	var envelope rafikiv1.ListChildrenResponse
	if err := protojson.Unmarshal(jsonOut, &envelope); err != nil {
		t.Fatalf("decode list -o json envelope: %v\noutput: %s", err, jsonOut)
	}
	c.NotEmpty(envelope.GetChildren(), "list -o json returned no children for label %s", jsonlTestLabel)

	jsonlOut, err := cliCmd(t, d, "-J", "list", "--label", jsonlTestLabel).Output()
	c.NoError(err, "list -J failed")
	lines := splitJSONLines(t, jsonlOut)
	c.Len(lines, len(envelope.GetChildren()), "list -J emitted %d lines for %d children in -o json's envelope; JSONL must carry every row", len(lines), len(envelope.GetChildren()))
	for i, line := range lines {
		var ch rafikiv1.ChildSummary
		if err := protojson.Unmarshal([]byte(line), &ch); err != nil {
			t.Fatalf("list -J line %d is not a bare ChildSummary protojson object (JSONL must unwrap the envelope): %v\nline: %s", i, err, line)
		}
		c.NotEq("", ch.GetChildId(), "list -J line %d has an empty childId: %s", i, line)
	}

	// ── models: -J emits one ModelRow per line, none wrapped ─────────────
	modelsOut, err := cliCmd(t, d, "-J", "models").Output()
	c.NoError(err, "models -J failed")
	modelLines := splitJSONLines(t, modelsOut)
	c.NotEmpty(modelLines, "models -J emitted no rows; the builtin source must always serve at least one model")
	for i, line := range modelLines {
		var row struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("models -J line %d is not a ModelRow object: %v\nline: %s", i, err, line)
		}
		c.NotEq("", row.ID, "models -J line %d has an empty id: %s", i, line)
	}

	// ── get: -J with two targets emits two lines ─────────────────────────
	getOut, err := cliCmd(t, d, "-J", "get", names[0], names[1]).Output()
	c.NoError(err, "get -J with two targets failed")
	getLines := splitJSONLines(t, getOut)
	c.Len(getLines, 2, "get -J with two targets emitted %d lines, want 2\noutput: %s", len(getLines), getOut)
	for i, line := range getLines {
		var ch rafikiv1.ChildSummary
		if err := protojson.Unmarshal([]byte(line), &ch); err != nil {
			t.Fatalf("get -J line %d is not a bare ChildSummary protojson object: %v\nline: %s", i, err, line)
		}
		c.NotEq("", ch.GetChildId(), "get -J line %d has an empty childId: %s", i, line)
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
	c.Error(badCmd.Run(), "get with a failing target must exit nonzero; stdout: %s", badOut.String())
	if badOut.Len() == 0 || !strings.Contains(badOut.String(), names[0]) {
		t.Fatalf("get -J should still emit the good sibling's row on stdout; got: %q", badOut.String())
	}
	c.NotStrContains(badOut.String(), "no-such-child-xyz", "failing target leaked onto stdout")
	c.StrContains(badErrBuf.String(), "no-such-child-xyz", "failing target's diagnostic missing from stderr")

	// ── tasks with NO flags: the default flip, end to end ─────────────────
	// auto on a pipe used to mean JSON; it now means table. A consumer that
	// pipes `rafiki tasks` and unmarshals must fail — and read a table border.
	tasksOut, err := cliCmd(t, d, "tasks").Output()
	c.NoError(err, "tasks failed")
	var probe any
	c.Error(json.Unmarshal(tasksOut, &probe), "rafiki tasks with no flags must NOT parse as JSON after the default flip; output: %s", tasksOut)
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
	c.Error(bothCmd.Run(), "combining -j and -J must fail; output: %s", bothErrBuf.String())
	c.StrContains(bothErrBuf.String(), "cannot combine -j and -J", "-j -J error text changed; got")
}

// operatorName mints a unique username per run: the suite shares one
// disposable database across runs and daemons, so a fixed name would collide
// with the user a previous run created ("username ... is already taken").
func operatorName() string {
	return fmt.Sprintf("operator-%d", time.Now().UnixNano())
}

// TestCLI_PresetPutThenCreateOnTheSameProfile is the preset round trip end to
// end: `rafiki preset put` writes the preset (owned by the profile's user —
// presets resolve against the connection's owner), then
// `rafiki create --preset` spawns with it and without a --model flag (a
// preset that did not apply would fail the spawn, not silently pass), and
// `rafiki get` confirms the child actually runs the preset's model.
//
// The user is minted through the CLI itself: `rafiki user create` dials
// TOKEN-LESS (it is the recovery path for a stale token, and must never
// present one), the UDS admits it anonymously because the socket is the trust
// mechanism, and it writes the minted token into the profile's token file,
// which every later invocation in this test then authenticates with. The mint
// also keeps this run's preset owner-scoped to a user no other run shares.
func TestCLI_PresetPutThenCreateOnTheSameProfile(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	d := bootDaemon(t)

	// One config dir for the whole flow: the token `user create` writes must
	// still be there when preset put and create resolve the profile.
	configDir := t.TempDir()

	// 1. Mint the daemon's first user; the CLI writes its token into profile
	// "it"'s token file (renderUserCreate) and prints it once on stderr. The
	// dial presents no token (user create is the recovery path for a stale
	// token, and must never present one) and the UDS admits it
	// anonymously — the socket is the trust mechanism, there is no bootstrap
	// window on the framed UDS. When this daemon genuinely has no users yet,
	// the user minted here becomes its admin (UserCreateLocal's zero-users
	// rule); on the suite's shared database other users already exist, so the
	// minted user is an ordinary one — this test does not depend on which.
	userCmd := cliCmdIn(t, d, configDir, "--output", "json", "user", "create", operatorName())
	var userStderr bytes.Buffer
	userCmd.Stderr = &userStderr
	userOut, err := userCmd.Output()
	c.NoError(err, "user create failed: %v\nstdout: %s\nstderr: %s", err, userOut, userStderr.String())
	tokenPath := filepath.Join(configDir, "rafiki", "profiles", "it", "token")
	if b, err := os.ReadFile(tokenPath); err != nil || len(bytes.TrimSpace(b)) == 0 {
		t.Fatalf("user create did not leave a token at %s: %v", tokenPath, err)
	}

	// 2. Write a preset over Connect: the profile's bearer token rides
	// control socket, so the preset is owned by the user minted in step 1.
	presetFile := filepath.Join(t.TempDir(), "review-fixer.json")
	presetJSON := `{"description":"uds-auth regression fixture","kind":"fundi","model":"anthropic/claude-sonnet-4-5"}`
	c.NoError(os.WriteFile(presetFile, []byte(presetJSON), 0o600))
	putOut, err := cliCmdIn(t, d, configDir, "preset", "put", "review:fixer", "-f", presetFile).CombinedOutput()
	c.NoError(err, "preset put failed: %v\noutput: %s", err, putOut)
	c.StrContains(string(putOut), "saved review:fixer", "preset put output missing confirmation: %s", putOut)

	// 3. Create with that preset and NO --model flag on purpose: the preset
	// supplies the model, so a preset that did not apply would fail the
	// spawn, not silently pass.
	var createStderr bytes.Buffer
	createCmd := cliCmdIn(t, d, configDir,
		"--output", "json",
		"create", "preset-bug",
		"--preset", "review:fixer",
		"--cwd", "/tmp",
		"--no-session",
		"--no-extensions",
		"--no-local-executor",
		"--detached",
	)
	createCmd.Stderr = &createStderr
	createOut, err := createCmd.Output()
	c.NoError(err, "create --preset failed: %v\nstderr: %s", err, createStderr.String())
	var spawnResp struct {
		ChildID string `json:"childId"`
	}
	if err := json.Unmarshal(createOut, &spawnResp); err != nil {
		t.Fatalf("decode create response: %v\n%s", err, createOut)
	}
	c.NotEq("", spawnResp.ChildID, "create --preset returned empty childId; output: %s", createOut)
	t.Cleanup(func() {
		cmd := cliCmdIn(t, d, configDir, "kill", "preset-bug")
		_, _ = cmd.CombinedOutput()
	})

	// 4. The round trip's payoff: the child runs the preset's model — the
	// preset resolved in the daemon and the model comes back on `get`.
	getOut, err := cliCmdIn(t, d, configDir, "--output", "json", "get", spawnResp.ChildID).CombinedOutput()
	c.NoError(err, "get after preset create: %v\n%s", err, getOut)
	var child rafikiv1.ChildSummary
	c.NoError(protojson.Unmarshal(getOut, &child), "decode get output %q", getOut)
	c.Eq("anthropic/claude-sonnet-4-5", child.GetModel(), "child created with --preset has model")
}
