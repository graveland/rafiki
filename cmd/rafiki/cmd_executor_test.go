package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

// Two rows minted in the same window: UUIDv7s share their leading timestamp
// bits, so everything before the final group is identical and only the tail
// distinguishes them.
const (
	fixturePrefix = "0198e5f2-9c3a-7def-8a1b-"
	fixtureTailA  = "4c2d9e0f1a2b"
	fixtureTailB  = "998877665544"
)

func TestShortExecutorIDKeepsTheTail(t *testing.T) {
	c := assert.NewAborting(t)
	full := fixturePrefix + fixtureTailA
	if got := shortExecutorID(full); got != fixtureTailA {
		t.Fatalf("shortExecutorID(%s) = %s, want the tail %s", full, got, fixtureTailA)
	}
	c.Eq(fixtureTailA, shortExecutorID(fixtureTailA), "short ids must pass through unchanged, got")
	c.Eq("exactly12ch", shortExecutorID("exactly12ch"), "ids of exactly %d chars must not be mangled, got", executorShortIDLen)
}

func TestFilterExecutorsForDelete(t *testing.T) {
	execs := []*rafikiv1.ExecutorRow{
		{Id: "disabled-online", Enabled: false, Connected: true},
		{Id: "enabled-offline", Enabled: true},
		{Id: "enabled-online", Enabled: true, Connected: true},
		{Id: "disabled-offline", Enabled: false},
	}

	ids := func(got []*rafikiv1.ExecutorRow) []string {
		out := make([]string, len(got))
		for i, e := range got {
			out[i] = e.GetId()
		}
		return out
	}

	t.Run("all-disabled selects Enabled==false regardless of connection", func(t *testing.T) {
		got := ids(filterExecutorsForDelete(execs, true, false))
		want := []string{"disabled-online", "disabled-offline"}
		assert.NewAborting(t).EqDiff(want, got, "got")
	})

	t.Run("all-offline selects Connected==false regardless of enabled", func(t *testing.T) {
		got := ids(filterExecutorsForDelete(execs, false, true))
		want := []string{"enabled-offline", "disabled-offline"}
		assert.NewAborting(t).EqDiff(want, got, "got")
	})

	t.Run("both flags union rather than intersect", func(t *testing.T) {
		got := ids(filterExecutorsForDelete(execs, true, true))
		want := []string{"disabled-online", "enabled-offline", "disabled-offline"}
		assert.NewAborting(t).EqDiff(want, got, "got")
	})

	t.Run("neither flag selects nothing", func(t *testing.T) {
		got := filterExecutorsForDelete(execs, false, false)
		assert.NewAborting(t).Empty(got, "got")
	})
}

func TestRenderExecutorTableShowsTailIDs(t *testing.T) {
	c := assert.NewCollecting(t)
	execs := []*rafikiv1.ExecutorRow{
		{Id: fixturePrefix + fixtureTailA,
			Enabled: true, Connected: true,
			Labels: map[string]string{"b": "2", "a": "1", "machine": "laptop"}},
		{Id: fixturePrefix + fixtureTailB,
			Labels: map[string]string{"env": "work", "machine": "rack"}},
	}

	var buf bytes.Buffer
	c.Require().NoError(renderExecutorTable(&buf, execs, false), "renderExecutorTable")
	out := buf.String()

	for _, want := range []string{fixtureTailA, fixtureTailB, "laptop", "rack", "live", "disabled", "a=1,b=2"} {
		c.StrContains(out, want, "output missing")
	}
	c.NotStrContains(out, fixturePrefix, "the shared timestamp prefix must not be displayed:\n")
	c.NotStrContains(out, "\x1b", "useColor=false must emit no ANSI escapes:\n")
	for _, header := range []string{"ID", "MACHINE", "STATUS", "LABELS", "ADMITS", "CONNECTED", "LAST SEEN"} {
		c.StrContains(out, header, "missing header")
	}
}

