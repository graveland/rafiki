// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

func TestResumeTextLine(t *testing.T) {
	cases := []struct {
		name    string
		results []resumeResult
		want    string
	}{
		{"named", []resumeResult{{ChildID: "c_9", Name: "worker", Resp: &rafikiv1.ResumeResponse{ChildId: "c_9"}}}, "resumed c_9 (worker)\n"},
		{"name could not be resolved", []resumeResult{{ChildID: "c_9", Resp: &rafikiv1.ResumeResponse{ChildId: "c_9"}}}, "resumed c_9\n"},
		{
			"multiple targets",
			[]resumeResult{
				{ChildID: "c_1", Name: "a", Resp: &rafikiv1.ResumeResponse{ChildId: "c_1"}},
				{ChildID: "c_2", Name: "b", Resp: &rafikiv1.ResumeResponse{ChildId: "c_2"}},
			},
			"resumed c_1 (a)\nresumed c_2 (b)\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			var buf bytes.Buffer
			c.Require().NoError(renderResume(&buf, len(tc.results), tc.results, outputTable), "renderResume")
			c.Eq(tc.want, buf.String(), "output")
		})
	}
}

// The Resume response's canonical protojson is the JSON contract for a
// single requested target — pretty in -j, one compact line in -J. The wire
// response carries the child id only.
func TestResumeJSONProtojson(t *testing.T) {
	c := assert.NewCollecting(t)
	results := []resumeResult{{ChildID: "c_9", Name: "worker", Resp: &rafikiv1.ResumeResponse{ChildId: "c_9"}}}

	var buf bytes.Buffer
	c.Require().NoError(renderResume(&buf, 1, results, outputJSON), "renderResume json")
	const wantJSON = "{\n  \"childId\": \"c_9\"\n}\n"
	c.Eq(wantJSON, buf.String(), "json output")

	// JSONL: the record as one compact line.
	var jl bytes.Buffer
	c.Require().NoError(renderResume(&jl, 1, results, outputJSONL), "renderResume jsonl")
	c.Eq(`{"childId":"c_9"}`+"\n", jl.String(), "jsonl output")
}

// Multiple requested targets wrap in the `{"rows":[...]}` envelope shared by
// the CLI's other multi-target verbs, rather than the bare single-object
// shape reserved for a single requested target.
func TestResumeJSONMultipleTargets(t *testing.T) {
	c := assert.NewCollecting(t)
	results := []resumeResult{
		{ChildID: "c_1", Resp: &rafikiv1.ResumeResponse{ChildId: "c_1"}},
		{ChildID: "c_2", Resp: &rafikiv1.ResumeResponse{ChildId: "c_2"}},
	}

	var buf bytes.Buffer
	c.Require().NoError(renderResume(&buf, 2, results, outputJSON), "renderResume json")
	const wantJSON = "{\n  \"rows\": [\n    {\n      \"childId\": \"c_1\"\n    },\n    {\n      \"childId\": \"c_2\"\n    }\n  ]\n}\n"
	c.Eq(wantJSON, buf.String(), "json output")

	var jl bytes.Buffer
	c.Require().NoError(renderResume(&jl, 2, results, outputJSONL), "renderResume jsonl")
	c.Eq(`{"childId":"c_1"}`+"\n"+`{"childId":"c_2"}`+"\n", jl.String(), "jsonl output")
}
