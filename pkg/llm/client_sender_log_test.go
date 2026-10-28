// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// TestNewClientLogsOwnerBoundKindWithoutWarning pins the ruling that a provider
// with no key-based sender is not a fault. openai-responses-chatgpt binds its
// credential per owner at spawn (NewChatGPTResponsesSender), so SenderFor
// refusing it is the designed state; warning "provider unavailable" for it at
// every boot sends the operator hunting a bug that is not there. A genuinely
// broken entry must still warn, or the quiet path would swallow real breakage.
func TestNewClientLogsOwnerBoundKindWithoutWarning(t *testing.T) {
	c := assert.NewAborting(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	set := &providers.Set{
		DefaultProvider: "chatgpt",
		Providers: map[string]providers.Provider{
			"chatgpt": {Name: "chatgpt", Kind: providers.KindOpenAIResponsesChatGPT},
			"broken":  {Name: "broken", Kind: providers.Kind("no-such-kind")},
		},
	}
	client, err := NewClient(WithProviders(set), WithLogger(logger))
	c.Require().NoError(err, "NewClient")

	var ownerBoundWarn, ownerBoundDebug, brokenWarn int
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		switch {
		case strings.Contains(line, "provider=chatgpt") && strings.Contains(line, "level=WARN"):
			ownerBoundWarn++
		case strings.Contains(line, "provider=chatgpt") && strings.Contains(line, "level=DEBUG"):
			ownerBoundDebug++
			c.StrContains(line, "bound per owner at spawn", "the debug line must say why, logged: %s", line)
			c.StrContains(line, "key-based sender", "the debug line must name the fact, logged: %s", line)
		case strings.Contains(line, "provider=broken") && strings.Contains(line, "level=WARN"):
			brokenWarn++
		}
	}
	c.Eq(0, ownerBoundWarn, "WARN lines naming the owner-bound provider (logged: %s)", buf.String())
	c.Eq(1, ownerBoundDebug, "DEBUG lines naming the owner-bound provider (logged: %s)", buf.String())
	c.Eq(1, brokenWarn, "WARN lines naming the broken provider (logged: %s)", buf.String())

	c.StrContains(buf.String(), "llm: provider unavailable", "the broken provider's warning text")
	c.StrContains(buf.String(), "unknown kind", "the broken provider's reason")
	c.StrContains(buf.String(), "no-such-kind", "the broken provider's kind")

	// Neither provider got an implicit sender: callModel's "provider not
	// configured" error is the backstop if anything uses them.
	c.Nil(client.senders["chatgpt"], "a key-based sender was invented for the owner-bound kind")
	c.Nil(client.senders["broken"], "a sender was built for an unknown kind")
}
