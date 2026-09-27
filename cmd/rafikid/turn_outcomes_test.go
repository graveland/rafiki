package main

import (
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi"

	"github.com/multigres/testkit/assert"
)

func TestTurnOutcomeStoreTakeClearsIt(t *testing.T) {
	c := assert.NewCollecting(t)
	var s turnOutcomeStore
	s.set("c1", fundi.TurnOutcome{LimitReason: "budget exhausted"})

	got, ok := s.take("c1")
	c.Require().False(!ok || got.LimitReason != "budget exhausted", "take = %+v, %v, want the stored outcome", got, ok)

	_, ok = s.take("c1")
	c.False(ok, "second take found something; take must clear the entry")
}

func TestTurnOutcomeStoreMissingChild(t *testing.T) {
	var s turnOutcomeStore
	_, ok := s.take("nope")
	assert.NewCollecting(t).False(ok, "take on an unknown child reported ok=true")
}

func TestTurnOutcomeStoreOverwrites(t *testing.T) {
	var s turnOutcomeStore
	s.set("c1", fundi.TurnOutcome{Clean: true})
	s.set("c1", fundi.TurnOutcome{Err: errors.New("boom")})

	got, ok := s.take("c1")
	assert.NewAborting(t).False(!ok || got.Err == nil, "take = %+v, %v, want the SECOND set's outcome", got, ok)
}
