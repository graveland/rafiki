// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func TestResumeTextLine(t *testing.T) {
	cases := []struct {
		name    string
		childID string
		found   string
		want    string
	}{
		{"named", "c_9", "worker", "resumed c_9 (worker)\n"},
		{"name could not be resolved", "c_9", "", "resumed c_9\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			var buf bytes.Buffer
			c.Require().NoError(renderResume(&buf, tc.childID, tc.found, &rafikiv1.ResumeResponse{ChildId: "c_9"}, outputTable), "renderResume")
			c.Eq(tc.want, buf.String(), "output")
		})
	}
}

// The Resume response's canonical protojson is the JSON contract now — pretty
// in -j, one compact line in -J. The wire response carries the child id only
// (the framed payload's SpawnResponseData extras are gone from the wire).
func TestResumeJSONProtojson(t *testing.T) {
	c := assert.NewCollecting(t)
	resp := &rafikiv1.ResumeResponse{ChildId: "c_9"}

	var buf bytes.Buffer
	c.Require().NoError(renderResume(&buf, "c_9", "worker", proto.Message(resp), outputJSON), "renderResume json")
	const wantJSON = "{\n  \"childId\": \"c_9\"\n}\n"
	c.Eq(wantJSON, buf.String(), "json output")

	// JSONL: the record as one compact line.
	var jl bytes.Buffer
	c.Require().NoError(renderResume(&jl, "c_9", "", proto.Message(resp), outputJSONL), "renderResume jsonl")
	c.Eq(`{"childId":"c_9"}`+"\n", jl.String(), "jsonl output")
}