// Connected is a live-pool view field distinct from Enabled/last_seen_ms — a
// client wants to know how long the CURRENT connection has held, separate
// from whether the row is enabled or when it was last seen at all.
func TestRenderExecutorTableShowsConnectedSince(t *testing.T) {
	c := assert.NewCollecting(t)
	connectedMs := time.Now().Add(-90 * time.Second).UnixMilli()
	execs := []*rafikiv1.ExecutorRow{
		{Id: fixtureTailA, Enabled: true, Connected: true, ConnectedAtMs: connectedMs},
		{Id: fixtureTailB, Enabled: true}, // not connected: connected_at_ms 0
	}
	var buf bytes.Buffer
	c.Require().NoError(renderExecutorTable(&buf, execs, false), "renderExecutorTable")
	out := buf.String()
	c.StrContains(out, "CONNECTED", "output missing the CONNECTED column header:\n")
	c.StrContains(out, "ago", "expected a relative connected-since time for the live executor:\n")
}

func TestRenderExecutorTableEmptyAndLastSeen(t *testing.T) {
	c := assert.NewAborting(t)
	var buf bytes.Buffer
	c.NoError(renderExecutorTable(&buf, nil, false), "renderExecutorTable")
	c.Eq("No enrolled executors.\n", buf.String(), "empty pool renders")

	// last_seen_ms 0 is "never seen": a dash, not a relative time.
	buf.Reset()
	c.NoError(renderExecutorTable(&buf, []*rafikiv1.ExecutorRow{{Id: fixtureTailA, Enabled: true}}, false), "renderExecutorTable")
	c.NotStrContains(buf.String(), "ago", "a zero last-seen must render as '-', not a relative time:\n")

	// A real sighting renders under LAST SEEN.
	buf.Reset()
	seenMs := time.Now().Add(-time.Hour).UnixMilli()
	c.NoError(renderExecutorTable(&buf, []*rafikiv1.ExecutorRow{{Id: fixtureTailB, Enabled: true, LastSeenMs: seenMs}}, false), "renderExecutorTable")
	c.StrContains(buf.String(), "ago", "a sighted row must render a relative last-seen time:\n")
}

// ─── the Connect round trips ─────────────────────────────────────────────────

// executorStubControl serves the executor-admin slice over Connect for the CLI
// tests: it returns the rows each test seeds and records every request the CLI
// sent, so a test can pin the request shape (kind empty, selector and limit
// from flags, ids passed verbatim) as well as the rendering.
type executorStubControl struct {
	rafikiv1connect.UnimplementedControlHandler

	rows       []*rafikiv1.ExecutorRow
	labelRow   *rafikiv1.ExecutorRow
	token      string
	createdID  string
	credential string
	disableErr error

	sawList    *rafikiv1.ListExecutorsRequest
	sawLabel   *rafikiv1.LabelExecutorRequest
	sawEnroll  *rafikiv1.EnrollExecutorRequest
	sawCreate  *rafikiv1.CreateExecutorRequest
	sawDisable []string
	sawEnable  []string
	sawDelete  []string
}

func (s *executorStubControl) ListExecutors(
	_ context.Context,
	req *connect.Request[rafikiv1.ListExecutorsRequest],
) (*connect.Response[rafikiv1.ListExecutorsResponse], error) {
	s.sawList = req.Msg
	return connect.NewResponse(&rafikiv1.ListExecutorsResponse{Rows: s.rows}), nil
}

func (s *executorStubControl) LabelExecutor(
	_ context.Context,
	req *connect.Request[rafikiv1.LabelExecutorRequest],
) (*connect.Response[rafikiv1.LabelExecutorResponse], error) {
	s.sawLabel = req.Msg
	row := s.labelRow
	if row == nil {
		row = &rafikiv1.ExecutorRow{Id: req.Msg.GetExecutorId()}
	}
	return connect.NewResponse(&rafikiv1.LabelExecutorResponse{Executor: row}), nil
}

func (s *executorStubControl) EnrollExecutor(
	_ context.Context,
	req *connect.Request[rafikiv1.EnrollExecutorRequest],
) (*connect.Response[rafikiv1.EnrollExecutorResponse], error) {
	s.sawEnroll = req.Msg
	return connect.NewResponse(&rafikiv1.EnrollExecutorResponse{Token: s.token}), nil
}

func (s *executorStubControl) CreateExecutor(
	_ context.Context,
	req *connect.Request[rafikiv1.CreateExecutorRequest],
) (*connect.Response[rafikiv1.CreateExecutorResponse], error) {
	s.sawCreate = req.Msg
	return connect.NewResponse(&rafikiv1.CreateExecutorResponse{ExecutorId: s.createdID, Credential: s.credential}), nil
}

