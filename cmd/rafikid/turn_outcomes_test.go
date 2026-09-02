package main

import (
	"errors"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi"
)

func TestTurnOutcomeStoreTakeClearsIt(t *testing.T) {
	var s turnOutcomeStore
	s.set("c1", fundi.TurnOutcome{LimitReason: "budget exhausted"})

	got, ok := s.take("c1")
	if !ok || got.LimitReason != "budget exhausted" {
		t.Fatalf("take = %+v, %v, want the stored outcome", got, ok)
	}

	_, ok = s.take("c1")
	if ok {
		t.Error("second take found something; take must clear the entry")
	}
}

func TestTurnOutcomeStoreMissingChild(t *testing.T) {
	var s turnOutcomeStore
	if _, ok := s.take("nope"); ok {
		t.Error("take on an unknown child reported ok=true")
	}
}

func TestTurnOutcomeStoreOverwrites(t *testing.T) {
	var s turnOutcomeStore
	s.set("c1", fundi.TurnOutcome{Clean: true})
	s.set("c1", fundi.TurnOutcome{Err: errors.New("boom")})

	got, ok := s.take("c1")
	if !ok || got.Err == nil {
		t.Fatalf("take = %+v, %v, want the SECOND set's outcome", got, ok)
	}
}
