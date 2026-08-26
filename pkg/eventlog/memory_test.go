// SPDX-License-Identifier: Apache-2.0

package eventlog_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/eventlog"
	"go.graveland.dev/rafiki/pkg/eventlog/eventlogtest"
)

func TestMemoryConformance(t *testing.T) {
	eventlogtest.RunConformance(t, func(t *testing.T) (eventlog.Store, string) {
		return eventlog.NewMemory(), "c_test"
	})
}
