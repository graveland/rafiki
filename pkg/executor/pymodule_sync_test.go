// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
)

// pymoduleServer builds a Server whose pymodule sync targets a temp cache dir,
// by pointing XDG_CACHE_HOME at it. That indirection is the real mechanism, not
// a test seam: paths.CacheDir resolves the same variable on this machine.
func pymoduleServer(t *testing.T, optIn bool) *Server {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	return &Server{opts: Options{PyModulesSync: optIn}}
}

func TestSyncPyModulesRefusesWhenNotOptedIn(t *testing.T) {
	s := pymoduleServer(t, false)
	_, err := s.SyncPyModules(context.Background(), connect.NewRequest(&executorpb.SyncPyModulesRequest{}))
	if err == nil {
		t.Fatal("SyncPyModules ran on an executor that never opted in")
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("got %v, want PermissionDenied", connect.CodeOf(err))
	}
}

// A name is a file name. Anything that could escape the pymodules dir must be
// refused before a single byte is written.
func TestSyncPyModulesRejectsInvalidName(t *testing.T) {
	s := pymoduleServer(t, true)
	_, err := s.SyncPyModules(context.Background(), connect.NewRequest(&executorpb.SyncPyModulesRequest{
		Modules: []*executorpb.SyncPyModule{{Name: "../evil", Code: "x = 1\n"}},
	}))
	if err == nil {
		t.Fatal("accepted a name that can escape the pymodules directory")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("got code %v, want InvalidArgument", connect.CodeOf(err))
	}
	if _, derr := os.Stat(pymoduleCacheDir()); !os.IsNotExist(derr) {
		t.Errorf("a rejected sync wrote to disk (err=%v)", derr)
	}
}

func TestSyncPyModulesWritesAndReadsBack(t *testing.T) {
	s := pymoduleServer(t, true)
	resp, err := s.SyncPyModules(context.Background(), connect.NewRequest(&executorpb.SyncPyModulesRequest{
		Modules: []*executorpb.SyncPyModule{
			{Name: "alpha", Code: "X = 1\n"},
			{Name: "beta", Code: "Y = 2\n"},
		},
	}))
	if err != nil {
		t.Fatalf("SyncPyModules: %v", err)
	}
	if resp.Msg.GetWritten() != 2 {
		t.Errorf("written=%d, want 2", resp.Msg.GetWritten())
	}
	for name, want := range map[string]string{"alpha": "X = 1\n", "beta": "Y = 2\n"} {
		got, err := os.ReadFile(filepath.Join(pymoduleCacheDir(), name, name+".py"))
		if err != nil {
			t.Fatalf("%s/%s.py missing: %v", name, name, err)
		}
		if string(got) != want {
			t.Errorf("%s/%s.py = %q, want %q", name, name, got, want)
		}
	}
}

// A module that leaves the corpus takes its file with it; the kept one is
// untouched.
func TestSyncPyModulesPrunesRemovedModule(t *testing.T) {
	s := pymoduleServer(t, true)
	syncOnce := func(mods ...*executorpb.SyncPyModule) *executorpb.SyncPyModulesResponse {
		t.Helper()
		resp, err := s.SyncPyModules(context.Background(),
			connect.NewRequest(&executorpb.SyncPyModulesRequest{Modules: mods}))
		if err != nil {
			t.Fatalf("SyncPyModules: %v", err)
		}
		return resp.Msg
	}
	syncOnce(
		&executorpb.SyncPyModule{Name: "alpha", Code: "X = 1\n"},
		&executorpb.SyncPyModule{Name: "beta", Code: "Y = 2\n"},
	)
	resp := syncOnce(&executorpb.SyncPyModule{Name: "alpha", Code: "X = 1\n"})

	if _, err := os.Stat(filepath.Join(pymoduleCacheDir(), "beta")); !os.IsNotExist(err) {
		t.Errorf("removed module survived a sync that omitted it (err=%v)", err)
	}
	got, err := os.ReadFile(filepath.Join(pymoduleCacheDir(), "alpha", "alpha.py"))
	if err != nil || string(got) != "X = 1\n" {
		t.Errorf("kept module was collaterally damaged: content=%q err=%v", got, err)
	}
	if resp.GetPruned() != 1 {
		t.Errorf("pruned=%d, want 1", resp.GetPruned())
	}
}

// Re-syncing identical content must not rewrite files, or a converged fleet
// churns the cache directory forever.
func TestSyncPyModulesIsIdempotentOnUnchangedContent(t *testing.T) {
	s := pymoduleServer(t, true)
	req := &executorpb.SyncPyModulesRequest{
		Modules: []*executorpb.SyncPyModule{{Name: "alpha", Code: "X = 1\n"}},
	}
	if _, err := s.SyncPyModules(context.Background(), connect.NewRequest(req)); err != nil {
		t.Fatalf("first SyncPyModules: %v", err)
	}
	resp, err := s.SyncPyModules(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("second SyncPyModules: %v", err)
	}
	if resp.Msg.GetWritten() != 0 {
		t.Errorf("written=%d, want 0 for an unchanged corpus", resp.Msg.GetWritten())
	}
}

