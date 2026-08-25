// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/connectapi"
)

// TestControllerSatisfiesConnectSeams is a compile-time assertion with a
// runtime home: if a seam's signature drifts, this file stops compiling, which
// is the whole point. It is cheap and it is the only thing standing between a
// renamed method and a daemon that silently never wires its control plane.
func TestControllerSatisfiesConnectSeams(t *testing.T) {
	var _ connectapi.ChildLister = (*Controller)(nil)
	var _ connectapi.ChildLifecycle = connectLifecycle{}
	var _ connectapi.ConversationResolver = (*Controller)(nil)
}
