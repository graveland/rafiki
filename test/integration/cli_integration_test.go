package integration_test

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestCLI_Status verifies that `pi-ctl status` returns a JSON object containing
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

// TestCLI_SpawnListKillForget exercises the core child lifecycle via the CLI:
// spawn → list (child present) → kill → poll for exited → forget.
func TestCLI_SpawnListKillForget(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	// spawn
	spawnCmd := exec.Command(piCtlPath,
		"--socket", d.socketPath,
		"--output", "json",
		"spawn", "smoke",
		"--cwd", "/tmp",
		"--no-session",
		"--model", "fake/dummy",
		"--no-extensions",
	)
	out, err := spawnCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("spawn failed: %v\n%s", err, out)
	}

	var spawnResp struct {
		ChildID string `json:"childId"`
	}
	if err := json.Unmarshal(out, &spawnResp); err != nil {
		t.Fatalf("decode spawn response: %v\n%s", err, out)
	}
	childID := spawnResp.ChildID
	if childID == "" {
		t.Fatalf("spawn returned empty childId; output: %s", out)
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

// TestCLI_ResolveByPrefix verifies that a child can be addressed by a prefix
// of its name (e.g. "afk" resolves "afk-impl").
func TestCLI_ResolveByPrefix(t *testing.T) {
	t.Parallel()
	d := bootDaemon(t)

	spawnCmd := exec.Command(piCtlPath,
		"--socket", d.socketPath,
		"--output", "json",
		"spawn", "afk-impl",
		"--cwd", "/tmp",
		"--no-session",
		"--no-extensions",
		"--model", "fake/dummy",
	)
	out, err := spawnCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("spawn failed: %v\n%s", err, out)
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
