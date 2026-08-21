package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestNoExecutorSelectorMeansNoExecutor(t *testing.T) {
	c := &Controller{}
	got, err := c.resolveExecutor(protocol.SpawnRequest{}, "")
	if err != nil {
		t.Fatalf("an unconfigured executor is the default, not an error: %v", err)
	}
	if got != nil {
		t.Fatal("no selector must mean a nil client")
	}
}
