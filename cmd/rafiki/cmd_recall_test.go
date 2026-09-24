// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// TestCmdRecallContextIsSubcommand pins cobra's resolution of
// `rafiki recall context <id>`: "context" reaches the context subcommand, so
// the hit id never reads as a query word, while any other first word stays
// with recall itself as the query.
func TestCmdRecallContextIsSubcommand(t *testing.T) {
	isolateProfiles(t)

	root := newRootCmd()
	cmd, _, err := root.Find([]string{"recall", "context", "w:abc"})
	if err != nil {
		t.Fatalf("find recall context: %v", err)
	}
	if cmd.Name() != "context" {
		t.Fatalf("cobra resolved %q, want the context subcommand", cmd.Name())
	}

	// A query that merely contains "context" later in its words is unaffected.
	cmd, _, err = root.Find([]string{"recall", "big", "context", "window"})
	if err != nil {
		t.Fatalf("find multi-word query: %v", err)
	}
	if cmd.Name() != "recall" {
		t.Fatalf("cobra resolved %q, want recall itself for a multi-word query", cmd.Name())
	}
}

// TestCmdRecallSinceParsesDuration pins the --since parser: day durations
// (which Go's ParseDuration has no unit for), hour durations, and RFC3339.
func TestCmdRecallSinceParsesDuration(t *testing.T) {
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
			if err != nil {
				t.Fatalf("parseSinceArg(%q): %v", tc.in, err)
			}
			if d := got.Sub(tc.want); d > tc.tol || d < -tc.tol {
				t.Errorf("parseSinceArg(%q) = %v, want %v (±%v)", tc.in, got, tc.want, tc.tol)
			}
		})
	}

	got, err := parseSinceArg("2026-01-02T03:04:05Z")
	if err != nil {
		t.Fatalf("parseSinceArg(RFC3339): %v", err)
	}
	if !got.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("RFC3339 = %v, want the exact UTC timestamp", got)
	}

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
	put := func(body, file string) (*rafikiv1.PutMemoryRequest, error) {
		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader("piped body\n"))
		return memoryPutRequest(cmd, "proj/notes", "note1", body, file, "")
	}

	req, err := put("", "")
	if err != nil {
		t.Fatalf("put from stdin: %v", err)
	}
	if req.GetBody() != "piped body\n" {
		t.Errorf("stdin body = %q, want the piped text", req.GetBody())
	}
	if req.GetPath() != "proj/notes" || req.GetName() != "note1" {
		t.Errorf("path/name = %q/%q, want the arguments", req.GetPath(), req.GetName())
	}

	req, err = put("explicit body", "")
	if err != nil {
		t.Fatalf("put --body: %v", err)
	}
	if req.GetBody() != "explicit body" {
		t.Errorf("--body must win over stdin; got %q", req.GetBody())
	}

	req, err = put("", "-")
	if err != nil {
		t.Fatalf("put --file -: %v", err)
	}
	if req.GetBody() != "piped body\n" {
		t.Errorf("--file - body = %q, want the piped text", req.GetBody())
	}

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
	if err != nil {
		t.Fatalf("put --meta: %v", err)
	}
	if req.GetMetaJson() != `{"k":1}` {
		t.Errorf("meta_json = %q, want passthrough", req.GetMetaJson())
	}
}

// TestCmdRecallTableRendersHits pins the brief's exact table columns and the
// WHERE cell's two shapes (path/name for memories, repo·conversation name for
// conversation sources), plus the JSON arm printing the hit array.
func TestCmdRecallTableRendersHits(t *testing.T) {
	hits := []*rafikiv1.RecallHit{
		{Id: "m:1", Source: "memory", When: "2026-01-02T03:04:05Z", Path: "proj/notes", Name: "pin"},
		{Id: "w:2", Source: "window", When: "2026-01-03T03:04:05Z", Repo: "rafiki", ConversationName: "fix bug", Snippet: "needle"},
	}
	var buf bytes.Buffer
	if err := emitRecallHits(&buf, hits, outputTable, false); err != nil {
		t.Fatalf("table: %v", err)
	}
	out := buf.String()
	for _, col := range []string{"ID", "SOURCE", "WHEN", "WHERE", "SNIPPET"} {
		if !strings.Contains(out, col) {
			t.Errorf("table missing column %s:\n%s", col, out)
		}
	}
	if !strings.Contains(out, "proj/notes/pin") {
		t.Errorf("memory WHERE = %q, want proj/notes/pin", out)
	}
	if !strings.Contains(out, "rafiki·fix bug") {
		t.Errorf("conversation WHERE = %q, want rafiki·fix bug", out)
	}

	buf.Reset()
	if err := emitRecallHits(&buf, hits, outputJSON, false); err != nil {
		t.Fatalf("json: %v", err)
	}
	var arr []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &arr); err != nil {
		t.Fatalf("-o json did not print a hit array: %v\n%s", err, buf.String())
	}
	if len(arr) != 2 || arr[0]["id"] != "m:1" {
		t.Errorf("json array = %v", arr)
	}
}
