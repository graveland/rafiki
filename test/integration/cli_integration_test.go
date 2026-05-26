package integration_test

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestCLI_Status verifies that `pic status` returns a JSON object containing
// a "version" field.
func TestCLI_Status(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	cmd := exec.Command(piCtlPath, "--socket", d.socketPath, "--output", "json", "status")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), `"version"`) {
		t.Fatalf("status output missing version field: %s", out)
	}
}

// TestCLI_CreateListKillForget exercises the core child lifecycle via the CLI:
// create --detached → list (child present) → kill → poll for exited → forget.
func TestCLI_CreateListKillForget(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	// create --detached
	var createStderr bytes.Buffer
	createCmd := exec.Command(piCtlPath,
		"--socket", d.socketPath,
		"--output", "json",
		"create", "smoke",
		"--cwd", "/tmp",
		"--no-session",
		"--model", "fake/dummy",
		"--no-extensions",
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
	listCmd := exec.Command(piCtlPath, "--socket", d.socketPath, "--output", "json", "list")
	out, err = listCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), childID) {
		t.Fatalf("list missing childId %s: %s", childID, out)
	}

	// kill
	killCmd := exec.Command(piCtlPath, "--socket", d.socketPath, "kill", "smoke")
	out, err = killCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kill failed: %v\n%s", err, out)
	}

	// poll until status=exited (up to 5 seconds)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		getCmd := exec.Command(piCtlPath, "--socket", d.socketPath, "--output", "json", "get", "smoke")
		out, _ = getCmd.CombinedOutput()
		if strings.Contains(string(out), `"status":"exited"`) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// forget
	forgetCmd := exec.Command(piCtlPath, "--socket", d.socketPath, "forget", "smoke")
	out, err = forgetCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("forget failed: %v\n%s", err, out)
	}
}

// MANUAL SMOKE PROCEDURE (not run in CI):
//
//	# Boot daemon
//	./bin/pi-controller &
//	sleep 1
//
//	# Spawn a child interactively (attaches a TUI)
//	./bin/pic create demo --cwd /tmp --no-extensions \
//	    --model anthropic/claude-haiku-4-5
//
//	# In the TUI:
//	- Verify the pi welcome screen renders
//	- Type "Say hello" and press Enter
//	- Verify the assistant streams a response
//	- Press Ctrl+D to detach
//
//	# In another shell, verify the child is still alive:
//	./bin/pic list
//	# status should be "idle" (the agent finished its turn)
//
//	# Reattach
//	./bin/pic attach demo
//	# Verify the TUI re-renders the conversation history
//
//	# Quit with native semantics
//	./bin/pic attach demo --kill-on-exit
//	# Press Ctrl+D
//	./bin/pic list
//	# demo should now be "exited"
//
//	./bin/pic forget demo
//	pkill pi-controller

// TestCLI_CreateDetached verifies that `pic create --detached` spawns a child
// and returns JSON containing a childId, then confirms the child appears in
// `pic list`. Cleans up via kill + forget.
func TestCLI_CreateDetached(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	// create --detached: should spawn the child and print JSON without attaching.
	// Capture stdout and stderr separately — pic may emit a best-effort warning
	// on stderr (e.g. active-marker directory not found) that we don't want to
	// confuse with the JSON payload on stdout.
	createCmd := exec.Command(piCtlPath,
		"--socket", d.socketPath,
		"create", "test-detached",
		"--cwd", "/tmp",
		"--no-session",
		"--no-extensions",
		"--model", "fake/dummy",
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
	listCmd := exec.Command(piCtlPath, "--socket", d.socketPath, "--output", "json", "list")
	out, err = listCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), childID) {
		t.Fatalf("list missing childId %s: %s", childID, out)
	}

	// kill
	killCmd := exec.Command(piCtlPath, "--socket", d.socketPath, "kill", "test-detached")
	out, err = killCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kill failed: %v\n%s", err, out)
	}

	// poll until status=exited (up to 5 seconds).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		getCmd := exec.Command(piCtlPath, "--socket", d.socketPath, "--output", "json", "get", "test-detached")
		out, _ = getCmd.CombinedOutput()
		if strings.Contains(string(out), `"status":"exited"`) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// forget
	forgetCmd := exec.Command(piCtlPath, "--socket", d.socketPath, "forget", "test-detached")
	out, err = forgetCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("forget failed: %v\n%s", err, out)
	}
}

// TestCLI_AttachHelp verifies that `pic attach --help` exits cleanly and
// documents the --kill-on-exit flag.
func TestCLI_AttachHelp(t *testing.T) {
	t.Parallel()

	cmd := exec.Command(piCtlPath, "attach", "--help")
	out, err := cmd.CombinedOutput()
	// cobra exits 0 for --help.
	if err != nil {
		t.Fatalf("attach --help failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "--kill-on-exit") {
		t.Fatalf("attach --help missing --kill-on-exit flag; output: %s", out)
	}
}

// TestCLI_CreateHelp verifies that `pic create --help` exits cleanly and
// documents both --detached and --kill-on-exit flags.
func TestCLI_CreateHelp(t *testing.T) {
	t.Parallel()

	cmd := exec.Command(piCtlPath, "create", "--help")
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
	createCmd := exec.Command(piCtlPath,
		"--socket", d.socketPath,
		"--output", "json",
		"create", "afk-impl",
		"--cwd", "/tmp",
		"--no-session",
		"--no-extensions",
		"--model", "fake/dummy",
		"--detached",
	)
	createCmd.Stderr = &createStderr
	out, err := createCmd.Output() // stdout only
	if err != nil {
		t.Fatalf("create --detached failed: %v\nstderr: %s", err, createStderr.String())
	}

	// resolve by prefix "afk"
	getCmd := exec.Command(piCtlPath, "--socket", d.socketPath, "--output", "json", "get", "afk")
	out, err = getCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("get with prefix failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "afk-impl") {
		t.Fatalf("expected afk-impl in get output: %s", out)
	}

	// cleanup: kill before test exits to avoid leftover processes
	killCmd := exec.Command(piCtlPath, "--socket", d.socketPath, "kill", "afk-impl")
	_, _ = killCmd.CombinedOutput()
}
