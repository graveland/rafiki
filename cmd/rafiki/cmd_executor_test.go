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
	full := fixturePrefix + fixtureTailA
	if got := shortExecutorID(full); got != fixtureTailA {
		t.Fatalf("shortExecutorID(%s) = %s, want the tail %s", full, got, fixtureTailA)
	}
	if got := shortExecutorID(fixtureTailA); got != fixtureTailA {
		t.Fatalf("short ids must pass through unchanged, got %s", got)
	}
	if got := shortExecutorID("exactly12ch"); got != "exactly12ch" {
		t.Fatalf("ids of exactly %d chars must not be mangled, got %q", executorShortIDLen, got)
	}
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
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("all-offline selects Connected==false regardless of enabled", func(t *testing.T) {
		got := ids(filterExecutorsForDelete(execs, false, true))
		want := []string{"enabled-offline", "disabled-offline"}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("both flags union rather than intersect", func(t *testing.T) {
		got := ids(filterExecutorsForDelete(execs, true, true))
		want := []string{"disabled-online", "enabled-offline", "disabled-offline"}
		if !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("neither flag selects nothing", func(t *testing.T) {
		got := filterExecutorsForDelete(execs, false, false)
		if len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})
}

func TestRenderExecutorTableShowsTailIDs(t *testing.T) {
	execs := []*rafikiv1.ExecutorRow{
		{Id: fixturePrefix + fixtureTailA,
			Enabled: true, Connected: true,
			Labels: map[string]string{"b": "2", "a": "1", "machine": "laptop"}},
		{Id: fixturePrefix + fixtureTailB,
			Labels: map[string]string{"env": "work", "machine": "rack"}},
	}

	var buf bytes.Buffer
	if err := renderExecutorTable(&buf, execs, false); err != nil {
		t.Fatalf("renderExecutorTable: %v", err)
	}
	out := buf.String()

	for _, want := range []string{fixtureTailA, fixtureTailB, "laptop", "rack", "live", "disabled", "a=1,b=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, fixturePrefix) {
		t.Errorf("the shared timestamp prefix must not be displayed:\n%s", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("useColor=false must emit no ANSI escapes:\n%s", out)
	}
	for _, header := range []string{"ID", "MACHINE", "STATUS", "LABELS", "ADMITS", "CONNECTED", "LAST SEEN"} {
		if !strings.Contains(out, header) {
			t.Errorf("missing header %q:\n%s", header, out)
		}
	}
}

// Connected is a live-pool view field distinct from Enabled/last_seen_ms — a
// client wants to know how long the CURRENT connection has held, separate
// from whether the row is enabled or when it was last seen at all.
func TestRenderExecutorTableShowsConnectedSince(t *testing.T) {
	connectedMs := time.Now().Add(-90 * time.Second).UnixMilli()
	execs := []*rafikiv1.ExecutorRow{
		{Id: fixtureTailA, Enabled: true, Connected: true, ConnectedAtMs: connectedMs},
		{Id: fixtureTailB, Enabled: true}, // not connected: connected_at_ms 0
	}
	var buf bytes.Buffer
	if err := renderExecutorTable(&buf, execs, false); err != nil {
		t.Fatalf("renderExecutorTable: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "CONNECTED") {
		t.Errorf("output missing the CONNECTED column header:\n%s", out)
	}
	if !strings.Contains(out, "ago") {
		t.Errorf("expected a relative connected-since time for the live executor:\n%s", out)
	}
}

func TestRenderExecutorTableEmptyAndLastSeen(t *testing.T) {
	var buf bytes.Buffer
	if err := renderExecutorTable(&buf, nil, false); err != nil {
		t.Fatalf("renderExecutorTable: %v", err)
	}
	if got := buf.String(); got != "No enrolled executors.\n" {
		t.Fatalf("empty pool renders %q, want the friendly line", got)
	}

	// last_seen_ms 0 is "never seen": a dash, not a relative time.
	buf.Reset()
	if err := renderExecutorTable(&buf, []*rafikiv1.ExecutorRow{{Id: fixtureTailA, Enabled: true}}, false); err != nil {
		t.Fatalf("renderExecutorTable: %v", err)
	}
	if strings.Contains(buf.String(), "ago") {
		t.Fatalf("a zero last-seen must render as '-', not a relative time:\n%s", buf.String())
	}

	// A real sighting renders under LAST SEEN.
	buf.Reset()
	seenMs := time.Now().Add(-time.Hour).UnixMilli()
	if err := renderExecutorTable(&buf, []*rafikiv1.ExecutorRow{{Id: fixtureTailB, Enabled: true, LastSeenMs: seenMs}}, false); err != nil {
		t.Fatalf("renderExecutorTable: %v", err)
	}
	if !strings.Contains(buf.String(), "ago") {
		t.Fatalf("a sighted row must render a relative last-seen time:\n%s", buf.String())
	}
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
	isolateProfiles(t)
	resetProfileCache()

	// A short directory, not t.TempDir(): unix socket paths are capped at
	// ~104 bytes (sizeof sun_path on darwin), and t.TempDir() nests under the
	// full test name, which alone can exceed that.
	dir, err := os.MkdirTemp("", "raf-ex")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	routePath, handler := rafikiv1connect.NewControlHandler(ctl)
	serveConnectOnUnixSocket(t, sock, routePath, handler)

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"scratch": {Name: "scratch", Socket: sock},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("scratch"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
}

// runExecutorCLI executes one `rafiki executor …` argv through the real root
// command — the persistent -o/-j/-J flags live there — and returns what the
// verb wrote to stdout.
func runExecutorCLI(t *testing.T, args ...string) string {
	t.Helper()
	root := newRootCmd()
	root.SetArgs(append([]string{"executor"}, args...))
	out := captureStdout(t, func() {
		if err := root.Execute(); err != nil {
			t.Fatalf("rafiki executor %v: %v", args, err)
		}
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
		out := runExecutorCLI(t, "list")
		for _, want := range []string{
			fixtureTailA, fixtureTailB, // ID column: the tails, not the heads
			"laptop", "rack", // MACHINE
			"live", "disabled", // STATUS
			"env=prod,machine=laptop", // LABELS sorted
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
		for _, header := range []string{"ID", "MACHINE", "STATUS", "LABELS", "ADMITS", "CONNECTED", "LAST SEEN"} {
			if !strings.Contains(out, header) {
				t.Errorf("missing header %q:\n%s", header, out)
			}
		}
		if !strings.Contains(out, "ago") {
			t.Errorf("the connected/sighted timestamps must render as relative times:\n%s", out)
		}
		if strings.Contains(out, "\x1b") {
			t.Errorf("plain stdout must carry no ANSI escapes:\n%s", out)
		}
	})

	t.Run("json is protojson ExecutorRow rows under the rows envelope", func(t *testing.T) {
		out := runExecutorCLI(t, "list", "-j")
		if !strings.Contains(out, "\"rows\"") {
			t.Errorf("json output must use the canonical rows envelope:\n%s", out)
		}
		for _, want := range []string{
			"\"id\": \"" + fullA + "\"",
			"\"machine\": \"laptop\"",
			"\"enabled\": true",
			"\"connected\": true",
			"\"connectedAtMs\": \"1700000000000\"", // int64 rides as a JSON string
			"\"lastSeenMs\": \"1700000005000\"",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("protojson output missing %q:\n%s", want, out)
			}
		}
		// The framed full-executor shape is retired with its transport.
		for _, gone := range []string{"\"executors\"", "\"connected_at\"", "\"last_seen_at\"", "\"enrolled_at\"", "\"self_reported\""} {
			if strings.Contains(out, gone) {
				t.Errorf("retired framed key %q must not appear:\n%s", gone, out)
			}
		}
	})

	t.Run("jsonl is one compact row per line with no envelope", func(t *testing.T) {
		out := runExecutorCLI(t, "list", "-J")
		lines := nonEmptyLines(out)
		if len(lines) != 2 {
			t.Fatalf("got %d lines, want one row per line:\n%s", len(lines), out)
		}
		if strings.Contains(out, "\"rows\"") {
			t.Errorf("jsonl must not wrap rows in an envelope:\n%s", out)
		}
		if !strings.Contains(lines[0], "\"id\":\""+fullA+"\"") ||
			!strings.Contains(lines[1], "\"id\":\""+fullB+"\"") {
			t.Errorf("rows must land in daemon order:\n%s", out)
		}
	})

	t.Run("selector and limit ride the request, kind stays empty", func(t *testing.T) {
		runExecutorCLI(t, "list", "--selector", "env=prod", "--limit", "7")
		if srv.sawList == nil {
			t.Fatal("the CLI sent no ListExecutors request")
		}
		if got := srv.sawList.GetKind(); got != "" {
			t.Errorf("kind = %q, want empty: the plain listing is the empty-kind path", got)
		}
		if got := srv.sawList.GetSelector(); got != "env=prod" {
			t.Errorf("selector = %q, want env=prod", got)
		}
		if got := srv.sawList.GetLimit(); got != 7 {
			t.Errorf("limit = %d, want 7", got)
		}
	})
}

func TestExecutorLabelOnConnect(t *testing.T) {
	srv := &executorStubControl{labelRow: &rafikiv1.ExecutorRow{
		Id: "exec-full-1", Machine: "laptop", Enabled: true, Admits: "env=prod",
		Labels: map[string]string{"machine": "laptop"},
	}}
	executorTestDaemon(t, srv)

	out := runExecutorCLI(t, "label", "exec-full-1", "env=prod", "--remove", "stale")

	if got := srv.sawLabel.GetExecutorId(); got != "exec-full-1" {
		t.Errorf("executor_id = %q, want exec-full-1", got)
	}
	if got := srv.sawLabel.GetSet()["env"]; got != "prod" {
		t.Errorf("set[env] = %q, want prod", got)
	}
	if got := srv.sawLabel.GetRemove(); !slices.Equal(got, []string{"stale"}) {
		t.Errorf("remove = %v, want [stale]", got)
	}

	// The echo is the response's updated row as canonical protojson.
	for _, want := range []string{"\"id\": \"exec-full-1\"", "\"machine\": \"laptop\"", "\"admits\": \"env=prod\""} {
		if !strings.Contains(out, want) {
			t.Errorf("label echo missing %q:\n%s", want, out)
		}
	}
}

func TestExecutorEnrollTokenToStdoutOnly(t *testing.T) {
	srv := &executorStubControl{token: "tok-secret-1"}
	executorTestDaemon(t, srv)

	readErr := captureStderr(t)
	out := runExecutorCLI(t, "enroll", "--name", "lab", "--ttl", "30m")

	if out != "tok-secret-1\n" {
		t.Errorf("stdout = %q, want exactly the token (it pipes)", out)
	}
	if got := readErr(); !strings.Contains(got, "Token minted") {
		t.Errorf("the one-time notice must go to stderr, got:\n%s", got)
	}
	if got := srv.sawEnroll.GetName(); got != "lab" {
		t.Errorf("name = %q, want lab", got)
	}
	if got := srv.sawEnroll.GetTtlSeconds(); got != 1800 {
		t.Errorf("ttl_seconds = %d, want 1800", got)
	}
}

func TestExecutorCreateEchoesProtojson(t *testing.T) {
	srv := &executorStubControl{createdID: "exec-42", credential: "cred-once"}
	executorTestDaemon(t, srv)

	out := runExecutorCLI(t, "create", "--name", "lab")

	if got := srv.sawCreate.GetName(); got != "lab" {
		t.Errorf("name = %q, want lab", got)
	}
	for _, want := range []string{"\"executorId\": \"exec-42\"", "\"credential\": \"cred-once\""} {
		if !strings.Contains(out, want) {
			t.Errorf("create echo missing %q:\n%s", want, out)
		}
	}
}

func TestExecutorDisableEnableOnConnect(t *testing.T) {
	srv := &executorStubControl{}
	executorTestDaemon(t, srv)

	if out := runExecutorCLI(t, "disable", "abc123"); !strings.Contains(out, "Executor abc123 disabled.") {
		t.Errorf("disable output = %q", out)
	}
	if got := srv.sawDisable; !slices.Equal(got, []string{"abc123"}) {
		t.Errorf("disable sent %v, want [abc123]", got)
	}
	if out := runExecutorCLI(t, "enable", "abc123"); !strings.Contains(out, "Executor abc123 enabled.") {
		t.Errorf("enable output = %q", out)
	}
	if got := srv.sawEnable; !slices.Equal(got, []string{"abc123"}) {
		t.Errorf("enable sent %v, want [abc123]", got)
	}
}

func TestExecutorBulkDeleteOnConnect(t *testing.T) {
	fullA := fixturePrefix + fixtureTailA
	fullB := fixturePrefix + fixtureTailB
	fullC := fixturePrefix + "aabbccddeeff"

	t.Run("union of the two criteria, live rows spared", func(t *testing.T) {
		srv := &executorStubControl{rows: []*rafikiv1.ExecutorRow{
			{Id: fullA, Enabled: true, Connected: true}, // live: neither flag wants it
			{Id: fullB, Enabled: false},                 // disabled
			{Id: fullC, Enabled: true},                  // offline (not connected)
		}}
		executorTestDaemon(t, srv)

		out := runExecutorCLI(t, "delete", "--all-disabled", "--all-offline", "-y")

		// What is about to be deleted is listed first, then deleted.
		if !strings.Contains(out, fixtureTailB) || !strings.Contains(out, "aabbccddeeff") {
			t.Errorf("the listing must show the rows being deleted:\n%s", out)
		}
		got := srv.sawDelete
		slices.Sort(got)
		want := []string{fullB, fullC}
		if !slices.Equal(got, want) {
			t.Errorf("deleted %v, want %v", got, want)
		}
	})

	t.Run("no match prints the friendly line and deletes nothing", func(t *testing.T) {
		srv := &executorStubControl{rows: []*rafikiv1.ExecutorRow{
			{Id: fullA, Enabled: true, Connected: true},
		}}
		executorTestDaemon(t, srv)

		out := runExecutorCLI(t, "delete", "--all-offline", "-y")
		if !strings.Contains(out, "No executors match.") {
			t.Errorf("output = %q, want the no-match line", out)
		}
		if len(srv.sawDelete) != 0 {
			t.Errorf("deletes sent: %v, want none", srv.sawDelete)
		}
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
		executorTestDaemon(t, denied())
		root := newRootCmd()
		root.SetArgs([]string{"executor", "disable", "abc123"})
		var errOut bytes.Buffer
		root.SetErr(&errOut)
		err := root.Execute()
		if err == nil {
			t.Fatal("disable against a refusing daemon succeeded")
		}
		if got := err.Error(); !strings.Contains(got, "executor_disabled: nope") {
			t.Errorf("error = %q, want `<reason>: <message>` shape", got)
		}
	})

	t.Run("infra failure renders the advice", func(t *testing.T) {
		srv := &executorStubControl{}
		srv.disableErr = connect.NewError(connect.CodeUnavailable, errors.New("socket gone"))
		executorTestDaemon(t, srv)

		root := newRootCmd()
		root.SetArgs([]string{"executor", "disable", "abc123"})
		var errOut bytes.Buffer
		root.SetErr(&errOut)
		err := root.Execute()
		if err == nil {
			t.Fatal("disable against a down daemon succeeded")
		}
		if got := err.Error(); !strings.Contains(got, "is rafikid running?") {
			t.Errorf("error = %q, want diagnoseConnectError's advice", got)
		}
	})
}
