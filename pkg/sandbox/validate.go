// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"fmt"
	"math"
	"path"
	"regexp"
	"strings"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// Caller is the provenance of a create request: a child credential, or not.
type Caller struct{ Child bool }

// Resolved is a validated SandboxSpec with the defaults filled in (Image,
// Network, TTL, and a child's clamped limits).
type Resolved struct{ protocol.SandboxSpec }

var (
	volumeNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
	userRe       = regexp.MustCompile(`^[A-Za-z0-9_.-]+(:[A-Za-z0-9_.-]+)?$`)
)

// Validate checks spec for creation. named=true is CreateSandbox, false is a
// spawn block. roots are the launcher's reported --sandbox-mount-root values.
//
// It is pure: the input is never mutated, and a failure returns the zero
// Resolved. The returned Resolved owns genuine copies of the input's Mounts,
// Env and Labels, so a caller may add daemon-owned labels to it without
// touching the SandboxSpec it passed in.
func Validate(spec protocol.SandboxSpec, cfg Config, roots []string, c Caller, named bool) (Resolved, error) {
	// 1. Name / scope / (non-named) TTL.
	if named {
		if spec.Name == "" {
			return Resolved{}, fmt.Errorf("sandbox: name is required for a named sandbox")
		}
		if err := paths.ValidateMachineName(spec.Name); err != nil {
			return Resolved{}, fmt.Errorf("sandbox: name: %w", err)
		}
		if spec.Scope != "" {
			return Resolved{}, fmt.Errorf("sandbox: scope is only valid on a spawn block")
		}
	} else {
		if spec.Name != "" {
			return Resolved{}, fmt.Errorf("sandbox: name is only valid for a named sandbox")
		}
		switch spec.Scope {
		case protocol.ScopeSelf, protocol.ScopeSubtree:
		case "":
			return Resolved{}, fmt.Errorf("sandbox: scope is required on a spawn block (self or subtree)")
		default:
			return Resolved{}, fmt.Errorf("sandbox: scope must be %q or %q, got %q",
				protocol.ScopeSelf, protocol.ScopeSubtree, spec.Scope)
		}
		if spec.TTL != 0 {
			return Resolved{}, fmt.Errorf("sandbox: ttl is only valid for a named sandbox")
		}
	}

	out := Resolved{SandboxSpec: spec}
	// A genuine copy: the caller may add daemon-owned labels to the Resolved
	// without mutating the spec it passed in. nil stays nil and a non-nil
	// empty collection stays non-nil empty (the tri-state the preset/label
	// vocabulary relies on), so only allocate when the source is non-nil.
	out.Mounts = cloneMounts(spec.Mounts)
	out.Env = cloneStringMap(spec.Env)
	out.Labels = cloneStringMap(spec.Labels)

	// 2. Image.
	switch {
	case spec.Image != "":
		out.Image = spec.Image
	case cfg.Image != "":
		out.Image = cfg.Image
	default:
		return Resolved{}, fmt.Errorf("sandbox: image is required: set image in the spec or RAFIKI_SANDBOX_IMAGE on the daemon")
	}

	// 3. Named TTL.
	if named {
		switch {
		case spec.TTL == 0:
			out.TTL = cfg.TTL
		case spec.TTL < 0:
			return Resolved{}, fmt.Errorf("sandbox: ttl must not be negative")
		case spec.TTL > cfg.MaxTTL:
			return Resolved{}, fmt.Errorf("sandbox: ttl %s exceeds %s (%s)", spec.TTL, EnvMaxTTL, cfg.MaxTTL)
		default:
			out.TTL = spec.TTL
		}
	} else {
		out.TTL = 0
	}

	// 4. Mounts.
	seenTargets := map[string]bool{}
	for i, m := range spec.Mounts {
		if err := validateMount(m, i, roots, seenTargets); err != nil {
			return Resolved{}, err
		}
	}

	// 5. Workdir.
	if spec.Workdir != "" && !isCleanAbs(spec.Workdir) {
		return Resolved{}, fmt.Errorf("sandbox: workdir must be an absolute clean path, got %q", spec.Workdir)
	}

	// 6. Network.
	switch spec.Network {
	case "":
		out.Network = cfg.Network
	case protocol.NetworkEgress, protocol.NetworkNone:
		out.Network = spec.Network
	default:
		return Resolved{}, fmt.Errorf("sandbox: network must be %q or %q, got %q",
			protocol.NetworkEgress, protocol.NetworkNone, spec.Network)
	}

	// 7. Env.
	for k := range spec.Env {
		if k == "" {
			return Resolved{}, fmt.Errorf("sandbox: env has an empty key")
		}
		if strings.HasPrefix(k, "RAFIKI_") {
			return Resolved{}, fmt.Errorf("sandbox: env key %q is reserved (RAFIKI_ prefix)", k)
		}
	}

	// 8. User.
	if spec.User != "" && !userRe.MatchString(spec.User) {
		return Resolved{}, fmt.Errorf("sandbox: user %q is not a valid uid[:gid]", spec.User)
	}

	// 9. Limits: negative refused; a child is clamped to the configured caps.
	if err := validateLimits(&out, cfg, c); err != nil {
		return Resolved{}, err
	}

	// 10. Labels.
	for k := range spec.Labels {
		if k == "" {
			return Resolved{}, fmt.Errorf("sandbox: labels has an empty key")
		}
		if isReservedLabelKey(k) {
			return Resolved{}, fmt.Errorf("sandbox: labels key %q is reserved", k)
		}
	}

	// 11. Mounts may be empty — nothing to check.
	return out, nil
}

