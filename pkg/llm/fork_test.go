// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestForkFromWithoutStoreIsRefused pins that a fork is a stored conversation
// by definition: with no capture store there is nothing to write the edge to,
// so the conversation is refused rather than silently created unforked.
func TestForkFromWithoutStoreIsRefused(t *testing.T) {
	c := assert.NewAborting(t)

	client := memClient(t, &scriptedSender{})
	_, err := client.Conversation(context.Background(),
		NewConversation("", "test"), ForkFrom("0b7f9a4e-0000-0000-0000-000000000000"))
	c.Require().Error(err, "a fork needs a conversation store")
	c.Eq("llm: a fork needs a conversation store", err.Error(), "the refusal names the missing store")
}
