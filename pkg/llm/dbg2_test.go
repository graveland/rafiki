package llm

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"
)

func TestDbgResolve(t *testing.T) {
	set := routingOnlySet([]string{"alias-host"})
	_, modelID, alias, err := set.Resolve("openrouter/glm-flash:batch")
	t.Logf("modelID=%q alias=%+v err=%v", modelID, alias, err)
	var _ = providers.Default
}
