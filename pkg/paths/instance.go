package paths

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
)

// DefaultInstance is the executor instance every install had before
// instances existed; it maps to the unprefixed paths, so nothing migrates.
const DefaultInstance = "default"

// maxInstanceLen keeps an instance name usable as a single directory component
// and as a suffix on a unit name; the same cap the remote install path applies
// to the name it quotes into a shell script.
const maxInstanceLen = 64

// instanceMu guards instanceName. Instance() is read from every path resolver —
// once per tool call in a long-lived executor — so it takes a lock rather than
// being plain: SetInstance runs once at startup, before any path is read, and a
// data race here would be a race on where state lands, not on a display value.
var (
	instanceMu   sync.Mutex
	instanceName = DefaultInstance
)

// SetInstance namespaces this PROCESS's DataDir and CacheDir under
// profiles/<name> for any name other than DefaultInstance. It never touches
// ConfigDir, StateDir or RuntimeDir: the daemon's sockets and the client's
// own state live there. Call once, before any path is read.
func SetInstance(name string) error {
	if err := ValidateInstance(name); err != nil {
		return err
	}
	instanceMu.Lock()
	instanceName = name
	instanceMu.Unlock()
	return nil
}

// Instance reports the current instance name (DefaultInstance when unset).
func Instance() string {
	instanceMu.Lock()
	defer instanceMu.Unlock()
	return instanceName
}

// ValidateInstance rejects anything that is not a single, unambiguous directory
// component: empty, "." and ".." would resolve to the parent (or to the base
// directory itself) once joined, and a separator or any other character outside
// the set could reach out of profiles/ or need quoting in the POSIX sh the
// remote install path generates. A profile name is only checked as a path
// component by pkg/profile, so this is the stricter rule the instance namespace
// needs.
func ValidateInstance(name string) error {
	if name == "" {
		return errors.New("an instance name cannot be empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("instance name %q is reserved: it would resolve to the profiles directory itself", name)
	}
	if len(name) > maxInstanceLen {
		return fmt.Errorf("an instance name is at most %d characters", maxInstanceLen)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("an instance name may contain only letters, digits, "+
				"'-', '_' and '.': %q is not allowed", r)
		}
	}
	return nil
}

// instanceScoped appends profiles/<name> to an otherwise instance-agnostic base
// directory. The default instance returns dir unchanged, which is what keeps
// every existing install exactly where it is.
//
// Under profiles/ rather than as a sibling prefix so an instance name can never
// collide with a directory the base already has (logs, spill, state).
func instanceScoped(dir string) string {
	if name := Instance(); name != DefaultInstance {
		return filepath.Join(dir, "profiles", name)
	}
	return dir
}

// instanceFileName names a per-instance file that lives in a directory that is
// NOT namespaced (ConfigDir, StateDir): "<prefix><suffix>" for the default
// instance, "<prefix>-<name><suffix>" otherwise. Keeping the default spelling
// byte-identical is what makes this a rename-free change.
func instanceFileName(prefix, suffix string) string {
	name := Instance()
	if name == DefaultInstance {
		return prefix + suffix
	}
	return prefix + "-" + name + suffix
}
