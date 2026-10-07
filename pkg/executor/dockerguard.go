// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
)

// maxContainerCreateBody bounds how much of a docker container-create body the
// guard will buffer. A create request that is legitimate is tiny; anything
// above this is refused rather than held in memory, so a compromised daemon
// cannot make the executor allocate without limit through the proxy.
const maxContainerCreateBody = 1 << 20

// dockerVersionRe matches docker's API version path segment: any run of digits
// and dots after a leading "v" (v1, v1.43, v1.43.0, v1.43.). This mirrors the
// engine's own version middleware (`v[0-9.]+`), which strips exactly one such
// leading segment before routing — matching it is what makes the guard
// recognise every path the engine routes to a container create.
var dockerVersionRe = regexp.MustCompile(`^v[0-9.]+$`)

// guardContainersCreate reports whether a proxied request must be inspected
// before it is forwarded: it is a docker container-create (optionally behind a
// /v<version> segment). Every other docker request, and every other proxy,
// streams through untouched.
func guardContainersCreate(start *executorpb.ProxyStart) bool {
	return start.GetProxyName() == "docker" &&
		start.GetMethod() == "POST" &&
		isContainersCreate(start.GetPath())
}

// isContainersCreate reports whether the request path routes to docker's
// container create. It strips at most one leading version segment exactly as
// the engine's version middleware does, compares the route case-insensitively
// (a fail-closed lowercase costs nothing), and then fails closed: any path that
// still ends at the create route behind a prefix it did not strip is guarded
// rather than allowed to stream unchecked.
func isContainersCreate(p string) bool {
	// The engine routes on the DECODED path and a Go client drops the fragment,
	// so a path carrying a percent-escape or a fragment is one the guard cannot
	// reason about — treat it as a create. Over-inclusive is safe: a body with
	// nothing privileged passes through untouched, while under-inclusive would
	// stream a Binds/Mounts body unchecked.
	if strings.ContainsAny(p, "%#") {
		return true
	}
	raw, _, _ := strings.Cut(p, "?")
	clean := strings.ToLower(path.Clean(raw))

	rest := clean
	if seg, tail, ok := strings.Cut(strings.TrimPrefix(clean, "/"), "/"); ok {
		if dockerVersionRe.MatchString(seg) {
			rest = "/" + tail
		}
	}
	if rest == "/containers/create" {
		return true
	}
	return strings.HasSuffix(clean, "/containers/create")
}

// Allowlists of exactly the keys the daemon's CreateBody emits
// (pkg/sandbox/createbody.go). Anything else is refused: the daemon's body is
// a fixed struct, so a key outside these sets did not come from our daemon. A
// denylist would only enforce what its author remembered.
var (
	createBodyTopKeys = map[string]bool{
		"Image": true, "Entrypoint": true, "Cmd": true, "WorkingDir": true,
		"Env": true, "Labels": true, "User": true, "HostConfig": true,
	}
	hostConfigKeys = map[string]bool{
		"Mounts": true, "NetworkMode": true, "ReadonlyRootfs": true,
		"Memory": true, "NanoCpus": true, "PidsLimit": true, "RestartPolicy": true,
	}
	mountKeys         = map[string]bool{"Type": true, "Source": true, "Target": true, "ReadOnly": true}
	restartPolicyKeys = map[string]bool{"Name": true}
)

// refuseUnknownKeys returns a PermissionDenied error naming the first key in m
// that is not on allowed.
func refuseUnknownKeys(where string, m map[string]json.RawMessage, allowed map[string]bool) error {
	for k := range m {
		if !allowed[k] {
			return connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("sandbox: %s carries key %q, which the daemon never emits", where, k))
		}
	}
	return nil
}

