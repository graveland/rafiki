package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoMachineName means this box has no name and none was supplied.
//
// An ANSWER, never a default. The deleted machine id existed to escape
// os.Hostname(), which on darwin changes with the active network interface —
// so a hostname fallback here would reintroduce exactly the bug that motivated
// the id, and a generated one would reintroduce the wrong-machine failure that
// motivated deleting it.
var ErrNoMachineName = errors.New("this machine has no executor name")

// MachineNameFile is where an interactive box keeps its name.
func MachineNameFile() string { return filepath.Join(DataDir(), "executor-name") }

// MachineName resolves this machine's executor name.
//
// Env first, then the file. A container or pod cannot rely on a writable data
// directory and takes its name from the downward API; a laptop should not need
// the variable in a shell profile.
//
// source is "env" or the file path, so callers can tell the operator where the
// value they are looking at came from.
func MachineName() (name, source string, err error) {
	if v := strings.TrimSpace(Get(ExecutorName)); v != "" {
		if err := ValidateMachineName(v); err != nil {
			return "", "env", fmt.Errorf("%s=%q: %w", ExecutorName, v, err)
		}
		return v, "env", nil
	}
	p := MachineNameFile()
	if raw, rerr := os.ReadFile(p); rerr == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			if err := ValidateMachineName(v); err != nil {
				return "", p, fmt.Errorf("%s contains %q: %w", p, v, err)
			}
			return v, p, nil
		}
		// A blank or whitespace-only file is corruption, not a name.
	}
	return "", "", fmt.Errorf(
		"%w: set it with `rafiki executor name <name>`, or export %s "+
			"(a container or pod should take it from the downward API)",
		ErrNoMachineName, ExecutorName)
}

// SetMachineName writes the name for this box.
//
// Temp file plus rename: the id this replaces was written with a bare
// os.WriteFile, so two concurrent first runs each generated a different value
// and a concurrent reader could observe a truncated one.
func SetMachineName(name string) error {
	if err := ValidateMachineName(name); err != nil {
		return err
	}
	p := MachineNameFile()
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create data directory for the executor name: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".executor-name-*")
	if err != nil {
		return fmt.Errorf("write executor name: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("write executor name: %w", err)
	}
	if _, err := tmp.WriteString(name + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("write executor name: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write executor name: %w", err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("install executor name: %w", err)
	}
	return nil
}

// maxMachineNameLen keeps a name readable in `executor list` and in a selector.
const maxMachineNameLen = 63

// ValidateMachineName rejects anything a label selector cannot carry.
//
// A name goes into `machine=<name>` in a comma-separated selector, so a comma
// or an equals sign would silently reparse into a different selector — which
// for a value that gates which machine a child lands on is a confinement bug,
// not a formatting one.
func ValidateMachineName(name string) error {
	if name == "" {
		return errors.New("an executor name cannot be empty")
	}
	if len(name) > maxMachineNameLen {
		return fmt.Errorf("an executor name is at most %d characters", maxMachineNameLen)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("an executor name may contain only letters, digits, "+
				"'-', '_' and '.': %q is not allowed", r)
		}
	}
	return nil
}