func (s *executorStubControl) DisableExecutor(
	_ context.Context,
	req *connect.Request[rafikiv1.DisableExecutorRequest],
) (*connect.Response[rafikiv1.DisableExecutorResponse], error) {
	s.sawDisable = append(s.sawDisable, req.Msg.GetExecutorId())
	if s.disableErr != nil {
		return nil, s.disableErr
	}
	return connect.NewResponse(&rafikiv1.DisableExecutorResponse{}), nil
}

func (s *executorStubControl) EnableExecutor(
	_ context.Context,
	req *connect.Request[rafikiv1.EnableExecutorRequest],
) (*connect.Response[rafikiv1.EnableExecutorResponse], error) {
	s.sawEnable = append(s.sawEnable, req.Msg.GetExecutorId())
	return connect.NewResponse(&rafikiv1.EnableExecutorResponse{}), nil
}

func (s *executorStubControl) DeleteExecutor(
	_ context.Context,
	req *connect.Request[rafikiv1.DeleteExecutorRequest],
) (*connect.Response[rafikiv1.DeleteExecutorResponse], error) {
	s.sawDelete = append(s.sawDelete, req.Msg.GetExecutorId())
	return connect.NewResponse(&rafikiv1.DeleteExecutorResponse{}), nil
}

// executorTestDaemon stands a stub Control handler up on a scratch profile's
// own socket — the harness TestHistoryReachesTheSocketProfilesOwnDaemon
// uses — so a CLI verb dials THAT daemon and no other.
func executorTestDaemon(t *testing.T, ctl *executorStubControl) {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	// A short directory, not t.TempDir(): unix socket paths are capped at
	// ~104 bytes (sizeof sun_path on darwin), and t.TempDir() nests under the
	// full test name, which alone can exceed that.
	dir, err := os.MkdirTemp("", "raf-ex")
	c.NoError(err, "MkdirTemp")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	routePath, handler := rafikiv1connect.NewControlHandler(ctl)
	serveConnectOnUnixSocket(t, sock, routePath, handler)

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: sock},
	}}), "Save")
	c.NoError(profile.SavePointer("scratch"), "SavePointer")
}

// runExecutorCLI executes one `rafiki executor …` argv through the real root
// command — the persistent -o/-j/-J flags live there — and returns what the
// verb wrote to stdout.
func runExecutorCLI(t *testing.T, args ...string) string {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(append([]string{"executor"}, args...))
	out := captureStdout(t, func() {
		assert.NewAborting(t).NoError(root.Execute(), "rafiki executor %v", args)
	})
	return out
}

