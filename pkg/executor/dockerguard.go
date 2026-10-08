// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/sandbox"
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
	//
	// The test is applied to the PATH only. A query is never part of the route,
	// and `url.Values.Encode` percent-escapes the image ref in a pull
	// (`POST /images/create?fromImage=reg%2Fimg`, or a digest ref), so testing
	// the whole target would take every registry-qualified pull for a create and
	// refuse it on its empty body — breaking the daemon's own image pull.
	raw, _, _ := strings.Cut(p, "?")
	if strings.ContainsAny(raw, "%#") {
		return true
	}
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
		"Image": true, "Entrypoint": true, "Cmd": true, "Hostname": true,
		"WorkingDir": true, "Env": true, "Labels": true, "User": true,
		"HostConfig": true,
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
// passes, except the foothold relay volume (relayVolume), which is admitted
// only read-only at the fixed relay target; NetworkMode is limited to "",
// bridge or none.
func checkCreateBody(body []byte, roots []string, relayDir, relayVolume string) error {
	// Refuse a duplicated key at any object level FIRST. A map decode keeps only
	// the LAST value for a repeated key, while the Docker engine MERGES repeated
	// object keys into one struct: `{"HostConfig":{"Binds":[...]},"HostConfig":
	// {"NetworkMode":"none"}}` would leave the guard inspecting only the second
	// object while the engine acts on the merged one. The two views must be the
	// same document, so a duplicate is refused rather than reconciled.
	if err := checkNoDuplicateKeys(body); err != nil {
		return err
	}

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
			// exposed by naming one. The one exception is the foothold relay
			// volume: it carries the relay socket (daemon.sock), so a mount
			// that could replace it read-write would let one sandbox capture
			// another sandbox's executor credentials. It is therefore admitted
			// only when ReadOnly is the JSON boolean true and the target is the
			// fixed relay dir, so it can never be written to. A missing
			// ReadOnly key counts as false; a value that is not a JSON boolean
			// is a refusal, not a silent false.
			if relayVolume != "" && typ == "volume" && src == relayVolume {
				ro, err := mountBool(m, "ReadOnly")
				if err != nil || !ro || target != sandbox.ContainerRelayDir {
					return connect.NewError(connect.CodePermissionDenied,
						fmt.Errorf("sandbox: the relay volume %q may only be mounted read-only at /run/rafiki-relay", relayVolume))
				}
			}
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

// duplicateKeyError reports a key repeated within one object. It is distinct so
// checkNoDuplicateKeys can tell a duplicate (a refusal the guard makes on its
// own terms) from a syntax error the decoder happened to hit first.
type duplicateKeyError struct{ key string }

func (e duplicateKeyError) Error() string {
	return fmt.Sprintf("object repeats key %q", e.key)
}

// checkNoDuplicateKeys walks the whole JSON document and refuses any object
// level at which a key appears more than once. It uses json.Decoder.Token, not
// a map decode — a map CANNOT see a duplicate, so this is the only way to make
// the guard and the engine agree on what the document says.
func checkNoDuplicateKeys(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := scanDuplicateKeys(dec); err != nil {
		var dup duplicateKeyError
		if errors.As(err, &dup) {
			return connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("sandbox: container create body repeats key %q", dup.key))
		}
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("sandbox: malformed container create body: %w", err))
	}
	return nil
}

// scanDuplicateKeys reads exactly one JSON value, recursing through every array
// and object, and refuses a repeated key in any object. A syntax error surfaces
// as itself, so the caller can tell it from a duplicate.
func scanDuplicateKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // a scalar value has no keys under it
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("object key is not a string")
			}
			if seen[key] {
				return duplicateKeyError{key: key}
			}
			seen[key] = true
			if err := scanDuplicateKeys(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // consume the closing '}'
			return err
		}
		return nil
	case '[':
		for dec.More() {
			if err := scanDuplicateKeys(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // consume the closing ']'
			return err
		}
		return nil
	default:
		return fmt.Errorf("unexpected delimiter %q", delim)
	}
}

// bindSourceTypeName names a file mode for the refusal above: the four shapes
// the type check distinguishes, in the words an operator would use.
func bindSourceTypeName(m os.FileMode) string {
	switch {
	case m.IsDir():
		return "directory"
	case m.IsRegular():
		return "regular file"
	case m&os.ModeSocket != 0:
		return "socket"
	case m&os.ModeDevice != 0:
		return "device"
	case m&os.ModeNamedPipe != 0:
		return "named pipe"
	default:
		return "irregular file"
	}
}

// mountBool extracts a boolean field from a decoded mount. A missing key is
// false; a value that is not a JSON boolean is an error, which callers treat
// as a refusal rather than a silent false.
func mountBool(m map[string]json.RawMessage, key string) (bool, error) {
	raw, ok := m[key]
	if !ok {
		return false, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("sandbox: malformed mount %s: %w", key, err))
	}
	return b, nil
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

	// A bind of a unix SOCKET still permits connect(): a root containing
	// docker.sock, the daemon's controller.sock, an executor socket or a
	// credential file would be a full escape for every child of the launcher's
	// owner. The path checks below cannot see that — only the file type can — so
	// after resolving, refuse anything that is not a regular file or a
	// directory (socket, device, named pipe, irregular). This is the LAUNCHER's
	// check, on the launcher's host: the daemon's own pre-check (pkg/sandbox's
	// Validate) stays LEXICAL, because it cannot stat a path on another machine.
	info, err := os.Stat(resolved)
	if err != nil {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("sandbox: bind source %q cannot be inspected: %v", src, err))
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("sandbox: bind source %q is a %s, not a regular file or directory: a socket, device or fifo under a mount root is a capability grant, not data", src, bindSourceTypeName(info.Mode())))
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
