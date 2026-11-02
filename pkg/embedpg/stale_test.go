package embedpg

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

// staleServer is a Server whose data directory records a postmaster started
// from runningBin, and whose own tree is binDir.
func staleServer(t *testing.T, runningBin, binDir string) Server {
	t.Helper()
	data := t.TempDir()
	opts := runningBin + ` "-D" "` + data + `" "-p" "5432"` + "\n"
	if err := os.WriteFile(filepath.Join(data, "postmaster.opts"), []byte(opts), 0o600); err != nil {
		t.Fatalf("write postmaster.opts: %v", err)
	}
	return Server{BinDir: binDir, DataDir: data}
}

func TestRunningBinaryReadsTheFirstToken(t *testing.T) {
	c := assert.NewCollecting(t)
	s := staleServer(t, "/cache/with space/18.3.0/bin/postgres", "/cache/18.6-1")
	c.Eq("/cache/with space/18.3.0/bin/postgres", s.runningBinary(), "a path with a space survives")
	c.Eq("", Server{DataDir: t.TempDir()}.runningBinary(), "no postmaster.opts is no answer")
}

func TestStale(t *testing.T) {
	c := assert.NewCollecting(t)
	tree := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tree, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tree, "bin", "postgres"), nil, 0o755); err != nil {
		t.Fatalf("write postgres: %v", err)
	}

	c.True(staleServer(t, "/cache/18.3.0/bin/postgres", tree).stale(), "another tree is stale")
	c.False(staleServer(t, filepath.Join(tree, "bin", "postgres"), tree).stale(), "this tree is current")
	c.False(Server{BinDir: tree, DataDir: t.TempDir()}.stale(), "an unreadable opts file is never stale: nothing to act on")

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(tree, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	c.False(staleServer(t, filepath.Join(link, "bin", "postgres"), tree).stale(), "the same tree through a symlink is current")
}

// Without permission to restart, a stale server is left running and the
// operator is told how to finish the upgrade: nothing is stopped.
func TestReplaceStaleWithoutPermissionOnlyWarns(t *testing.T) {
	for name, allow := range map[string]func() bool{"nil": nil, "false": func() bool { return false }} {
		t.Run(name, func(t *testing.T) {
			c := assert.NewAborting(t)
			var progress bytes.Buffer
			s := staleServer(t, "/cache/18.3.0/bin/postgres", t.TempDir())
			s.Progress = &progress
			s.RestartStale = allow

			c.NoError(s.replaceStale(context.Background()), "replaceStale")
			c.StrContains(progress.String(), "still running from /cache/18.3.0/bin/postgres")
			c.StrContains(progress.String(), "Restart the rafiki service")
		})
	}
}

func TestReplaceStaleIsSilentWhenCurrent(t *testing.T) {
	c := assert.NewAborting(t)
	tree := t.TempDir()
	var progress bytes.Buffer
	s := staleServer(t, filepath.Join(tree, "bin", "postgres"), tree)
	s.Progress = &progress
	s.RestartStale = func() bool { t.Fatal("asked whether to restart a current server"); return true }

	c.NoError(s.replaceStale(context.Background()), "replaceStale")
	c.Eq("", progress.String(), "nothing to say")
}
