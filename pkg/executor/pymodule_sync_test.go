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
		got, err := os.ReadFile(filepath.Join(pymoduleCacheDir(), name+".py"))
		if err != nil {
			t.Fatalf("%s.py missing: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s.py = %q, want %q", name, got, want)
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

	if _, err := os.Stat(filepath.Join(pymoduleCacheDir(), "beta.py")); !os.IsNotExist(err) {
		t.Errorf("removed module survived a sync that omitted it (err=%v)", err)
	}
	got, err := os.ReadFile(filepath.Join(pymoduleCacheDir(), "alpha.py"))
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