// checkCreateBody inspects a docker container-create body against the exact
// shape the daemon emits and refuses anything outside it. It fails CLOSED: the
// top level, HostConfig, each mount and RestartPolicy are decoded into maps and
// every key not on the daemon's allowlist is refused, so a privilege field this
// code has never heard of (DeviceCgroupRules, Sysctls, Binds, VolumeOptions, a
// host namespace, …) is refused by construction rather than by enumeration.
//
// A bind mount's source must resolve (through symlinks) to the relay dir or
// under one of the operator's declared sandbox roots; a volume or tmpfs mount
// passes; NetworkMode is limited to "", bridge or none.
func checkCreateBody(body []byte, roots []string, relayDir string) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("sandbox: malformed container create body: %w", err))
	}
	if err := refuseUnknownKeys("create body", top, createBodyTopKeys); err != nil {
		return err
	}

	hcRaw, ok := top["HostConfig"]
	if !ok {
		// No HostConfig: nothing host-side is requested, so nothing to guard.
		return nil
	}
	var hc map[string]json.RawMessage
	if err := json.Unmarshal(hcRaw, &hc); err != nil {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("sandbox: malformed HostConfig: %w", err))
	}
	if err := refuseUnknownKeys("HostConfig", hc, hostConfigKeys); err != nil {
		return err
	}

	if raw, ok := hc["NetworkMode"]; ok {
		var mode string
		if err := json.Unmarshal(raw, &mode); err != nil {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("sandbox: malformed NetworkMode: %w", err))
		}
		if mode != "" && mode != "bridge" && mode != "none" {
			return connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("sandbox: container create sets NetworkMode=%q, which the daemon never emits", mode))
		}
	}

	if raw, ok := hc["RestartPolicy"]; ok {
		var rp map[string]json.RawMessage
		if err := json.Unmarshal(raw, &rp); err != nil {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("sandbox: malformed RestartPolicy: %w", err))
		}
		if err := refuseUnknownKeys("RestartPolicy", rp, restartPolicyKeys); err != nil {
			return err
		}
	}

	rawMounts, ok := hc["Mounts"]
	if !ok {
		return nil
	}
	var mounts []map[string]json.RawMessage
	if err := json.Unmarshal(rawMounts, &mounts); err != nil {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("sandbox: malformed Mounts: %w", err))
	}
	for _, m := range mounts {
		if err := refuseUnknownKeys("mount", m, mountKeys); err != nil {
			return err
		}
		typ, err := mountString(m, "Type")
		if err != nil {
			return err
		}
		src, err := mountString(m, "Source")
		if err != nil {
			return err
		}
		target, err := mountString(m, "Target")
		if err != nil {
			return err
		}
		switch typ {
		case "volume", "tmpfs":
			// The daemon owns the contents of these; nothing host-side is
			// exposed by naming one.
		case "bind":
			if err := checkBindSource(src, roots, relayDir); err != nil {
				return err
			}
		default:
			return connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("sandbox: mount %q has unsupported type %q", target, typ))
		}
	}
	return nil
}

// mountString extracts a string field from a decoded mount, refusing a
// wrong-typed value rather than silently treating it as empty.
func mountString(m map[string]json.RawMessage, key string) (string, error) {
	raw, ok := m[key]
	if !ok {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("sandbox: malformed mount %s: %w", key, err))
	}
	return s, nil
}

// checkBindSource resolves src through every symlink and refuses it unless it
// equals or sits under a resolved sandbox root, or equals the resolved relay
// dir. Resolution happens on both sides so a symlink placed inside a root that
// points outside it is caught, and a source that does not exist is refused
// rather than waved through.
func checkBindSource(src string, roots []string, relayDir string) error {
	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("sandbox: bind source %q cannot be resolved: %v", src, err))
	}

	// Roots first: a source under a declared root passes without the relay dir
	// being consulted, so a relay dir that is configured but not yet present
	// cannot block a legitimate root bind.
	for _, root := range roots {
		resolvedRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("sandbox: mount root %q cannot be resolved: %v", root, err))
		}
		if resolvedRoot == string(filepath.Separator) ||
			resolved == resolvedRoot ||
			strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
			return nil
		}
	}

	if relayDir != "" {
		resolvedRelay, err := filepath.EvalSymlinks(relayDir)
		if err != nil {
			return connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("sandbox: relay dir %q cannot be resolved: %v", relayDir, err))
		}
		if resolved == resolvedRelay {
			return nil
		}
	}

	return connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("sandbox: bind source %q is outside sandbox mount roots %v and the relay dir", src, roots))
}
