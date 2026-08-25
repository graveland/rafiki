package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
)

// TestOwnsChildRow guards the cross-daemon case. Child rows are shared now, so
// every daemon sees every daemon's children; Forget must not hard-delete the
// durable state of a child that is running somewhere else.
func TestOwnsChildRow(t *testing.T) {
	c := &Controller{daemonID: "daemon-a"}

	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"own row", map[string]string{"rafiki/daemon": "daemon-a"}, true},
		{"another daemon's row", map[string]string{"rafiki/daemon": "daemon-b"}, false},
		{"unattributed row is ours", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.ownsChildRow(childstore.Snapshot{Labels: tc.labels})
			if got != tc.want {
				t.Errorf("ownsChildRow = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOwnsChildRowWithoutDaemonID: a daemon that could not resolve an id owns
// everything, which preserves single-daemon behaviour rather than making Forget
// silently stop working.
func TestOwnsChildRowWithoutDaemonID(t *testing.T) {
	c := &Controller{daemonID: ""}
	got := c.ownsChildRow(childstore.Snapshot{Labels: map[string]string{"rafiki/daemon": "daemon-b"}})
	if !got {
		t.Error("a daemon with no id must still own every row")
	}
}
