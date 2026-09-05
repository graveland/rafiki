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
	// `get` always emits indented JSON (it ignores --output), hence the space.
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
	// Capture stdout and stderr separately — rafiki may emit a best-effort warning
	// on stderr (e.g. active-marker directory not found) that we don't want to
	// confuse with the JSON payload on stdout.
	createCmd := cliCmd(t, d,
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
	// `get` always emits indented JSON (it ignores --output), hence the space.
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
