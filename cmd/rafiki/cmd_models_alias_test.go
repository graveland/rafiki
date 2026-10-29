// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"

	"github.com/multigres/testkit/assert"
)

// aliasStubControl serves the three model-alias RPCs and records what the CLI
// sent. It mirrors the daemon's own refusals where the tests exercise them: an
// alias with no live row is not_found on delete, which the CLI surfaces rather
// than printing a confirmation.
type aliasStubControl struct {
	rafikiv1connect.UnimplementedControlHandler
	mu         sync.Mutex
	putReqs    []*connect.Request[rafikiv1.PutModelAliasRequest]
	deleteReqs []*connect.Request[rafikiv1.DeleteModelAliasRequest]
	listCalls  int
	rows       []*rafikiv1.ModelAliasRow
	deleteErr  error
}

func (s *aliasStubControl) ListModelAliases(
	_ context.Context, _ *connect.Request[rafikiv1.ListModelAliasesRequest],
) (*connect.Response[rafikiv1.ListModelAliasesResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	return connect.NewResponse(&rafikiv1.ListModelAliasesResponse{Rows: s.rows}), nil
}

func (s *aliasStubControl) PutModelAlias(
	_ context.Context, req *connect.Request[rafikiv1.PutModelAliasRequest],
) (*connect.Response[rafikiv1.PutModelAliasResponse], error) {
	s.mu.Lock()
	s.putReqs = append(s.putReqs, req)
	s.mu.Unlock()
	if req.Msg.GetName() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	return connect.NewResponse(&rafikiv1.PutModelAliasResponse{Row: &rafikiv1.ModelAliasRow{
		Name:      req.Msg.GetName(),
		Target:    req.Msg.GetTarget(),
		CreatedAt: timestamppb.New(time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)),
		UpdatedAt: timestamppb.New(time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)),
	}}), nil
}

func (s *aliasStubControl) DeleteModelAlias(
	_ context.Context, req *connect.Request[rafikiv1.DeleteModelAliasRequest],
) (*connect.Response[rafikiv1.DeleteModelAliasResponse], error) {
	s.mu.Lock()
	s.deleteReqs = append(s.deleteReqs, req)
	delErr := s.deleteErr
	s.mu.Unlock()
	if delErr != nil {
		return nil, delErr
	}
	return connect.NewResponse(&rafikiv1.DeleteModelAliasResponse{}), nil
}

func (s *aliasStubControl) lastPut() *connect.Request[rafikiv1.PutModelAliasRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.putReqs) == 0 {
		return nil
	}
	return s.putReqs[len(s.putReqs)-1]
}

func (s *aliasStubControl) lastDelete() *connect.Request[rafikiv1.DeleteModelAliasRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.deleteReqs) == 0 {
		return nil
	}
	return s.deleteReqs[len(s.deleteReqs)-1]
}

func (s *aliasStubControl) setRows(rows []*rafikiv1.ModelAliasRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = rows
}

// serveAliasScratch seeds one socket profile "it" at a short temp dir and
// serves the alias stub on the profile's own socket — the path
// newConnectEndpoint dials. dir must be a SHORT path: unix socket paths are
// capped at ~104 bytes.
func serveAliasScratch(t *testing.T, stub *aliasStubControl) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp, not t.TempDir: the UDS path cap.
	dir, err := os.MkdirTemp("", "rafiki-ax")
	assert.NewAborting(t).NoError(err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	aliasPath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, sock, aliasPath, handler)
	writeTokenedProfile(t, sock, "")
}

