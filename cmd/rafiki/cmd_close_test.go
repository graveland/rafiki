// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestCloseCommandKeepsForgetAsAnAlias(t *testing.T) {
	cmd := newCloseCmd()
	if cmd.Name() != "close" {
		t.Errorf("Name() = %q, want close", cmd.Name())
	}
	var hasForget, hasRM bool
	for _, a := range cmd.Aliases {
		switch a {
		case "forget":
			hasForget = true
		case "rm":
			hasRM = true
		}
	}
	if !hasForget {
		t.Error("`forget` must stay an alias: it is in muscle memory and in scripts")
	}
	if !hasRM {
		t.Error("`rm` was an alias before the rename and must survive it")
	}
}

func TestCloseCmd_HasStopTimeoutFlags(t *testing.T) {
	cmd := newCloseCmd()
	if cmd.Flags().Lookup("shutdown-timeout") == nil {
		t.Error("--shutdown-timeout missing: close must be able to override the stop-first step's timeout")
	}
	if cmd.Flags().Lookup("kill-timeout") == nil {
		t.Error("--kill-timeout missing: close must be able to override the stop-first step's timeout")
	}
}

func TestCloseAllExitedTextCount(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"three closed", `{"count":3,"children":["a","b","c"]}`, "closed 3 exited children\n"},
		{"zero closed", `{"count":0}`, "no exited children to close\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := renderCloseAllExited(&buf, json.RawMessage(tc.raw), outputTable); err != nil {
				t.Fatalf("renderCloseAllExited: %v", err)
			}
			if buf.String() != tc.want {
				t.Errorf("output = %q, want %q", buf.String(), tc.want)
			}
		})
	}
}

// JSON mode stays the raw payload passthrough it has always been — the client
// reshapes nothing, so whatever the daemon sent (including fields this client
// does not know about) reaches the consumer.
func TestCloseAllExitedJSONRawPassthrough(t *testing.T) {
	raw := json.RawMessage(`{"count":2,"children":["a","b"],"extra":"field"}`)
	var buf bytes.Buffer
	if err := renderCloseAllExited(&buf, raw, outputJSON); err != nil {
		t.Fatalf("renderCloseAllExited: %v", err)
	}
	var want bytes.Buffer
	enc := json.NewEncoder(&want)
	enc.SetIndent("", "  ")
	if err := enc.Encode(raw); err != nil {
		t.Fatalf("encode reference: %v", err)
	}
	if buf.String() != want.String() {
		t.Errorf("json output changed:\nold: %s\nnew: %s", want.String(), buf.String())
	}
}

func TestCloseAllExitedJSONLIds(t *testing.T) {
	var buf bytes.Buffer
	raw := json.RawMessage(`{"count":3,"children":["c_a","c_b","c_c"]}`)
	if err := renderCloseAllExited(&buf, raw, outputJSONL); err != nil {
		t.Fatalf("renderCloseAllExited: %v", err)
	}
	if buf.String() != "\"c_a\"\n\"c_b\"\n\"c_c\"\n" {
		t.Errorf("jsonl output = %q, want one id per line", buf.String())
	}

	// A zero-count response carries no children — zero lines.
	var empty bytes.Buffer
	if err := renderCloseAllExited(&empty, json.RawMessage(`{"count":0}`), outputJSONL); err != nil {
		t.Fatalf("renderCloseAllExited zero: %v", err)
	}
	if empty.Len() != 0 {
		t.Errorf("zero-count jsonl output = %q, want nothing", empty.String())
	}
}
