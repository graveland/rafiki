package llm

import (
	"testing"
)

func TestDbgSplit(t *testing.T) {
	set := routingOnlySet([]string{"alias-host"})
	p, id, alias, err := set.Resolve("openrouter/glm-flash")
	t.Logf("plain: id=%q alias=%v err=%v p.Models=%v", id, alias, p.Models, err)
}
