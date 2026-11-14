// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// A switch continues the same work on the same machine, so the fork keeps the
// source's home executor: --cwd and CLAUDE.md only exist there, and letting the
// daemon re-resolve home could land the continuation on a different executor
// (or on none, with a warning). An unlabelled source carries none, leaving the
// daemon to resolve it.
func TestHomeExecutorCarriedIntoAModelSwitch(t *testing.T) {
	c := assert.NewAborting(t)

	labelled := switchSpawnRequest(&rafikiv1.ChildSummary{
		ChildId: "c_01SRC",
		Cwd:     "/work/proj",
		Labels: map[string]string{
			"rafiki/home-executor": "greyshift",
			"team":                 "review",
		},
	}, "c_01SRC", "fakeor/mini")

	c.Eq("greyshift", labelled.GetHomeExecutor(),
		"a labelled source's home must ride the switch's fork request")
	c.Eq("fakeor/mini", labelled.GetModel(), "the fork runs on the requested model")
	c.Eq(protocol.KindFundi, labelled.GetKind(), "a switch always lands on fundi")
	c.Eq("c_01SRC", labelled.GetForkFrom().GetChildId(), "the fork's source")
	c.Eq("review", labelled.GetLabels()["team"],
		"the user label carries over alongside the home")
	c.Eq("", labelled.GetLabels()["rafiki/home-executor"],
		"a daemon-namespace label must not be re-sent as a user label")

	unlabelled := switchSpawnRequest(&rafikiv1.ChildSummary{
		ChildId: "c_01SRC",
		Cwd:     "/work/proj",
		Labels:  map[string]string{},
	}, "c_01SRC", "fakeor/mini")

	c.Eq("", unlabelled.GetHomeExecutor(),
		"an unlabelled source must leave the home to the daemon")
}
