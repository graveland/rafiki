package childstore

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestSnapshot_RoundTripsKindAndConfigDir(t *testing.T) {
	c := assert.NewAborting(t)
	s := &Session{
		ChildID:   "c1",
		Cwd:       "/tmp",
		Kind:      "claude",
		ConfigDir: "/home/u/.claude-personal",
	}
	snap := s.Snapshot()
	c.Eq("claude", snap.Kind, "snapshot Kind")
	c.Eq("/home/u/.claude-personal", snap.ConfigDir, "snapshot ConfigDir =")
}