// TestModelsAliasSetListDelete drives set → list → delete against the
// in-process Connect harness: the name and target reach the daemon verbatim,
// the confirmation names both, the table is NAME / TARGET / UPDATED with the
// stamp rendered the routing table's way, and -j/-J are the response's
// canonical protojson.
func TestModelsAliasSetListDelete(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &aliasStubControl{}
	serveAliasScratch(t, stub)

	// set: name and target pass through untouched, and the confirmation echoes
	// the daemon's stored row.
	root, out := userTestRoot(t, newModelsAliasSetCmd(), "set", "ds", "openrouter/~deepseek/deepseek-flash-latest[sort=price]")
	c.NoError(root.Execute(), "alias set failed: %v\n%s", out.String())
	req := stub.lastPut()
	c.NotNil(req, "the stub never saw a PutModelAlias request")
	c.Eq("ds", req.Msg.GetName(), "alias name")
	c.Eq("openrouter/~deepseek/deepseek-flash-latest[sort=price]", req.Msg.GetTarget(), "alias target")
	c.StrContains(out.String(), "set ds → openrouter/~deepseek/deepseek-flash-latest[sort=price]", "set output:\n")

	// list: the table carries the stub's row, and the stamp is the local
	// date+time render of the wire timestamp, never the raw string.
	row := &rafikiv1.ModelAliasRow{
		Name:      "ds",
		Target:    "openrouter/~deepseek/deepseek-flash-latest[sort=price]",
		CreatedAt: timestamppb.New(time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)),
		UpdatedAt: timestamppb.New(time.Date(2026, 10, 29, 10, 30, 0, 0, time.UTC)),
	}
	stub.setRows([]*rafikiv1.ModelAliasRow{row})
	root, out = userTestRoot(t, newModelsAliasListCmd(), "list")
	c.NoError(root.Execute(), "alias list failed: %v\n%s", out.String())
	rendered := out.String()
	for _, want := range []string{"NAME", "TARGET", "UPDATED", "ds", "openrouter/~deepseek/deepseek-flash-latest[sort=price]"} {
		c.StrContains(rendered, want, "alias list table missing")
	}
	c.StrContains(rendered, row.GetUpdatedAt().AsTime().Local().Format(time.DateTime),
		"UPDATED column missing the local stamp:\n%s", rendered)

	// -j: the canonical protojson of the whole response — the rows envelope.
	root, out = userTestRoot(t, newModelsAliasListCmd(), "list", "-j")
	c.NoError(root.Execute(), "alias list -j failed: %v\n%s", out.String())
	var pretty map[string]any
	c.NoError(json.Unmarshal(out.Bytes(), &pretty), "alias list -j printed non-JSON:\n%s", out.String())
	rows, ok := pretty["rows"].([]any)
	c.True(ok && len(rows) == 1, "alias list -j rows = %v, want the one live row", pretty["rows"])

	// -J: one compact record per row, no envelope.
	root, out = userTestRoot(t, newModelsAliasListCmd(), "list", "-J")
	c.NoError(root.Execute(), "alias list -J failed: %v\n%s", out.String())
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	c.Require().Len(lines, 1, "alias list -J printed %d lines, want one per row:\n%s", len(lines), out.String())
	var got map[string]any
	c.NoError(json.Unmarshal([]byte(lines[0]), &got), "JSONL line %q", lines[0])
	c.Eq("ds", got["name"], "JSONL row's name")
	c.Eq("openrouter/~deepseek/deepseek-flash-latest[sort=price]", got["target"], "JSONL row's target")

	// delete: the name reaches the daemon; the confirmation names it.
	root, out = userTestRoot(t, newModelsAliasDeleteCmd(), "delete", "ds")
	c.NoError(root.Execute(), "alias delete failed: %v\n%s", out.String())
	del := stub.lastDelete()
	c.NotNil(del, "the stub never saw a DeleteModelAlias request")
	c.Eq("ds", del.Msg.GetName(), "delete name")
	c.StrContains(out.String(), "deleted alias ds", "delete output:\n")
}