// The pre-directory layout was a flat <name>.py per module; the first sync
// from a newer binary must sweep those stale files as absent from the
// corpus, because want is now keyed by bare module name. No manual migration
// is needed anywhere in the fleet.
func TestSyncPyModulesPrunesLegacyFlatLayout(t *testing.T) {
	s := pymoduleServer(t, true)
	req := &executorpb.SyncPyModulesRequest{
		Modules: []*executorpb.SyncPyModule{{Name: "alpha", Code: "X = 1\n"}},
	}
	if _, err := s.SyncPyModules(context.Background(), connect.NewRequest(req)); err != nil {
		t.Fatalf("SyncPyModules: %v", err)
	}
	legacy := filepath.Join(pymoduleCacheDir(), "alpha.py")
	if err := os.WriteFile(legacy, []byte("X = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	resp, err := s.SyncPyModules(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("second SyncPyModules: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Errorf("legacy flat file survived a sync (err=%v)", err)
	}
	if resp.Msg.GetPruned() != 1 {
		t.Errorf("pruned=%d, want 1", resp.Msg.GetPruned())
	}
}

// A symlink entry in the managed root is unlinked, never followed: the sweep
// must not reach whatever the link points at.
func TestSyncPyModulesPrunesSymlinkWithoutFollowing(t *testing.T) {
	s := pymoduleServer(t, true)
	req := &executorpb.SyncPyModulesRequest{
		Modules: []*executorpb.SyncPyModule{{Name: "alpha", Code: "X = 1\n"}},
	}
	if _, err := s.SyncPyModules(context.Background(), connect.NewRequest(req)); err != nil {
		t.Fatalf("SyncPyModules: %v", err)
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(pymoduleCacheDir(), "sneaky")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	resp, err := s.SyncPyModules(context.Background(), connect.NewRequest(req))
	if err != nil {
		t.Fatalf("sync with a symlink entry: %v", err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("symlink survived a sync (err=%v)", err)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Errorf("the symlink's target was damaged: %v", err)
	}
	if resp.Msg.GetPruned() != 1 {
		t.Errorf("pruned=%d, want 1", resp.Msg.GetPruned())
	}
}

// A module declaring a requirements block gets its venv built and reported
// on every sync -- using the fake uv, never a real one (see writeFakeUV).
func TestSyncPyModulesBuildsVenvAndReportsReady(t *testing.T) {
	uvDir := writeFakeUV(t)
	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
	s := pymoduleServer(t, true)
	code := "# pymodule-requirements:\n# requests\n\nX = 1\n"

	resp, err := s.SyncPyModules(context.Background(), connect.NewRequest(&executorpb.SyncPyModulesRequest{
		Modules: []*executorpb.SyncPyModule{{Name: "alpha", Code: code}},
	}))
	if err != nil {
		t.Fatalf("SyncPyModules: %v", err)
	}
	results := resp.Msg.GetVenvResults()
	if len(results) != 1 {
		t.Fatalf("venv results = %d entries, want 1", len(results))
	}
	if results[0].GetName() != "alpha" {
		t.Errorf("venv result name = %q, want %q", results[0].GetName(), "alpha")
	}
	if !results[0].GetReady() {
		t.Errorf("alpha's venv not ready: %q", results[0].GetError())
	}
	if _, err := os.Stat(filepath.Join(pymoduleCacheDir(), "alpha", ".venv", "bin", "python3")); err != nil {
		t.Errorf("venv was not built into alpha's module directory: %v", err)
	}
}

// prunePyModules' whole-directory RemoveAll takes the .venv with the module:
// a module absent from the corpus must leave neither its code file nor an
// orphaned venv directory behind.
func TestSyncPyModulesPrunesVenvWithFullModuleRemoval(t *testing.T) {
	uvDir := writeFakeUV(t)
	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
	s := pymoduleServer(t, true)
	alpha := &executorpb.SyncPyModule{Name: "alpha", Code: "# pymodule-requirements:\n# requests\n\nX = 1\n"}
	beta := &executorpb.SyncPyModule{Name: "beta", Code: "Y = 2\n"}

	if _, err := s.SyncPyModules(context.Background(),
		connect.NewRequest(&executorpb.SyncPyModulesRequest{Modules: []*executorpb.SyncPyModule{alpha, beta}})); err != nil {
		t.Fatalf("first SyncPyModules: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pymoduleCacheDir(), "alpha", ".venv", "bin", "python3")); err != nil {
		t.Fatalf("alpha's venv was not built during the first sync: %v", err)
	}

	resp, err := s.SyncPyModules(context.Background(),
		connect.NewRequest(&executorpb.SyncPyModulesRequest{Modules: []*executorpb.SyncPyModule{beta}}))
	if err != nil {
		t.Fatalf("second SyncPyModules: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pymoduleCacheDir(), "alpha", "alpha.py")); !os.IsNotExist(err) {
		t.Errorf("removed module's code file survived a sync that omitted it (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(pymoduleCacheDir(), "alpha", ".venv")); !os.IsNotExist(err) {
		t.Errorf("removed module's .venv survived independently (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(pymoduleCacheDir(), "alpha")); !os.IsNotExist(err) {
		t.Errorf("removed module's directory survived as an empty husk (err=%v)", err)
	}
	if resp.Msg.GetPruned() != 1 {
		t.Errorf("pruned=%d, want 1", resp.Msg.GetPruned())
	}
}