func TestExecutorListOnConnect(t *testing.T) {
	const (
		connectedMs = int64(1700000000000)
		seenMs      = int64(1700000005000)
	)
	fullA := fixturePrefix + fixtureTailA
	fullB := fixturePrefix + fixtureTailB
	srv := &executorStubControl{rows: []*rafikiv1.ExecutorRow{
		{Id: fullA, Machine: "laptop", Enabled: true, Connected: true,
			ConnectedAtMs: connectedMs, LastSeenMs: seenMs,
			Labels: map[string]string{"env": "prod", "machine": "laptop"}},
		{Id: fullB, Machine: "rack",
			Labels: map[string]string{"machine": "rack"}},
	}}
	executorTestDaemon(t, srv)

	t.Run("table keeps the seven columns and renders the proto rows", func(t *testing.T) {
		c := assert.NewCollecting(t)
		out := runExecutorCLI(t, "list")
		for _, want := range []string{
			fixtureTailA, fixtureTailB, // ID column: the tails, not the heads
			"laptop", "rack", // MACHINE
			"live", "disabled", // STATUS
			"env=prod,machine=laptop", // LABELS sorted
		} {
			c.StrContains(out, want, "output missing")
		}
		for _, header := range []string{"ID", "MACHINE", "STATUS", "LABELS", "ADMITS", "CONNECTED", "LAST SEEN"} {
			c.StrContains(out, header, "missing header")
		}
		c.StrContains(out, "ago", "the connected/sighted timestamps must render as relative times:\n")
		c.NotStrContains(out, "\x1b", "plain stdout must carry no ANSI escapes:\n")
	})

	t.Run("json is protojson ExecutorRow rows under the rows envelope", func(t *testing.T) {
		c := assert.NewCollecting(t)
		out := runExecutorCLI(t, "list", "-j")
		c.StrContains(out, "\"rows\"", "json output must use the canonical rows envelope:\n")
		for _, want := range []string{
			"\"id\": \"" + fullA + "\"",
			"\"machine\": \"laptop\"",
			"\"enabled\": true",
			"\"connected\": true",
			"\"connectedAtMs\": \"1700000000000\"", // int64 rides as a JSON string
			"\"lastSeenMs\": \"1700000005000\"",
		} {
			c.StrContains(out, want, "protojson output missing")
		}
		// The framed full-executor shape is retired with its transport.
		for _, gone := range []string{"\"executors\"", "\"connected_at\"", "\"last_seen_at\"", "\"enrolled_at\"", "\"self_reported\""} {
			c.NotStrContains(out, gone, "retired framed key")
		}
	})

	t.Run("jsonl is one compact row per line with no envelope", func(t *testing.T) {
		c := assert.NewCollecting(t)
		out := runExecutorCLI(t, "list", "-J")
		lines := nonEmptyLines(out)
		c.Require().Len(lines, 2, "got %d lines, want one row per line:\n%s", len(lines), out)
		c.NotStrContains(out, "\"rows\"", "jsonl must not wrap rows in an envelope:\n")
		c.False(!strings.Contains(lines[0], "\"id\":\""+fullA+"\"") ||
			!strings.Contains(lines[1], "\"id\":\""+fullB+"\""), "rows must land in daemon order:\n%s", out)
	})

	t.Run("selector and limit ride the request, kind stays empty", func(t *testing.T) {
		c := assert.NewCollecting(t)
		runExecutorCLI(t, "list", "--selector", "env=prod", "--limit", "7")
		c.Require().NotNil(srv.sawList, "the CLI sent no ListExecutors request")
		c.Eq("", srv.sawList.GetKind(), "kind")
		c.Eq("env=prod", srv.sawList.GetSelector(), "selector")
		c.Eq(7, srv.sawList.GetLimit(), "limit")
	})
}

func TestExecutorLabelOnConnect(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := &executorStubControl{labelRow: &rafikiv1.ExecutorRow{
		Id: "exec-full-1", Machine: "laptop", Enabled: true, Admits: "env=prod",
		Labels: map[string]string{"machine": "laptop"},
	}}
	executorTestDaemon(t, srv)

	out := runExecutorCLI(t, "label", "exec-full-1", "env=prod", "--remove", "stale")

	c.Eq("exec-full-1", srv.sawLabel.GetExecutorId(), "executor_id")
	c.Eq("prod", srv.sawLabel.GetSet()["env"], "set[env]")
	c.EqDiff([]string{"stale"}, srv.sawLabel.GetRemove(), "remove")

	// The echo is the response's updated row as canonical protojson.
	for _, want := range []string{"\"id\": \"exec-full-1\"", "\"machine\": \"laptop\"", "\"admits\": \"env=prod\""} {
		c.StrContains(out, want, "label echo missing")
	}
}

func TestExecutorEnrollTokenToStdoutOnly(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := &executorStubControl{token: "tok-secret-1"}
	executorTestDaemon(t, srv)

	readErr := captureStderr(t)
	out := runExecutorCLI(t, "enroll", "--name", "lab", "--ttl", "30m")

	c.Eq("tok-secret-1\n", out, "stdout")
	c.StrContains(readErr(), "Token minted", "the one-time notice must go to stderr, got:\n")
	c.Eq("lab", srv.sawEnroll.GetName(), "name")
	c.Eq(1800, srv.sawEnroll.GetTtlSeconds(), "ttl_seconds")
}

func TestExecutorCreateEchoesProtojson(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := &executorStubControl{createdID: "exec-42", credential: "cred-once"}
	executorTestDaemon(t, srv)

	out := runExecutorCLI(t, "create", "--name", "lab")

	c.Eq("lab", srv.sawCreate.GetName(), "name")
	for _, want := range []string{"\"executorId\": \"exec-42\"", "\"credential\": \"cred-once\""} {
		c.StrContains(out, want, "create echo missing")
	}
}

