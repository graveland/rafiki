package llm

import (
	"testing"
)

func TestDbgMinimal(t *testing.T) {
	set := routingOnlySet([]string{"alias-host"})
	t.Logf("set=%#v", set.Providers["openrouter"].Models)
}
