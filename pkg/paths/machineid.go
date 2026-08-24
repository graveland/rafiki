package paths

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MachineIDFile is where this machine's stable identity lives.
func MachineIDFile() string { return filepath.Join(DataDir(), "machine-id") }

// MachineID returns a stable identifier for this machine, creating it on first
// call.
//
// It replaces os.Hostname() as the session executor's identity key. A hostname
// is not an identity: on darwin it changes with the active network interface,
// so one laptop minted a separate executor row per network and switching
// networks mid-session orphaned the running child's workspace.
//
// This is a real-machine and VM concept only. A container or a k8s pod has no
// meaningful "machine" to share with an interactive client, and correctly
// matches nothing — the id exists to answer "is there a durable executor on
// THIS box?", which a deployed executor never satisfies.
//
// Random rather than derived from hardware: IOPlatformUUID and
// /etc/machine-id need per-platform code and are meaningless inside a
// container, and nothing here needs the id to survive wiping the data
// directory.
func MachineID() (string, error) {
	path := MachineIDFile()
	if raw, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id, nil
		}
		// A blank or whitespace-only file is corruption, not an id. Fall
		// through and replace it.
	}

	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generate machine id: %w", err)
	}
	id := hex.EncodeToString(buf[:])

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create data directory for the machine id: %w", err)
	}
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write machine id: %w", err)
	}
	return id, nil
}
