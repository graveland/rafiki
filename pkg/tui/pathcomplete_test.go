package tui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestCompletePath(t *testing.T) {
	ck := assert.NewCollecting(t)
	root := t.TempDir()
	for _, d := range []string{"alpha", "alpine", "beta"} {
		ck.NoError(os.Mkdir(filepath.Join(root, d), 0o755), "mkdir")
	}
	ck.NoError(os.WriteFile(filepath.Join(root, "alfile"), nil, 0o644), "file")

	ck.Eq(root+"/beta/", completePath(root+"/b"), "unique match gets a separator")
	ck.Eq(root+"/alp", completePath(root+"/a"), "ambiguous completes to the common prefix")
	ck.Eq(root+"/zz", completePath(root+"/zz"), "no match leaves input")
}