// TestModelsAliasListRendersCannedResponse pins the three output modes against
// a canned response, without any daemon: the table's columns and absent-value
// render, -j as the rows envelope, and -J as unwrapped compact records.
func TestModelsAliasListRendersCannedResponse(t *testing.T) {
	c := assert.NewCollecting(t)
	resp := &rafikiv1.ListModelAliasesResponse{Rows: []*rafikiv1.ModelAliasRow{
		{
			Name:      "ds",
			Target:    "openrouter/~deepseek/deepseek-flash-latest[sort=price]",
			UpdatedAt: timestamppb.New(time.Date(2026, 10, 29, 10, 30, 0, 0, time.UTC)),
		},
		{Name: "sparse"}, // no target, no stamp: both render as "-"
	}}

	var table bytes.Buffer
	c.NoError(emitModelAliases(&table, resp, outputAuto, false), "table render")
	rendered := table.String()
	for _, want := range []string{"NAME", "TARGET", "UPDATED", "ds", "sparse",
		"openrouter/~deepseek/deepseek-flash-latest[sort=price]"} {
		c.StrContains(rendered, want, "table missing")
	}
	c.StrContains(rendered, resp.GetRows()[0].GetUpdatedAt().AsTime().Local().Format(time.DateTime),
		"first row's stamp:")
	// The sparse row's absent target and stamp both render as a dash, never as
	// an empty cell.
	c.StrContains(rendered, "│ sparse │ -", "an absent target must render as a dash:\n%s", rendered)

	var pretty bytes.Buffer
	c.NoError(emitModelAliases(&pretty, resp, outputJSON, false), "-j render")
	var env map[string]any
	c.NoError(json.Unmarshal(pretty.Bytes(), &env), "-j printed non-JSON:\n%s", pretty.String())
	rows, ok := env["rows"].([]any)
	c.True(ok && len(rows) == 2, "-j rows = %v, want both", env["rows"])

	var jsonl bytes.Buffer
	c.NoError(emitModelAliases(&jsonl, resp, outputJSONL, false), "-J render")
	records := strings.Split(strings.TrimSpace(jsonl.String()), "\n")
	c.Require().Len(records, 2, "-J printed %d lines, want one per row:\n%s", len(records), jsonl.String())
	var first map[string]any
	c.NoError(json.Unmarshal([]byte(records[0]), &first), "-J line %q", records[0])
	c.Eq("ds", first["name"], "-J name")
	c.Eq("openrouter/~deepseek/deepseek-flash-latest[sort=price]", first["target"], "-J target")
}

// TestModelsAliasDeleteCompletionReadsListModelAliases proves the completion
// source: the candidates are exactly the daemon's alias names, filtered by the
// typed prefix, and a failing daemon yields silence rather than an error (a
// completion handler must never exit, block or print).
func TestModelsAliasDeleteCompletionReadsListModelAliases(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &aliasStubControl{rows: []*rafikiv1.ModelAliasRow{
		{Name: "ds"}, {Name: "deepseek"}, {Name: "fast"},
	}}
	serveAliasScratch(t, stub)

	cmd := newModelsAliasDeleteCmd()
	root, _ := userTestRoot(t, cmd, "delete")
	_ = root
	// The command needs no execution; the helper resolves the endpoint from the
	// profile environment, the same way the real completion handler does.
	got, directive := completeModelAliasArgs(cmd, nil, "d")
	c.EqDiff([]string{"deepseek", "ds"}, got, "completion of \"d\"")
	c.Eq(cobra.ShellCompDirectiveNoFileComp, directive, "directive")

	// No prefix: every alias, sorted.
	got, _ = completeModelAliasArgs(cmd, nil, "")
	c.EqDiff([]string{"deepseek", "ds", "fast"}, got, "completion of \"\"")

	// A name that matches nothing yields nothing, not the whole set.
	got, _ = completeModelAliasArgs(cmd, nil, "zzz")
	c.Empty(got, "completion of an unmatched prefix")
}

// TestModelsAliasDeleteOfAnAbsentAliasSurfacesNotFound pins that the daemon's
// not_found is the command's error, and no confirmation is printed for it.
func TestModelsAliasDeleteOfAnAbsentAliasSurfacesNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &aliasStubControl{deleteErr: connect.NewError(connect.CodeNotFound,
		errors.New("no alias named nobody"))}
	serveAliasScratch(t, stub)

	root, out := userTestRoot(t, newModelsAliasDeleteCmd(), "delete", "nobody")
	err := root.Execute()
	c.Error(err, "alias delete of an absent alias succeeded")
	c.StrContains(err.Error(), "no alias named", "error text: %v", err)
	c.NotStrContains(out.String(), "deleted", "a delete confirmation printed for a refused alias:\n%s", out.String())
}

// TestModelsAliasParentHasNoRunE pins the brief's help rule: a bare
// `rafiki models alias` prints help rather than doing something, so the parent
// carries subcommands only.
func TestModelsAliasParentHasNoRunE(t *testing.T) {
	c := assert.NewCollecting(t)
	parent := newModelsAliasCmd()
	c.Nil(parent.Run, "the alias parent must have no Run")
	c.Nil(parent.RunE, "the alias parent must have no RunE")
	names := []string{}
	for _, ch := range parent.Commands() {
		names = append(names, ch.Name())
	}
	sort.Strings(names)
	c.EqDiff([]string{"delete", "list", "set"}, names, "subcommands")
}
