// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"sync/atomic"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// newSourceClient builds a client on the openrouter-only registry with a
// routing source and an optional static spec, plus an openrouter sender.
func newSourceClient(t *testing.T, src func() routing.Spec, static routing.Spec, openrouter Sender) *Client {
	t.Helper()
	c, err := NewClient(
		WithProviders(routingSet(t)),
		WithProviderSender("anthropic", &scriptedSender{}),
		WithProviderSender("openrouter", openrouter),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
		WithRoutingSource(src),
		WithRouting(static),
	)
	assert.NewAborting(t).NoError(err, "NewClient")
	return c
}

// TestWithRoutingSourceIsReadPerRequest proves a WithRoutingSource is consulted
// fresh on every send: a source answering spec A then spec B produces two
// requests whose provider objects differ, and the source is evaluated EXACTLY
// once per send (a second evaluation mid-request would let one request's body
// and its error decoration disagree).
func TestWithRoutingSourceIsReadPerRequest(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("one"),
		respondText("two"),
	}}
	specA := routing.Spec{Only: []string{"fireworks"}}
	specB := routing.Spec{Only: []string{"together"}}
	var calls atomic.Int32
	c := newSourceClient(t, func() routing.Spec {
		if calls.Add(1) == 1 {
			return specA
		}
		return specB
	}, routing.Spec{}, openrouter)

	sendAliasParams(t, c, "openrouter/glmflash")
	sendAliasParams(t, c, "openrouter/glmflash")

	ck.Require().Len(openrouter.lastReq, 2, "two sends")
	ck.StrContains(wireBody(t, openrouter.lastReq, 0), `"only":["fireworks"]`,
		"first request must carry the first source spec")
	ck.StrContains(wireBody(t, openrouter.lastReq, 1), `"only":["together"]`,
		"second request must carry the second source spec")
	ck.Eq(int32(2), calls.Load(), "source must be evaluated exactly once per send")
	ck.Require()
}

// TestWithRoutingSourceNilIsIgnored pins that WithRoutingSource(nil) is a
// no-op: it neither panics nor displaces the static spec installed by
// WithRouting.
func TestWithRoutingSourceNilIsIgnored(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	c := newSourceClient(t, nil, routing.Spec{Only: []string{"fireworks"}}, openrouter)

	sendAliasParams(t, c, "openrouter/glmflash")

	ck.StrContains(wireBody(t, openrouter.lastReq, 0), `"only":["fireworks"]`,
		"a nil source must leave the static WithRouting spec in place")
	ck.Require()
}

// TestWithRoutingSourceBeatsStaticRouting pins precedence: a non-nil source
// overrides the static spec installed with WithRouting.
func TestWithRoutingSourceBeatsStaticRouting(t *testing.T) {
	ck := assert.NewCollecting(t)
	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	c := newSourceClient(t, func() routing.Spec {
		return routing.Spec{Only: []string{"together"}}
	}, routing.Spec{Only: []string{"fireworks"}}, openrouter)

	sendAliasParams(t, c, "openrouter/glmflash")

	body := wireBody(t, openrouter.lastReq, 0)
	ck.StrContains(body, `"only":["together"]`, "the live source must win over the static spec")
	ck.NotStrContains(body, `"fireworks"`, "the static spec must not leak in")
	ck.Require()
}