func TestExecutorDisableEnableOnConnect(t *testing.T) {
	c := assert.NewCollecting(t)
	srv := &executorStubControl{}
	executorTestDaemon(t, srv)

	c.StrContains(runExecutorCLI(t, "disable", "abc123"), "Executor abc123 disabled.", "disable output =")
	c.EqDiff([]string{"abc123"}, srv.sawDisable, "disable sent")
	c.StrContains(runExecutorCLI(t, "enable", "abc123"), "Executor abc123 enabled.", "enable output =")
	c.EqDiff([]string{"abc123"}, srv.sawEnable, "enable sent")
}

func TestExecutorBulkDeleteOnConnect(t *testing.T) {
	fullA := fixturePrefix + fixtureTailA
	fullB := fixturePrefix + fixtureTailB
	fullC := fixturePrefix + "aabbccddeeff"

	t.Run("union of the two criteria, live rows spared", func(t *testing.T) {
		c := assert.NewCollecting(t)
		srv := &executorStubControl{rows: []*rafikiv1.ExecutorRow{
			{Id: fullA, Enabled: true, Connected: true}, // live: neither flag wants it
			{Id: fullB, Enabled: false},                 // disabled
			{Id: fullC, Enabled: true},                  // offline (not connected)
		}}
		executorTestDaemon(t, srv)

		out := runExecutorCLI(t, "delete", "--all-disabled", "--all-offline", "-y")

		// What is about to be deleted is listed first, then deleted.
		c.False(!strings.Contains(out, fixtureTailB) || !strings.Contains(out, "aabbccddeeff"), "the listing must show the rows being deleted:\n%s", out)
		got := srv.sawDelete
		slices.Sort(got)
		want := []string{fullB, fullC}
		c.EqDiff(want, got, "deleted")
	})

	t.Run("no match prints the friendly line and deletes nothing", func(t *testing.T) {
		c := assert.NewCollecting(t)
		srv := &executorStubControl{rows: []*rafikiv1.ExecutorRow{
			{Id: fullA, Enabled: true, Connected: true},
		}}
		executorTestDaemon(t, srv)

		out := runExecutorCLI(t, "delete", "--all-offline", "-y")
		c.StrContains(out, "No executors match.", "output")
		c.Empty(srv.sawDelete, "deletes sent")
	})
}

// nonEmptyLines splits s on newlines and drops the empty tail lines a
// trailing newline leaves.
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// TestExecutorVerbErrRendersReasonAndInfraAdvice pins the shared
// connectVerbErr composition on an executor verb: a daemon refusal carrying a
// rafiki reason renders `<reason>: <message>`, while an infrastructure
// failure (Unavailable) renders diagnoseConnectError's advice instead of the
// bare connect transport string.
func TestExecutorVerbErrRendersReasonAndInfraAdvice(t *testing.T) {
	denied := func() *executorStubControl {
		srv := &executorStubControl{}
		srv.disableErr = rpcreason.Attach(
			connect.NewError(connect.CodePermissionDenied, errors.New("nope")),
			"executor_disabled")
		return srv
	}

	t.Run("reason-carrying refusal renders reason and message", func(t *testing.T) {
		c := assert.NewCollecting(t)
		executorTestDaemon(t, denied())
		root := newRootCmd()
		root.SetArgs([]string{"executor", "disable", "abc123"})
		var errOut bytes.Buffer
		root.SetErr(&errOut)
		err := root.Execute()
		c.Require().Error(err, "disable against a refusing daemon succeeded")
		c.StrContains(err.Error(), "executor_disabled: nope", "error")
	})

	t.Run("infra failure renders the advice", func(t *testing.T) {
		c := assert.NewCollecting(t)
		srv := &executorStubControl{}
		srv.disableErr = connect.NewError(connect.CodeUnavailable, errors.New("socket gone"))
		executorTestDaemon(t, srv)

		root := newRootCmd()
		root.SetArgs([]string{"executor", "disable", "abc123"})
		var errOut bytes.Buffer
		root.SetErr(&errOut)
		err := root.Execute()
		c.Require().Error(err, "disable against a down daemon succeeded")
		c.StrContains(err.Error(), "is rafikid running?", "error")
	})
}
