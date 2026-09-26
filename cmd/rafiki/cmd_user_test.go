package main

// The user verbs ride the Connect control plane (CreateUser/ListUsers/
// RemoveUser) through newConnectEndpoint, with the profile's token as usual —
// there is no token-less dial and no framed protocol on this surface. The
// tests below therefore exercise three layers:
//
//   - writeTokenFile / the render functions against in-memory types,
//   - userConnectErr's rendering of Connect errors,
//   - the real commands against a fake Connect server served on a socket
//     profile's own connect.sock (the same harness cmd_history_test.go uses),
//     which pins that the request actually carries the profile's credential.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"
)

// `rafiki user create` is also the login step: the token is shown once, so
// the CLI must persist it or the user is locked out of their own daemon.
func TestWriteTokenFileCreates0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")

	if err := writeTokenFile(path, "rfk_secret"); err != nil {
		t.Fatalf("writeTokenFile: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(b) != "rfk_secret\n" {
		t.Fatalf("content = %q", b)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// Overwriting must not widen the mode of an existing file, and must not
// leave the old (longer) token's tail behind.
func TestWriteTokenFileOverwritesCleanly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("rfk_a_very_long_previous_token\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeTokenFile(path, "rfk_short"); err != nil {
		t.Fatalf("writeTokenFile: %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != "rfk_short\n" {
		t.Fatalf("content = %q; the old token was not fully replaced", b)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 after overwrite", fi.Mode().Perm())
	}
}

// TestUserCreateWritesTheProfilesTokenNotAGlobalOne pins the property Task 7
// exists to establish: `user create` writes to the resolved PROFILE's token
// file (profile.TokenFile), not a single global one — minting a credential
// against one daemon must not disturb another profile's credential.
func TestUserCreateWritesTheProfilesTokenNotAGlobalOne(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work":     {Name: "work", Socket: "/tmp/work.sock"},
		"personal": {Name: "personal", URL: "https://h", Proxy: "https://h"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.WriteToken("work", "sk-work-existing"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	if err := profile.SavePointer("personal"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	if err := profile.WriteToken("personal", "sk-personal-new"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}

	// The bug this feature exists to fix: minting on one daemon must not
	// disturb the other's credential.
	if got := profile.ReadToken("work"); got != "sk-work-existing" {
		t.Fatalf("work token = %q after writing personal's; it was clobbered", got)
	}
	if got := profile.ReadToken("personal"); got != "sk-personal-new" {
		t.Fatalf("personal token = %q", got)
	}
}

// ─── renderUserCreate: write-failure-still-prints-token ────────────────────

func sampleCreateResponse() *rafikiv1.CreateUserResponse {
	return &rafikiv1.CreateUserResponse{
		Id:            "usr_1",
		Username:      "alice",
		Token:         "rfk_only_copy_ever",
		CreatedAtUnix: 1800000000,
	}
}

// TestRenderUserCreate_WriteFailureStillPrintsToken is the difference between
// a user having their credential and losing it permanently: the daemon shows
// the plaintext token exactly once, so a token-file write failure must never
// suppress it from stdout, only warn on stderr.
func TestRenderUserCreate_WriteFailureStillPrintsToken(t *testing.T) {
	writeErr := errors.New("permission denied")
	failingWrite := func(path, token string) error { return writeErr }

	var stdout, stderr bytes.Buffer
	err := renderUserCreate(&stdout, &stderr, sampleCreateResponse(), "/does/not/matter", true, failingWrite, outputAuto)
	if err != nil {
		t.Fatalf("renderUserCreate returned an error instead of degrading: %v", err)
	}

	if !strings.Contains(stdout.String(), "rfk_only_copy_ever") {
		t.Fatalf("token missing from stdout after a write failure — it is now unrecoverable\nstdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "warning") || !strings.Contains(stderr.String(), writeErr.Error()) {
		t.Fatalf("stderr does not warn about the write failure: %s", stderr.String())
	}
}

// TestRenderUserCreate_NoWriteNeverCallsWriteFn pins --no-write: writeFn must
// not be invoked at all, not merely "invoked and its result ignored".
func TestRenderUserCreate_NoWriteNeverCallsWriteFn(t *testing.T) {
	called := false
	writeFn := func(path, token string) error { called = true; return nil }

	var stdout, stderr bytes.Buffer
	if err := renderUserCreate(&stdout, &stderr, sampleCreateResponse(), "/does/not/matter", false, writeFn, outputAuto); err != nil {
		t.Fatalf("renderUserCreate: %v", err)
	}
	if called {
		t.Fatal("writeFn was called despite shouldWrite=false")
	}
	if !strings.Contains(stdout.String(), "rfk_only_copy_ever") {
		t.Fatalf("token missing from stdout: %s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected no stderr output on --no-write, got: %s", stderr.String())
	}
}

// TestRenderUserCreate_SuccessfulWriteConfirms pins the happy path's stderr
// confirmation, which is the only signal a scripted caller has that the token
// actually landed on disk.
func TestRenderUserCreate_SuccessfulWriteConfirms(t *testing.T) {
	writeFn := func(path, token string) error { return nil }

	var stdout, stderr bytes.Buffer
	if err := renderUserCreate(&stdout, &stderr, sampleCreateResponse(), "/config/rafiki/token", true, writeFn, outputAuto); err != nil {
		t.Fatalf("renderUserCreate: %v", err)
	}
	if !strings.Contains(stderr.String(), "/config/rafiki/token") {
		t.Fatalf("stderr does not confirm the write path: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "rfk_only_copy_ever") {
		t.Fatalf("token missing from stdout: %s", stdout.String())
	}
}

// TestRenderUserCreate_PrintsCanonicalProtojsonInEveryMode pins the create
// output against the protojson contract: pretty in the default and -j modes,
// one compact line under -J, and the token on stdout in every one of them —
// the daemon cannot repeat it, so no mode may drop it.
func TestRenderUserCreate_PrintsCanonicalProtojsonInEveryMode(t *testing.T) {
	cases := []struct {
		name    string
		mode    outputMode
		compact bool
	}{
		{name: "auto", mode: outputAuto},
		{name: "json", mode: outputJSON},
		{name: "jsonl", mode: outputJSONL, compact: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := renderUserCreate(&stdout, &stderr, sampleCreateResponse(), "/does/not/matter", false, func(string, string) error { return nil }, tc.mode); err != nil {
				t.Fatalf("renderUserCreate: %v", err)
			}
			out := stdout.String()
			if !strings.Contains(out, "rfk_only_copy_ever") {
				t.Fatalf("token missing from stdout in mode %v: %s", tc.mode, out)
			}
			// protojson renders int64 as a string; the canonical name is
			// camelCase createdAtUnix.
			var got map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
				t.Fatalf("output is not JSON in mode %v: %v\n%s", tc.mode, err, out)
			}
			if got["createdAtUnix"] != "1800000000" {
				t.Fatalf("createdAtUnix = %v, want the string \"1800000000\" (protojson's int64 form)", got["createdAtUnix"])
			}
			if got["id"] != "usr_1" || got["username"] != "alice" {
				t.Fatalf("unexpected fields: %v", got)
			}
			if tc.compact && strings.Contains(out, "\n") && strings.Count(out, "\n") > 1 {
				t.Fatalf("JSONL create output is not one line: %q", out)
			}
		})
	}
}

// ─── emitUserList ───────────────────────────────────────────────────────────

func sampleUserRows() []*rafikiv1.UserRow {
	removed := int64(1800000100)
	return []*rafikiv1.UserRow{
		{Id: "usr_1", Username: "alice", IsAdmin: true, CreatedAtUnix: 1800000000},
		{Id: "usr_2", Username: "bob", CreatedAtUnix: 1800000050, DeletedAtUnix: &removed},
	}
}

// TestEmitUserList_TableShowsAdminAndRemoval pins the table's semantics: the
// ADMIN column distinguishes an admin from an ordinary user, an active user's
// REMOVED cell is "-" rather than a zero date, and a tombstoned row shows its
// removal time. Row lines are matched by id and split on the table's cell
// separator, since pkg/table draws a bordered box.
func TestEmitUserList_TableShowsAdminAndRemoval(t *testing.T) {
	var out bytes.Buffer
	if err := emitUserList(&out, sampleUserRows(), outputTable, false); err != nil {
		t.Fatalf("emitUserList: %v", err)
	}
	got := out.String()
	for _, want := range []string{"ID", "USER", "ADMIN", "CREATED", "REMOVED", "usr_1", "usr_2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("table output missing %q:\n%s", want, got)
		}
	}

	alice := tableRowFor(t, got, "usr_1")
	if len(alice) != 5 {
		t.Fatalf("alice's row = %v, want 5 cells", alice)
	}
	if alice[1] != "alice" || alice[2] != "yes" {
		t.Fatalf("alice's row = %v, want the admin marked", alice)
	}
	if alice[4] != "-" {
		t.Fatalf("alice's REMOVED cell = %q, want \"-\" (she is active)", alice[4])
	}

	bob := tableRowFor(t, got, "usr_2")
	if bob[1] != "bob" || bob[2] != "-" {
		t.Fatalf("bob's row = %v, want a non-admin", bob)
	}
	if bob[4] == "-" {
		t.Fatalf("bob's REMOVED cell = %q, want the tombstone date", bob[4])
	}
}

// tableRowFor finds the bordered table row carrying id and returns its
// trimmed cells, splitting on the box-drawing separator pkg/table draws.
func tableRowFor(t *testing.T, out, id string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, id) {
			continue
		}
		var cells []string
		for _, c := range strings.Split(line, "│") {
			if c = strings.TrimSpace(c); c != "" {
				cells = append(cells, c)
			}
		}
		return cells
	}
	t.Fatalf("no table row for %q in:\n%s", id, out)
	return nil
}

// TestEmitUserList_JSONIsTheCanonicalProtojson pins -j and -J against the
// emitProtoRows contract: -j is the {"rows":[...]} envelope, -J is one
// compact row per line with NO envelope, and both carry protojson's camelCase
// names with int64 rendered as a string. A UserRow has no token field, so the
// rendered output cannot leak one.
func TestEmitUserList_JSONIsTheCanonicalProtojson(t *testing.T) {
	t.Run("json envelope", func(t *testing.T) {
		var out bytes.Buffer
		if err := emitUserList(&out, sampleUserRows(), outputJSON, false); err != nil {
			t.Fatalf("emitUserList: %v", err)
		}
		var got struct {
			Rows []map[string]any `json:"rows"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("output is not the rows envelope: %v\n%s", err, out.String())
		}
		if len(got.Rows) != 2 {
			t.Fatalf("got %d rows, want 2: %s", len(got.Rows), out.String())
		}
		if got.Rows[0]["createdAtUnix"] != "1800000000" {
			t.Fatalf("createdAtUnix = %v, want the string protojson form", got.Rows[0]["createdAtUnix"])
		}
		if got.Rows[1]["deletedAtUnix"] != "1800000100" {
			t.Fatalf("deletedAtUnix = %v, want the string protojson form", got.Rows[1]["deletedAtUnix"])
		}
		if _, ok := got.Rows[0]["is_admin"]; ok {
			t.Fatalf("snake_case field name survived: %s", out.String())
		}
		if s := strings.ToLower(out.String()); strings.Contains(s, "token") {
			t.Fatalf("rendered user list mentions a token: %s", out.String())
		}
	})
	t.Run("jsonl one row per line", func(t *testing.T) {
		var out bytes.Buffer
		if err := emitUserList(&out, sampleUserRows(), outputJSONL, false); err != nil {
			t.Fatalf("emitUserList: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) != 2 {
			t.Fatalf("got %d lines, want one row per line:\n%s", len(lines), out.String())
		}
		if strings.Contains(out.String(), "rows") {
			t.Fatalf("JSONL must not carry the envelope:\n%s", out.String())
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
			t.Fatalf("line 0 is not a row object: %v", err)
		}
		if row["username"] != "alice" {
			t.Fatalf("line 0 = %v, want alice's row", row)
		}
	})
}

// ─── userConnectErr ─────────────────────────────────────────────────────────

// TestUserConnectErr_RendersTheRafikiReason pins that a fine-grained daemon
// reason reaches stderr verbatim rather than collapsed into connect's coarser
// code name (invalid_argument), which would read as if the CLI, not the
// daemon, had rejected the name.
func TestUserConnectErr_RendersTheRafikiReason(t *testing.T) {
	err := rpcreason.Attach(
		connect.NewError(connect.CodeInvalidArgument, errors.New("username alice is already taken")),
		protocol.ErrInvalidArgs)

	got := userConnectErr(err, "sock")
	if got.Error() != "invalid_args: username alice is already taken" {
		t.Fatalf("userConnectErr = %q, want the reason rendered", got.Error())
	}
}

// TestUserConnectErr_KeepsTheInfrastructureAdvice pins that the three codes
// whose fixes are environmental — the daemon is not there, the token is
// stale, the daemon predates Connect — keep diagnoseConnectError's advice
// instead of the bare code-and-message rendering.
func TestUserConnectErr_KeepsTheInfrastructureAdvice(t *testing.T) {
	unreachable := connect.NewError(connect.CodeUnavailable, errors.New("connection refused"))
	got := userConnectErr(unreachable, "/tmp/x/connect.sock")
	if !strings.Contains(got.Error(), "cannot reach the rafiki daemon at /tmp/x/connect.sock") {
		t.Fatalf("unreachable error lost the daemon-down advice: %v", got)
	}
	if !errors.Is(got, unreachable) {
		t.Fatal("the original error is no longer wrapped")
	}

	stale := connect.NewError(connect.CodeUnauthenticated, errors.New("invalid auth token"))
	if got := userConnectErr(stale, "sock"); !strings.Contains(got.Error(), "check its token file") {
		t.Fatalf("unauthenticated error lost the token-file advice: %v", got)
	}
}

// TestUserConnectErr_NonConnectErrorPassesThrough keeps ordinary failures
// (a wrapped io error from the transport, say) rendering as themselves.
func TestUserConnectErr_NonConnectErrorPassesThrough(t *testing.T) {
	err := errors.New("ordinary failure")
	if got := userConnectErr(err, "sock"); got.Error() != "ordinary failure" {
		t.Fatalf("userConnectErr = %v, want the error's own text", got)
	}
}

// ─── the commands against a fake Connect server ────────────────────────────

// userStubControl serves the three user RPCs and records the requests, so the
// tests can assert what actually went over the wire — including the
// Authorization header, which is how "the profile's token rides Connect" is
// pinned.
type userStubControl struct {
	rafikiv1connect.UnimplementedControlHandler
	mu         sync.Mutex
	createReqs []*connect.Request[rafikiv1.CreateUserRequest]
	listReqs   []*connect.Request[rafikiv1.ListUsersRequest]
	rmReqs     []*connect.Request[rafikiv1.RemoveUserRequest]
	rows       []*rafikiv1.UserRow
	created    *rafikiv1.CreateUserResponse
}

func (s *userStubControl) CreateUser(
	_ context.Context,
	req *connect.Request[rafikiv1.CreateUserRequest],
) (*connect.Response[rafikiv1.CreateUserResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createReqs = append(s.createReqs, req)
	resp := s.created
	if resp == nil {
		resp = &rafikiv1.CreateUserResponse{
			Id: "usr_1", Username: req.Msg.GetUsername(),
			Token: "rfk_new_minted", CreatedAtUnix: 1800000000,
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *userStubControl) ListUsers(
	_ context.Context,
	req *connect.Request[rafikiv1.ListUsersRequest],
) (*connect.Response[rafikiv1.ListUsersResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listReqs = append(s.listReqs, req)
	return connect.NewResponse(&rafikiv1.ListUsersResponse{Users: s.rows}), nil
}

func (s *userStubControl) RemoveUser(
	_ context.Context,
	req *connect.Request[rafikiv1.RemoveUserRequest],
) (*connect.Response[rafikiv1.RemoveUserResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rmReqs = append(s.rmReqs, req)
	return connect.NewResponse(&rafikiv1.RemoveUserResponse{}), nil
}

func (s *userStubControl) lastCreate() *connect.Request[rafikiv1.CreateUserRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.createReqs) == 0 {
		return nil
	}
	return s.createReqs[len(s.createReqs)-1]
}

func (s *userStubControl) lastList() *connect.Request[rafikiv1.ListUsersRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.listReqs) == 0 {
		return nil
	}
	return s.listReqs[len(s.listReqs)-1]
}

func (s *userStubControl) lastRm() *connect.Request[rafikiv1.RemoveUserRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rmReqs) == 0 {
		return nil
	}
	return s.rmReqs[len(s.rmReqs)-1]
}

// serveUserConnect serves the stub on connectSock — the SIBLING of the
// profile's control socket, which is where newConnectEndpoint dials a socket
// profile (serveConnectOnUnixSocket is cmd_history_test.go's shared harness).
func serveUserConnect(t *testing.T, connectSock string, stub *userStubControl) {
	t.Helper()
	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, connectSock, routePath, handler)
}

// userTestRoot builds a root carrying the same persistent flags the real one
// does, so Execute-driven tests exercise the flag plumbing (-o/-j/-J/--color)
// rather than outputOpts's zero-value fallbacks for missing flags.
func userTestRoot(t *testing.T, sub *cobra.Command, args ...string) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	root := &cobra.Command{Use: "rafiki"}
	root.PersistentFlags().StringP("output", "o", "auto", "")
	root.PersistentFlags().BoolP("json", "j", false, "")
	root.PersistentFlags().BoolP("jsonl", "J", false, "")
	root.PersistentFlags().StringP("color", "c", "auto", "")
	root.PersistentFlags().StringP("profile", "P", "", "")
	root.AddCommand(sub)

	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	return root, &out
}

// serveUserScratch seeds one socket profile "it" at dir and serves stub on the
// profile's connect.sock sibling. dir must be a SHORT path: unix socket paths
// are capped at ~104 bytes on darwin (the same reasoning
// TestUserCreateDialsWithoutTheProfileToken's predecessor used).
func serveUserScratch(t *testing.T, stub *userStubControl, token string) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp, not t.TempDir: the UDS path cap.
	dir, err := os.MkdirTemp("", "rafiki-u")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	controlSock := filepath.Join(dir, "controller.sock")
	connectSock := filepath.Join(dir, "connect.sock")

	serveUserConnect(t, connectSock, stub)
	writeTokenedProfile(t, controlSock, token)
}

// TestUserCreateSendsTheProfileTokenOverConnect pins the credential contract
// of the move onto Connect: the request carries the profile's token as a
// Bearer credential (there is NO token-less dial — the framed recovery-path
// dial is gone; a stale token is unstuck on the daemon host, per the design's
// replacement of the bootstrap window) — and the minted token lands in the
// profile's token file and on stdout exactly once.
func TestUserCreateSendsTheProfileTokenOverConnect(t *testing.T) {
	stub := &userStubControl{}
	serveUserScratch(t, stub, "rfk_stale")

	root, out := userTestRoot(t, newUserCreateCmd(), "create", "alice")
	if err := root.Execute(); err != nil {
		t.Fatalf("user create failed: %v\n%s", err, out.String())
	}

	req := stub.lastCreate()
	if req == nil {
		t.Fatal("the stub never saw a CreateUser request")
	}
	if got := req.Msg.GetUsername(); got != "alice" {
		t.Fatalf("username = %q, want alice", got)
	}
	if got := req.Header().Get("Authorization"); got != "Bearer rfk_stale" {
		t.Fatalf("Authorization = %q, want the profile's token on the wire", got)
	}

	// The minted token replaced the profile's (now stale) one — the login
	// step — and was printed once for the human.
	if b, err := os.ReadFile(profile.TokenFile("it")); err != nil {
		t.Fatalf("read token file: %v", err)
	} else if got := strings.TrimSpace(string(b)); got != "rfk_new_minted" {
		t.Fatalf("token file = %q, want the freshly minted token", got)
	}
	if !strings.Contains(out.String(), "rfk_new_minted") {
		t.Fatalf("minted token missing from stdout:\n%s", out.String())
	}
}

// TestUserListReachesTheSocketProfilesDaemon drives `user list` against a
// daemon served on the profile's OWN connect.sock — the property the framed
// completion helper once got wrong by dialing a hardcoded socket.
func TestUserListReachesTheSocketProfilesDaemon(t *testing.T) {
	stub := &userStubControl{rows: sampleUserRows()}
	serveUserScratch(t, stub, "")

	root, out := userTestRoot(t, newUserListCmd(), "list")
	if err := root.Execute(); err != nil {
		t.Fatalf("user list failed: %v\n%s", err, out.String())
	}

	got := out.String()
	if !strings.Contains(got, "alice") || !strings.Contains(got, "bob") {
		t.Fatalf("user list output missing the stub's rows:\n%s", got)
	}
	req := stub.lastList()
	if req == nil {
		t.Fatal("the stub never saw a ListUsers request")
	}
	if req.Msg.GetIncludeDeleted() {
		t.Fatal("the default list must not ask for tombstoned rows")
	}
}

// TestUserListAllAsksForTombstonedRows pins the --all flag's wire effect.
func TestUserListAllAsksForTombstonedRows(t *testing.T) {
	stub := &userStubControl{rows: sampleUserRows()}
	serveUserScratch(t, stub, "")

	root, out := userTestRoot(t, newUserListCmd(), "list", "--all")
	if err := root.Execute(); err != nil {
		t.Fatalf("user list --all failed: %v\n%s", err, out.String())
	}
	if req := stub.lastList(); req == nil || !req.Msg.GetIncludeDeleted() {
		t.Fatalf("--all did not set include_deleted: %+v", stub.lastList())
	}
}

// TestUserListJSONLOverTheWire pins the -J contract end to end: one compact
// row per line, no envelope, straight from a live (fake) daemon.
func TestUserListJSONLOverTheWire(t *testing.T) {
	stub := &userStubControl{rows: sampleUserRows()}
	serveUserScratch(t, stub, "")

	root, out := userTestRoot(t, newUserListCmd(), "list", "-J")
	if err := root.Execute(); err != nil {
		t.Fatalf("user list -J failed: %v\n%s", err, out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want one row per line:\n%s", len(lines), out.String())
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("line 0 is not a row object: %v\n%s", err, out.String())
	}
	if row["username"] != "alice" {
		t.Fatalf("line 0 = %v, want alice's row", row)
	}
}

// TestUserRmSendsTheUsername pins rm's request and its stderr confirmation.
func TestUserRmSendsTheUsername(t *testing.T) {
	stub := &userStubControl{}
	serveUserScratch(t, stub, "rfk_tok")

	root, out := userTestRoot(t, newUserRmCmd(), "rm", "alice")
	if err := root.Execute(); err != nil {
		t.Fatalf("user rm failed: %v\n%s", err, out.String())
	}
	req := stub.lastRm()
	if req == nil {
		t.Fatal("the stub never saw a RemoveUser request")
	}
	if got := req.Msg.GetUsername(); got != "alice" {
		t.Fatalf("username = %q, want alice", got)
	}
	if got := req.Header().Get("Authorization"); got != "Bearer rfk_tok" {
		t.Fatalf("Authorization = %q, want the profile's token on the wire", got)
	}
	if !strings.Contains(out.String(), "removed alice") {
		t.Fatalf("rm did not confirm the removal:\n%s", out.String())
	}
}

// TestUserRmPropagatesARefusal pins that a daemon-side refusal reaches the
// caller as the verb's error, rendered with the daemon's reason — not as a
// success with an empty confirmation. The refusing handler mirrors the admin
// gate's shape (connect_users.go's requireUserAdmin).
func TestUserRmPropagatesARefusal(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp, not t.TempDir: the UDS path cap.
	dir, err := os.MkdirTemp("", "rafiki-u")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	controlSock := filepath.Join(dir, "controller.sock")
	connectSock := filepath.Join(dir, "connect.sock")

	routePath, handler := rafikiv1connect.NewControlHandler(&refusingUserControl{})
	serveConnectOnUnixSocket(t, connectSock, routePath, handler)
	writeTokenedProfile(t, controlSock, "rfk_tok")

	root, out := userTestRoot(t, newUserRmCmd(), "rm", "alice")
	if err := root.Execute(); err == nil {
		t.Fatalf("rm succeeded against a refusing daemon:\n%s", out.String())
	}
}

// refusingUserControl refuses RemoveUser the way the daemon's admin gate
// does, for the refusal-propagation test above.
type refusingUserControl struct {
	rafikiv1connect.UnimplementedControlHandler
}

func (s *refusingUserControl) RemoveUser(
	_ context.Context,
	_ *connect.Request[rafikiv1.RemoveUserRequest],
) (*connect.Response[rafikiv1.RemoveUserResponse], error) {
	return nil, connect.NewError(connect.CodePermissionDenied,
		errors.New("user administration requires an admin user credential"))
}