func validateMount(m protocol.SandboxMount, i int, roots []string, seenTargets map[string]bool) error {
	if m.Target == "" || !path.IsAbs(m.Target) {
		return fmt.Errorf("sandbox: mounts[%d].target must be an absolute path", i)
	}
	if path.Clean(m.Target) != m.Target {
		return fmt.Errorf("sandbox: mounts[%d].target must be a clean path, got %q", i, m.Target)
	}
	if seenTargets[m.Target] {
		return fmt.Errorf("sandbox: mounts[%d].target %q is duplicated", i, m.Target)
	}
	seenTargets[m.Target] = true

	switch m.Kind {
	case protocol.MountRO, protocol.MountRW, protocol.MountEphemeral:
	case "":
		return fmt.Errorf("sandbox: mounts[%d].kind is required (ro, rw or ephemeral)", i)
	default:
		return fmt.Errorf("sandbox: mounts[%d].kind must be ro, rw or ephemeral, got %q", i, m.Kind)
	}

	switch m.Kind {
	case protocol.MountRO, protocol.MountRW:
		if m.HostPath != "" && m.Volume != "" {
			return fmt.Errorf("sandbox: mounts[%d] must set exactly one of host_path or volume, not both", i)
		}
		if m.Kind == protocol.MountRO && m.HostPath == "" && m.Volume == "" {
			return fmt.Errorf("sandbox: mounts[%d] (ro) requires a host_path or volume source", i)
		}
	case protocol.MountEphemeral:
		if m.HostPath != "" || m.Volume != "" {
			return fmt.Errorf("sandbox: mounts[%d] (ephemeral) must not set a source", i)
		}
	}

	if m.HostPath != "" {
		if !isCleanAbs(m.HostPath) {
			return fmt.Errorf("sandbox: mounts[%d].host_path must be an absolute clean path, got %q", i, m.HostPath)
		}
		if len(roots) == 0 {
			return fmt.Errorf("sandbox: mounts[%d].host_path: the launcher declares no --sandbox-mount-root, so no host path may be mounted", i)
		}
		if !underAnyRoot(m.HostPath, roots) {
			return fmt.Errorf("sandbox: mounts[%d].host_path %q is not under any launcher mount root (--sandbox-mount-root)", i, m.HostPath)
		}
	}
	if m.Volume != "" && !volumeNameRe.MatchString(m.Volume) {
		return fmt.Errorf("sandbox: mounts[%d].volume %q is not a valid volume name", i, m.Volume)
	}
	return nil
}

func validateLimits(out *Resolved, cfg Config, c Caller) error {
	if out.MemoryBytes < 0 {
		return fmt.Errorf("sandbox: memory_bytes must not be negative")
	}
	// NaN and Inf are neither < 0 nor > cap nor == 0, so without this a child
	// could slip the clamp un-clamped and hand Docker an undefined NanoCPUs.
	// cpus arrives as a proto double, so NaN is reachable from a caller.
	if math.IsNaN(out.CPUs) || math.IsInf(out.CPUs, 0) {
		return fmt.Errorf("sandbox: cpus must be a finite number")
	}
	if out.CPUs < 0 {
		return fmt.Errorf("sandbox: cpus must not be negative")
	}
	if out.PidsLimit < 0 {
		return fmt.Errorf("sandbox: pids_limit must not be negative")
	}
	if !c.Child {
		return nil
	}
	if cap := cfg.ChildMaxMemoryBytes; cap > 0 {
		switch {
		case out.MemoryBytes == 0:
			out.MemoryBytes = cap
		case out.MemoryBytes > cap:
			return fmt.Errorf("sandbox: memory_bytes %d exceeds %s (%d)", out.MemoryBytes, EnvChildMaxMemory, cap)
		}
	}
	if cap := cfg.ChildMaxCPUs; cap > 0 {
		switch {
		case out.CPUs == 0:
			out.CPUs = cap
		case out.CPUs > cap:
			return fmt.Errorf("sandbox: cpus %g exceeds %s (%g)", out.CPUs, EnvChildMaxCPUs, cap)
		}
	}
	if cap := cfg.ChildMaxPids; cap > 0 {
		switch {
		case out.PidsLimit == 0:
			out.PidsLimit = cap
		case out.PidsLimit > cap:
			return fmt.Errorf("sandbox: pids_limit %d exceeds %s (%d)", out.PidsLimit, EnvChildMaxPids, cap)
		}
	}
	return nil
}

// cloneMounts returns a copy of in, preserving the nil/empty distinction: a
// nil slice stays nil, a non-nil empty slice stays non-nil empty.
func cloneMounts(in []protocol.SandboxMount) []protocol.SandboxMount {
	if in == nil {
		return nil
	}
	out := make([]protocol.SandboxMount, len(in))
	copy(out, in)
	return out
}

// cloneStringMap returns a copy of in, preserving the nil/empty distinction: a
// nil map stays nil, a non-nil empty map stays non-nil empty.
func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// isReservedLabelKey reports whether k is reserved for the daemon.
func isReservedLabelKey(k string) bool {
	if strings.HasPrefix(k, "rafiki/") || strings.HasPrefix(k, "rafiki.") || strings.HasPrefix(k, "fundi/") {
		return true
	}
	return k == "owner" || k == "machine"
}

// isCleanAbs reports whether p is absolute and path.Clean(p) == p, which
// rejects "..", trailing slashes (except "/") and doubled separators.
func isCleanAbs(p string) bool { return path.IsAbs(p) && path.Clean(p) == p }

// underAnyRoot reports whether p is at or below one of roots, comparing
// lexically on a separator boundary so "/srv/repos" covers "/srv/repos" and
// "/srv/repos/a" but not "/srv/reposX".
func underAnyRoot(p string, roots []string) bool {
	for _, r := range roots {
		r = path.Clean(r)
		if !path.IsAbs(r) {
			continue
		}
		if r == "/" || p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}
