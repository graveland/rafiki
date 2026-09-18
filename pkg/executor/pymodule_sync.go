// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/paths"
)

// pymoduleCacheDir is where synced pymodules land: paths.CacheDir() is
// documented as "disposable, regenerable data", which is exactly what a
// synced mirror is -- unlike claudeSkillsDir(), there is no third-party
// contract pinning this location, so it lives under rafiki's own directory.
func pymoduleCacheDir() string {
	return filepath.Join(paths.CacheDir(), "pymodules")
}

// SyncPyModules replaces this executor's rafiki-managed pymodule files with
// req's corpus -- the whole corpus every time, same as SyncSkills, so a
// child launching mid-sync never observes a half-written set.
func (s *Server) SyncPyModules(
	_ context.Context,
	req *connect.Request[executorpb.SyncPyModulesRequest],
) (*connect.Response[executorpb.SyncPyModulesResponse], error) {
	if !s.opts.PyModulesSync {
		return nil, connect.NewError(connect.CodePermissionDenied,
			errors.New("this executor does not accept pymodule syncs"))
	}

	root := pymoduleCacheDir()

	// Validate every name before touching the filesystem: a partial apply
	// that aborts halfway leaves a corpus no daemon ever published.
	want := make(map[string]bool, len(req.Msg.GetModules()))
	for _, m := range req.Msg.GetModules() {
		if err := validSegment(m.GetName()); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("module name: %w", err))
		}
		want[m.GetName()+".py"] = true
	}

	// Only ever write into a directory we marked, or one that does not
	// exist yet. Checked BEFORE MkdirAll: creating the directory first
	// would make the absent case unrecognisable -- an empty dir is not a
	// managed one.
	if err := assertManagedOrAbsent(root); err == nil {
		if _, statErr := os.Stat(filepath.Join(root, managedMarker)); os.IsNotExist(statErr) {
			if err := os.MkdirAll(root, 0o755); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create pymodules dir: %w", err))
			}
			if err := os.WriteFile(filepath.Join(root, managedMarker), []byte("pymodules\n"), 0o644); err != nil {
				return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("mark pymodules dir: %w", err))
			}
		}
	} else {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}

	var written, pruned int32
	for _, m := range req.Msg.GetModules() {
		changed, err := writePyModuleFile(root, m.GetName(), m.GetCode())
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if changed {
			written++
		}
	}

	n, err := prunePyModuleFiles(root, want)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	pruned = n

	return connect.NewResponse(&executorpb.SyncPyModulesResponse{Written: written, Pruned: pruned}), nil
}

// writePyModuleFile writes name+".py" atomically (write to a sibling temp
// file, then rename) and reports whether the content actually changed, so a
// converged fleet stays quiet on repeated syncs.
func writePyModuleFile(root, name, code string) (bool, error) {
	final := filepath.Join(root, name+".py")
	existing, err := os.ReadFile(final)
	if err == nil && string(existing) == code {
		return false, nil
	}
	tmp, err := os.CreateTemp(root, ".rafiki-staging-*")
	if err != nil {
		return false, fmt.Errorf("stage %s: %w", name, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename has moved it
	if _, err := tmp.WriteString(code); err != nil {
		tmp.Close()
		return false, fmt.Errorf("write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return false, fmt.Errorf("publish %s: %w", name, err)
	}
	return true, nil
}

// prunePyModuleFiles removes managed .py files absent from want, and
// touches nothing else -- in particular never the managedMarker file itself.
func prunePyModuleFiles(root string, want map[string]bool) (int32, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("read pymodules dir: %w", err)
	}
	var pruned int32
	for _, e := range entries {
		name := e.Name()
		if name == managedMarker || want[name] || e.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(root, name)); err != nil {
			return pruned, fmt.Errorf("prune %s: %w", name, err)
		}
		pruned++
	}
	return pruned, nil
}
