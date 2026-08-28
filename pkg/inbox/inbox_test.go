// SPDX-License-Identifier: Apache-2.0

package inbox_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/inbox/inboxtest"
)

func TestMemoryConformance(t *testing.T) {
	inboxtest.RunConformance(t, func(t *testing.T) (inbox.Store, string) {
		return inbox.NewMemory(), "c_"
	})
}
