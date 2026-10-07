// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bytes"
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

// checkCreateBody inspects a docker container-create body and refuses any mount
// or privilege field that could reach past the operator's declared sandbox
// roots. It stays narrow — only the HostConfig fields below are read, and
// nothing the container reports gates anything — but it fails closed on
// everything the daemon cannot express: the daemon's CreateBody emits only
// mount {Type,Source,Target,ReadOnly}, so a body carrying Binds, VolumeOptions,
// a device, extra capabilities, a host namespace, or privileged mode did not
// come from our daemon and is refused.
//
// Binds are refused outright because their source is a bare host path that is
// easy to get wrong; Mounts name a Type, so a bind can be told from a volume.
// A volume or tmpfs with no VolumeOptions is passed (the daemon controls what
// those contain); a bind must resolve to the relay dir or under one of the
// declared roots.
func checkCreateBody(body []byte, roots []string, relayDir string) error {
	var req struct {
		HostConfig struct {
			Binds  []string `json:"Binds"`
			Mounts []struct {
				Type          string          `json:"Type"`
				Source        string          `json:"Source"`
				Target        string          `json:"Target"`
				VolumeOptions json.RawMessage `json:"VolumeOptions"`
			} `json:"Mounts"`
			Privileged  bool              `json:"Privileged"`
			Devices     []json.RawMessage `json:"Devices"`
			VolumesFrom []string          `json:"VolumesFrom"`
			CapAdd      []string          `json:"CapAdd"`
			CapDrop     []string          `json:"CapDrop"`
			SecurityOpt []string          `json:"SecurityOpt"`
			PidMode     string            `json:"PidMode"`
			IpcMode     string            `json:"IpcMode"`
			UTSMode     string            `json:"UTSMode"`
			UsernsMode  string            `json:"UsernsMode"`
			NetworkMode string            `json:"NetworkMode"`
		} `json:"HostConfig"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("sandbox: malformed container create body: %w", err))
	}

	if len(req.HostConfig.Binds) > 0 {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("sandbox: container create declares binds %v; use Mounts with an explicit type", req.HostConfig.Binds))
	}

	for _, m := range req.HostConfig.Mounts {
		// A volume can itself be a host bind: VolumeOptions with a local
		// driver whose options name a device bind /etc onto a "volume". The
		// daemon never emits VolumeOptions, so any non-null value is refused.
		if opts := bytes.TrimSpace(m.VolumeOptions); len(opts) > 0 && !bytes.Equal(opts, []byte("null")) {
			return connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("sandbox: mount %q carries VolumeOptions %s, which can hide a host bind", m.Target, opts))
		}
		switch m.Type {
		case "volume", "tmpfs":
			// The daemon owns the contents of these; nothing host-side is
			// exposed by naming one.
		case "bind":
			if err := checkBindSource(m.Source, roots, relayDir); err != nil {
				return err
			}
		default:
			return connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("sandbox: mount %q has unsupported type %q", m.Target, m.Type))
		}
	}

	if err := checkPrivilege(req.HostConfig.Privileged, req.HostConfig.Devices, req.HostConfig.VolumesFrom,
		req.HostConfig.CapAdd, req.HostConfig.CapDrop, req.HostConfig.SecurityOpt, req.HostConfig.PidMode,
		req.HostConfig.IpcMode, req.HostConfig.UTSMode, req.HostConfig.UsernsMode, req.HostConfig.NetworkMode); err != nil {
		return err
	}
	return nil
}

// checkPrivilege refuses the HostConfig fields that grant a container more
// privilege than a sandbox spec allows. The daemon's CreateBody cannot set any
// of them, so their mere presence means the body did not come from our daemon.
func checkPrivilege(privileged bool, devices []json.RawMessage, volumesFrom, capAdd, capDrop, securityOpt []string,
	pidMode, ipcMode, utsMode, usernsMode, networkMode string) error {
	refuse := func(field string, value any) error {
		return connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("sandbox: container create sets %s=%v, which the daemon never emits", field, value))
	}
	switch {
	case privileged:
		return refuse("Privileged", privileged)
	case len(devices) > 0:
		return refuse("Devices", len(devices))
	case len(volumesFrom) > 0:
		return refuse("VolumesFrom", volumesFrom)
	case len(capAdd) > 0:
		return refuse("CapAdd", capAdd)
	case len(capDrop) > 0:
		return refuse("CapDrop", capDrop)
	case len(securityOpt) > 0:
		return refuse("SecurityOpt", securityOpt)
	case pidMode != "":
		return refuse("PidMode", pidMode)
	case ipcMode != "":
		return refuse("IpcMode", ipcMode)
	case utsMode != "":
		return refuse("UTSMode", utsMode)
	case usernsMode != "":
		return refuse("UsernsMode", usernsMode)
	case networkMode != "" && networkMode != "bridge" && networkMode != "none":
		return refuse("NetworkMode", networkMode)
	}
	return nil
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
