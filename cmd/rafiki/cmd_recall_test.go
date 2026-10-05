// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// TestCmdRecallContextIsSubcommand pins cobra's resolution of
// `rafiki recall context <id>`: "context" reaches the context subcommand, so
// the hit id never reads as a query word, while any other first word stays
// with recall itself as the query.
func TestCmdRecallContextIsSubcommand(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)

	root := newRootCmd()
	cmd, _, err := root.Find([]string{"recall", "context", "w:abc"})
	c.NoError(err, "find recall context")
	c.Eq("context", cmd.Name(), "cobra resolved")

	// A query that merely contains "context" later in its words is unaffected.
	cmd, _, err = root.Find([]string{"recall", "big", "context", "window"})
	c.NoError(err, "find multi-word query")
	c.Eq("recall", cmd.Name(), "cobra resolved")
}

// TestCmdRecallSinceParsesDuration pins the --since parser: day durations
// (which Go's ParseDuration has no unit for), hour durations, and RFC3339.
func TestCmdRecallSinceParsesDuration(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct {
		name string
		in   string
		want time.Time // for relative forms, compared with a tolerance
		tol  time.Duration
	}{
		{name: "days", in: "7d", want: time.Now().Add(-7 * 24 * time.Hour), tol: 5 * time.Second},
		{name: "hours", in: "36h", want: time.Now().Add(-36 * time.Hour), tol: 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSinceArg(tc.in)
			assert.NewAborting(t).NoError(err, "parseSinceArg(%q)", tc.in)
			if d := got.Sub(tc.want); d > tc.tol || d < -tc.tol {
				t.Errorf("parseSinceArg(%q) = %v, want %v (±%v)", tc.in, got, tc.want, tc.tol)
			}
		})
	}

	got, err := parseSinceArg("2026-01-02T03:04:05Z")
	c.Require().NoError(err, "parseSinceArg(RFC3339)")
	c.True(got.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)), "RFC3339 = %v, want the exact UTC timestamp", got)

	if t2, err := parseSinceArg(""); err != nil || t2 != nil {
		t.Errorf("parseSinceArg(\"\") = %v, %v, want nil, nil", t2, err)
	}
	if _, err := parseSinceArg("yesterday"); err == nil {
		t.Error("parseSinceArg(yesterday) = nil error, want a failure")
	}
}

// TestCmdMemoryPutReadsStdin pins put's body resolution: with neither --body
// nor --file the body is read from stdin, --body wins over stdin, --file - is
// stdin too, and an empty resolved body is refused.
func TestCmdMemoryPutReadsStdin(t *testing.T) {
	c := assert.NewCollecting(t)
	put := func(body, file string) (*rafikiv1.PutMemoryRequest, error) {
		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader("piped body\n"))
		return memoryPutRequest(cmd, "proj/notes", "note1", body, file, "")
	}

	req, err := put("", "")
	c.Require().NoError(err, "put from stdin")
	c.Eq("piped body\n", req.GetBody(), "stdin body")
	if req.GetPath() != "proj/notes" || req.GetName() != "note1" {
		t.Errorf("path/name = %q/%q, want the arguments", req.GetPath(), req.GetName())
	}

	req, err = put("explicit body", "")
	c.Require().NoError(err, "put --body")
	c.Eq("explicit body", req.GetBody(), "--body must win over stdin; got")

	req, err = put("", "-")
	c.Require().NoError(err, "put --file -")
	c.Eq("piped body\n", req.GetBody(), "--file - body")

	// An empty piped body is refused rather than saved silently.
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(""))
	if _, err := memoryPutRequest(cmd, "p", "n", "", "", ""); err == nil {
		t.Error("empty body accepted")
	}

	// --meta must be valid JSON before any dial.
	cmd = &cobra.Command{}
	cmd.SetIn(strings.NewReader("b"))
	if _, err := memoryPutRequest(cmd, "p", "n", "b", "", "{oops"); err == nil {
		t.Error("invalid --meta accepted")
	}
	req, err = memoryPutRequest(cmd, "p", "n", "b", "", `{"k":1}`)
	c.Require().NoError(err, "put --meta")
	c.Eq(`{"k":1}`, req.GetMetaJson(), "meta_json")
}

// TestCmdRecallTableRendersHits pins the brief's exact table columns and the
// WHERE cell's two shapes (path/name for memories, repo·conversation name for
// conversation sources), plus the JSON arm printing the hit array.
func TestCmdRecallTableRendersHits(t *testing.T) {
	c := assert.NewCollecting(t)
	hits := []*rafikiv1.RecallHit{
		{Id: "m:1", Source: "memory", When: timestamppb.New(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)), Path: "proj/notes", Name: "pin"},
		{Id: "w:2", Source: "window", When: timestamppb.New(time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC)), Repo: "rafiki", ConversationName: "fix bug", Snippet: "needle"},
	}
	var buf bytes.Buffer
	c.Require().NoError(emitRecallHits(&buf, hits, outputTable, false), "table")
	out := buf.String()
	for _, col := range []string{"ID", "SOURCE", "WHEN", "WHERE", "SNIPPET"} {
		c.StrContains(out, col, "table missing column")
	}
	c.StrContains(out, "proj/notes/pin", "memory WHERE")
	c.StrContains(out, "rafiki·fix bug", "conversation WHERE")

	buf.Reset()
	c.Require().NoError(emitRecallHits(&buf, hits, outputJSON, false), "json")
	var arr []map[string]any
	err := json.Unmarshal(buf.Bytes(), &arr)
	c.Require().NoError(err, "-o json did not print a hit array: %v\n%s", err, buf.String())
	c.False(len(arr) != 2 || arr[0]["id"] != "m:1", "json array = %v", arr)
}
