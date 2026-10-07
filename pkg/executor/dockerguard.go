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

// dockerVersionRe matches docker's API version path segment, e.g. "v1.43".
var dockerVersionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+$`)

// guardContainersCreate reports whether a proxied request must be inspected
// before it is forwarded: it is a docker container-create (optionally behind a
// /v<major>.<minor> version segment). Every other docker request, and every
// other proxy, streams through untouched.
func guardContainersCreate(start *executorpb.ProxyStart) bool {
	return start.GetProxyName() == "docker" &&
		start.GetMethod() == "POST" &&
		isContainersCreate(start.GetPath())
}

// isContainersCreate reports whether the request path, ignoring query and an
// optional leading docker version segment, is exactly /containers/create.
func isContainersCreate(p string) bool {
	raw, _, _ := strings.Cut(p, "?")
	segs := strings.Split(strings.TrimPrefix(path.Clean(raw), "/"), "/")
	if len(segs) > 0 && dockerVersionRe.MatchString(segs[0]) {
		segs = segs[1:]
	}
	return strings.Join(segs, "/") == "containers/create"
}

// checkCreateBody inspects a docker container-create body and refuses any mount
// that would reach outside the operator's declared sandbox roots. It is
// deliberately narrow: only HostConfig.Binds and HostConfig.Mounts are read,
// nothing the container reports gates anything, and it checks bind SOURCES
// only — it does not otherwise parse or understand the docker body.
//
// Binds are refused outright because their source is a bare host path that is
// easy to get wrong; Mounts name a Type, so a bind can be told from a volume.
// A volume or tmpfs is passed (the daemon controls what those contain); a bind
// must resolve to the relay dir or under one of the declared roots.
func checkCreateBody(body []byte, roots []string, relayDir string) error {
	var req struct {
		HostConfig struct {
			Binds  []string `json:"Binds"`
			Mounts []struct {
				Type   string `json:"Type"`
				Source string `json:"Source"`
				Target string `json:"Target"`
			} `json:"Mounts"`
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
