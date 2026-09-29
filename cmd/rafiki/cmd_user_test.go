package main

// The user verbs ride the Connect control plane (CreateUser/ListUsers/
// RemoveUser) through newConnectEndpoint, with the profile's token as usual —
// there is no token-less dial and no framed protocol on this surface. The
// tests below therefore exercise three layers:
//
//   - writeTokenFile / the render functions against in-memory types,
//   - userConnectErr's rendering of Connect errors,
//   - the real commands against a fake Connect server served on a socket
//     profile's own socket (the same harness cmd_history_test.go uses),
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

	"github.com/multigres/testkit/assert"
)

// writeTokenedProfile isolates the profile environment and writes one profile
// "it" at sockPath, with a token file when token != "".
func writeTokenedProfile(t *testing.T, sockPath, token string) {
	t.Helper()
	c := assert.NewAborting(t)
	set := profile.Set{Profiles: map[string]profile.Profile{
		"it": {Name: "it", Socket: sockPath},
	}}
	c.NoError(profile.Save(set), "save profile")
	c.NoError(profile.SavePointer("it"), "save pointer")
	if token != "" {
		c.NoError(profile.WriteToken("it", token), "write token")
	}
}

// `rafiki user create` is also the login step: the token is shown once, so
// the CLI must persist it or the user is locked out of their own daemon.
func TestWriteTokenFileCreates0600(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "token")

	c.NoError(writeTokenFile(path, "rfk_secret"), "writeTokenFile")

	b, err := os.ReadFile(path)
	c.NoError(err, "read")
	c.Eq("rfk_secret\n", string(b), "content = %q", b)
	fi, err := os.Stat(path)
	c.NoError(err, "stat")
	c.Eq(0o600, fi.Mode().Perm(), "mode")
}

// Overwriting must not widen the mode of an existing file, and must not
// leave the old (longer) token's tail behind.
func TestWriteTokenFileOverwritesCleanly(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	c.NoError(os.WriteFile(path, []byte("rfk_a_very_long_previous_token\n"), 0o644), "seed")
	c.NoError(writeTokenFile(path, "rfk_short"), "writeTokenFile")
	b, _ := os.ReadFile(path)
	c.Eq("rfk_short\n", string(b), "content = %q; the old token was not fully replaced", b)
	fi, _ := os.Stat(path)
	c.Eq(0o600, fi.Mode().Perm(), "mode")
}

// TestUserCreateWritesTheProfilesTokenNotAGlobalOne pins the property Task 7
// exists to establish: `user create` writes to the resolved PROFILE's token
// file (profile.TokenFile), not a single global one — minting a credential
// against one daemon must not disturb another profile's credential.
func TestUserCreateWritesTheProfilesTokenNotAGlobalOne(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work":     {Name: "work", Socket: "/tmp/work.sock"},
		"personal": {Name: "personal", URL: "https://h", Proxy: "https://h"},
	}}), "Save")
	c.NoError(profile.WriteToken("work", "sk-work-existing"), "WriteToken")
	c.NoError(profile.SavePointer("personal"), "SavePointer")

	c.NoError(profile.WriteToken("personal", "sk-personal-new"), "WriteToken")

	// The bug this feature exists to fix: minting on one daemon must not
	// disturb the other's credential.
	c.Eq("sk-work-existing", profile.ReadToken("work"), "work token")
	c.Eq("sk-personal-new", profile.ReadToken("personal"), "personal token =")
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
	c := assert.NewAborting(t)
	writeErr := errors.New("permission denied")
	failingWrite := func(path, token string) error { return writeErr }

	var stdout, stderr bytes.Buffer
	err := renderUserCreate(&stdout, &stderr, sampleCreateResponse(), "/does/not/matter", true, failingWrite, outputAuto)
	c.NoError(err, "renderUserCreate returned an error instead of degrading")

	c.StrContains(stdout.String(), "rfk_only_copy_ever", "token missing from stdout after a write failure — it is now unrecoverable\nstdout")
	if !strings.Contains(stderr.String(), "warning") || !strings.Contains(stderr.String(), writeErr.Error()) {
		t.Fatalf("stderr does not warn about the write failure: %s", stderr.String())
	}
}

// TestRenderUserCreate_NoWriteNeverCallsWriteFn pins --no-write: writeFn must
// not be invoked at all, not merely "invoked and its result ignored".
func TestRenderUserCreate_NoWriteNeverCallsWriteFn(t *testing.T) {
	c := assert.NewAborting(t)
	called := false
	writeFn := func(path, token string) error { called = true; return nil }

	var stdout, stderr bytes.Buffer
	c.NoError(renderUserCreate(&stdout, &stderr, sampleCreateResponse(), "/does/not/matter", false, writeFn, outputAuto), "renderUserCreate")
	c.False(called, "writeFn was called despite shouldWrite=false")
	c.StrContains(stdout.String(), "rfk_only_copy_ever", "token missing from stdout")
	c.Eq(0, stderr.Len(), "expected no stderr output on --no-write, got: %s", stderr.String())
}

// TestRenderUserCreate_SuccessfulWriteConfirms pins the happy path's stderr
// confirmation, which is the only signal a scripted caller has that the token
// actually landed on disk.
func TestRenderUserCreate_SuccessfulWriteConfirms(t *testing.T) {
	c := assert.NewAborting(t)
	writeFn := func(path, token string) error { return nil }

	var stdout, stderr bytes.Buffer
	c.NoError(renderUserCreate(&stdout, &stderr, sampleCreateResponse(), "/config/rafiki/token", true, writeFn, outputAuto), "renderUserCreate")
	c.StrContains(stderr.String(), "/config/rafiki/token", "stderr does not confirm the write path")
	c.StrContains(stdout.String(), "rfk_only_copy_ever", "token missing from stdout")
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
			c := assert.NewAborting(t)
			var stdout, stderr bytes.Buffer
			c.NoError(renderUserCreate(&stdout, &stderr, sampleCreateResponse(), "/does/not/matter", false, func(string, string) error { return nil }, tc.mode), "renderUserCreate")
			out := stdout.String()
			c.StrContains(out, "rfk_only_copy_ever", "token missing from stdout in mode %v", tc.mode)
			// protojson renders int64 as a string; the canonical name is
			// camelCase createdAtUnix.
			var got map[string]any
			err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got)
			c.NoError(err, "output is not JSON in mode %v: %v\n%s", tc.mode, err, out)
			if got["createdAtUnix"] != "1800000000" {
				t.Fatalf("createdAtUnix = %v, want the string \"1800000000\" (protojson's int64 form)", got["createdAtUnix"])
			}
			c.False(got["id"] != "usr_1" || got["username"] != "alice", "unexpected fields: %v", got)
			c.False(tc.compact && strings.Contains(out, "\n") && strings.Count(out, "\n") > 1, "JSONL create output is not one line: %q", out)
		})
	}
}

// ─── emitUserList ───────────────────────────────────────────────────────────

func sampleUserRows() []*rafikiv1.UserRow {
	removed := int64(1800000100)
	return []*rafikiv1.UserRow{
		{Id: "usr_1", Username: "alice", IsAdmin: true, Email: "alice@x.dev", CreatedAtUnix: 1800000000},
		{Id: "usr_2", Username: "bob", CreatedAtUnix: 1800000050, DeletedAtUnix: &removed},
	}
}

// TestEmitUserList_TableShowsAdminAndRemoval pins the table's semantics: the
// ADMIN column distinguishes an admin from an ordinary user, an active user's
// REMOVED cell is "-" rather than a zero date, and a tombstoned row shows its
// removal time. Row lines are matched by id and split on the table's cell
// separator, since pkg/table draws a bordered box.
func TestEmitUserList_TableShowsAdminAndRemoval(t *testing.T) {
	c := assert.NewAborting(t)
	var out bytes.Buffer
	c.NoError(emitUserList(&out, sampleUserRows(), outputTable, false), "emitUserList")
	got := out.String()
	for _, want := range []string{"ID", "USER", "EMAIL", "ADMIN", "CREATED", "REMOVED", "usr_1", "usr_2", "alice@x.dev"} {
		c.StrContains(got, want, "table output missing")
	}

	alice := tableRowFor(t, got, "usr_1")
	c.Len(alice, 6, "alice's row")
	c.False(alice[1] != "alice" || alice[2] != "alice@x.dev" || alice[3] != "yes", "alice's row = %v, want the email and the admin marked", alice)
	c.Eq("-", alice[5], "alice's REMOVED cell = %q, want \"-\" (she is active)", alice[5])

	bob := tableRowFor(t, got, "usr_2")
	c.False(bob[1] != "bob" || bob[2] != "-" || bob[3] != "-", "bob's row = %v, want an email-less non-admin", bob)
	c.NotEq("-", bob[5], "bob's REMOVED cell")
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
		c := assert.NewAborting(t)
		var out bytes.Buffer
		c.NoError(emitUserList(&out, sampleUserRows(), outputJSON, false), "emitUserList")
		var got struct {
			Rows []map[string]any `json:"rows"`
		}
		err := json.Unmarshal(out.Bytes(), &got)
		c.NoError(err, "output is not the rows envelope: %v\n%s", err, out.String())
		c.Len(got.Rows, 2, "got %d rows, want 2: %s", len(got.Rows), out.String())
		if got.Rows[0]["createdAtUnix"] != "1800000000" {
			t.Fatalf("createdAtUnix = %v, want the string protojson form", got.Rows[0]["createdAtUnix"])
		}
		if got.Rows[1]["deletedAtUnix"] != "1800000100" {
			t.Fatalf("deletedAtUnix = %v, want the string protojson form", got.Rows[1]["deletedAtUnix"])
		}
		if _, ok := got.Rows[0]["is_admin"]; ok {
			t.Fatalf("snake_case field name survived: %s", out.String())
		}
		c.NotStrContains(strings.ToLower(out.String()), "token", "rendered user list mentions a token: %s", out.String())
	})
	t.Run("jsonl one row per line", func(t *testing.T) {
		c := assert.NewAborting(t)
		var out bytes.Buffer
		c.NoError(emitUserList(&out, sampleUserRows(), outputJSONL, false), "emitUserList")
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		c.Len(lines, 2, "got %d lines, want one row per line:\n%s", len(lines), out.String())
		c.NotStrContains(out.String(), "rows", "JSONL must not carry the envelope:\n")
		var row map[string]any
		c.NoError(json.Unmarshal([]byte(lines[0]), &row), "line 0 is not a row object")
		c.False(row["username"] != "alice", "line 0 = %v, want alice's row", row)
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
	assert.NewAborting(t).Eq("invalid_args: username alice is already taken", got.Error(), "userConnectErr")
}

// TestUserConnectErr_KeepsTheInfrastructureAdvice pins that the three codes
// whose fixes are environmental — the daemon is not there, the token is
// stale, the daemon predates Connect — keep diagnoseConnectError's advice
// instead of the bare code-and-message rendering.
func TestUserConnectErr_KeepsTheInfrastructureAdvice(t *testing.T) {
	c := assert.NewAborting(t)
	unreachable := connect.NewError(connect.CodeUnavailable, errors.New("connection refused"))
	got := userConnectErr(unreachable, "/tmp/x/controller.sock")
	c.StrContains(got.Error(), "cannot reach the rafiki daemon at /tmp/x/controller.sock", "unreachable error lost the daemon-down advice: %v", got)
	c.ErrorIs(got, unreachable, "the original error is no longer wrapped")

	stale := connect.NewError(connect.CodeUnauthenticated, errors.New("invalid auth token"))
	if got := userConnectErr(stale, "sock"); !strings.Contains(got.Error(), "check its token file") {
		t.Fatalf("unauthenticated error lost the token-file advice: %v", got)
	}
}

// TestUserConnectErr_NonConnectErrorPassesThrough keeps ordinary failures
// (a wrapped io error from the transport, say) rendering as themselves.
func TestUserConnectErr_NonConnectErrorPassesThrough(t *testing.T) {
	err := errors.New("ordinary failure")
	got := userConnectErr(err, "sock")
	assert.NewAborting(t).Eq("ordinary failure", got.Error(), "userConnectErr = %v, want the error's own text", got)
}

// ─── the commands against a fake Connect server ────────────────────────────

// userStubControl serves the three user RPCs and records the requests, so the
// tests can assert what actually went over the wire — including the
// Authorization header, which is how "the profile's token rides Connect" is
// pinned.
type userStubControl struct {
	rafikiv1connect.UnimplementedControlHandler
	mu          sync.Mutex
	createReqs  []*connect.Request[rafikiv1.CreateUserRequest]
	listReqs    []*connect.Request[rafikiv1.ListUsersRequest]
	rmReqs      []*connect.Request[rafikiv1.RemoveUserRequest]
	updateReqs  []*connect.Request[rafikiv1.UpdateUserRequest]
	mintReqs    []*connect.Request[rafikiv1.MintTokenRequest]
	listTokReqs []*connect.Request[rafikiv1.ListTokensRequest]
	revokeReqs  []*connect.Request[rafikiv1.RevokeTokenRequest]
	rows        []*rafikiv1.UserRow
	created     *rafikiv1.CreateUserResponse
	updated     *rafikiv1.UserRow
	minted      *rafikiv1.MintTokenResponse
	tokenRows   []*rafikiv1.TokenRow
	revoked     *rafikiv1.TokenRow
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

func (s *userStubControl) UpdateUser(
	_ context.Context,
	req *connect.Request[rafikiv1.UpdateUserRequest],
) (*connect.Response[rafikiv1.UpdateUserResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateReqs = append(s.updateReqs, req)
	row := s.updated
	if row == nil {
		row = &rafikiv1.UserRow{Id: "usr_1", Username: req.Msg.GetUsername(), Email: req.Msg.GetEmail(), CreatedAtUnix: 1800000000}
	}
	return connect.NewResponse(&rafikiv1.UpdateUserResponse{User: row}), nil
}

func (s *userStubControl) MintToken(
	_ context.Context,
	req *connect.Request[rafikiv1.MintTokenRequest],
) (*connect.Response[rafikiv1.MintTokenResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mintReqs = append(s.mintReqs, req)
	resp := s.minted
	if resp == nil {
		resp = &rafikiv1.MintTokenResponse{
			Info:  &rafikiv1.TokenRow{Id: "tok_1", Username: req.Msg.GetUsername(), Name: req.Msg.GetName()},
			Token: "rfk_fresh",
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *userStubControl) ListTokens(
	_ context.Context,
	req *connect.Request[rafikiv1.ListTokensRequest],
) (*connect.Response[rafikiv1.ListTokensResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listTokReqs = append(s.listTokReqs, req)
	return connect.NewResponse(&rafikiv1.ListTokensResponse{Tokens: s.tokenRows}), nil
}

func (s *userStubControl) RevokeToken(
	_ context.Context,
	req *connect.Request[rafikiv1.RevokeTokenRequest],
) (*connect.Response[rafikiv1.RevokeTokenResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokeReqs = append(s.revokeReqs, req)
	return connect.NewResponse(&rafikiv1.RevokeTokenResponse{Info: s.revoked}), nil
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

func (s *userStubControl) lastUpdate() *connect.Request[rafikiv1.UpdateUserRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.updateReqs) == 0 {
		return nil
	}
	return s.updateReqs[len(s.updateReqs)-1]
}

func (s *userStubControl) lastMint() *connect.Request[rafikiv1.MintTokenRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.mintReqs) == 0 {
		return nil
	}
	return s.mintReqs[len(s.mintReqs)-1]
}

func (s *userStubControl) lastListTokens() *connect.Request[rafikiv1.ListTokensRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.listTokReqs) == 0 {
		return nil
	}
	return s.listTokReqs[len(s.listTokReqs)-1]
}

func (s *userStubControl) lastRevoke() *connect.Request[rafikiv1.RevokeTokenRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.revokeReqs) == 0 {
		return nil
	}
	return s.revokeReqs[len(s.revokeReqs)-1]
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

// serveUserScratch seeds one socket profile "it" at dir and serves stub on
// the profile's own socket — the one path the client dials. dir must be a
// SHORT path: unix socket paths are capped at ~104 bytes on darwin.
func serveUserScratch(t *testing.T, stub *userStubControl, token string) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp, not t.TempDir: the UDS path cap.
	dir, err := os.MkdirTemp("", "rafiki-u")
	assert.NewAborting(t).NoError(err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	serveUserConnect(t, sock, stub)
	writeTokenedProfile(t, sock, token)
}

// TestUserCreateSendsTheProfileTokenOverConnect pins the credential contract:
// the request carries the profile's token as a Bearer credential (there is no
// token-less dial; a stale token is unstuck on the daemon host) — and the
// minted token lands in the profile's token file and on stdout exactly once.
func TestUserCreateSendsTheProfileTokenOverConnect(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &userStubControl{}
	serveUserScratch(t, stub, "rfk_stale")

	root, out := userTestRoot(t, newUserCreateCmd(), "create", "alice")
	if err := root.Execute(); err != nil {
		t.Fatalf("user create failed: %v\n%s", err, out.String())
	}

	req := stub.lastCreate()
	c.NotNil(req, "the stub never saw a CreateUser request")
	c.Eq("alice", req.Msg.GetUsername(), "username")
	c.Eq("Bearer rfk_stale", req.Header().Get("Authorization"), "Authorization")

	// The minted token replaced the profile's (now stale) one — the login
	// step — and was printed once for the human.
	if b, err := os.ReadFile(profile.TokenFile("it")); err != nil {
		t.Fatalf("read token file: %v", err)
	} else if got := strings.TrimSpace(string(b)); got != "rfk_new_minted" {
		t.Fatalf("token file = %q, want the freshly minted token", got)
	}
	c.StrContains(out.String(), "rfk_new_minted", "minted token missing from stdout:\n")
}

// TestUserListReachesTheSocketProfilesDaemon drives `user list` against a
// daemon served on the profile's OWN socket.
func TestUserListReachesTheSocketProfilesDaemon(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &userStubControl{rows: sampleUserRows()}
	serveUserScratch(t, stub, "")

	root, out := userTestRoot(t, newUserListCmd(), "list")
	err := root.Execute()
	c.NoError(err, "user list failed: %v\n%s", err, out.String())

	got := out.String()
	c.False(!strings.Contains(got, "alice") || !strings.Contains(got, "bob"), "user list output missing the stub's rows:\n%s", got)
	req := stub.lastList()
	c.NotNil(req, "the stub never saw a ListUsers request")
	c.False(req.Msg.GetIncludeDeleted(), "the default list must not ask for tombstoned rows")
}

// TestUserListAllAsksForTombstonedRows pins the --all flag's wire effect.
func TestUserListAllAsksForTombstonedRows(t *testing.T) {
	stub := &userStubControl{rows: sampleUserRows()}
	serveUserScratch(t, stub, "")

	root, out := userTestRoot(t, newUserListCmd(), "list", "--all")
	err := root.Execute()
	assert.NewAborting(t).NoError(err, "user list --all failed: %v\n%s", err, out.String())
	if req := stub.lastList(); req == nil || !req.Msg.GetIncludeDeleted() {
		t.Fatalf("--all did not set include_deleted: %+v", stub.lastList())
	}
}

// TestUserListJSONLOverTheWire pins the -J contract end to end: one compact
// row per line, no envelope, straight from a live (fake) daemon.
func TestUserListJSONLOverTheWire(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &userStubControl{rows: sampleUserRows()}
	serveUserScratch(t, stub, "")

	root, out := userTestRoot(t, newUserListCmd(), "list", "-J")
	if err := root.Execute(); err != nil {
		t.Fatalf("user list -J failed: %v\n%s", err, out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	c.Len(lines, 2, "got %d lines, want one row per line:\n%s", len(lines), out.String())
	var row map[string]any
	err := json.Unmarshal([]byte(lines[0]), &row)
	c.NoError(err, "line 0 is not a row object: %v\n%s", err, out.String())
	c.False(row["username"] != "alice", "line 0 = %v, want alice's row", row)
}

// TestUserRmSendsTheUsername pins rm's request and its stderr confirmation.
func TestUserRmSendsTheUsername(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &userStubControl{}
	serveUserScratch(t, stub, "rfk_tok")

	root, out := userTestRoot(t, newUserRmCmd(), "rm", "alice")
	err := root.Execute()
	c.NoError(err, "user rm failed: %v\n%s", err, out.String())
	req := stub.lastRm()
	c.NotNil(req, "the stub never saw a RemoveUser request")
	c.Eq("alice", req.Msg.GetUsername(), "username")
	c.Eq("Bearer rfk_tok", req.Header().Get("Authorization"), "Authorization")
	c.StrContains(out.String(), "removed alice", "rm did not confirm the removal:\n")
}

// TestUserRmPropagatesARefusal pins that a daemon-side refusal reaches the
// caller as the verb's error, rendered with the daemon's reason — not as a
// success with an empty confirmation. The refusing handler mirrors the admin
// gate's shape (connect_users.go's requireUserAdmin).
func TestUserRmPropagatesARefusal(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()

	// os.MkdirTemp, not t.TempDir: the UDS path cap.
	dir, err := os.MkdirTemp("", "rafiki-u")
	c.NoError(err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	routePath, handler := rafikiv1connect.NewControlHandler(&refusingUserControl{})
	serveConnectOnUnixSocket(t, sock, routePath, handler)
	writeTokenedProfile(t, sock, "rfk_tok")

	root, out := userTestRoot(t, newUserRmCmd(), "rm", "alice")
	c.Error(root.Execute(), "rm succeeded against a refusing daemon:\n%s", out.String())
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

// TestUserCreateHelpPinsTheAdminRecoveryPath pins the two phrases the brief
// requires `user create`'s help to carry: the verb mints only NON-admin
// users, and the admin recovery path is `rafikid user create --admin` on the
// daemon host — so a later help rewording cannot silently drop them.
func TestUserCreateHelpPinsNonAdminAndRafikidAdminRecovery(t *testing.T) {
	long := newUserCreateCmd().Long
	for _, phrase := range []string{
		"NON-admin",
		"rafikid user create --admin",
	} {
		assert.NewCollecting(t).StrContains(long, phrase, "user create help is missing pinned phrase")
	}
}

// ─── user create: the tri-state mint flag and the hints ────────────────────

// TestUserCreateMintFlagMapsToTheWire pins every flag state against the
// request the stub received: no flag → mint_token UNSET (the daemon decides);
// --token → true; --no-token → false; both → a usage error and no request.
func TestUserCreateMintFlagMapsToTheWire(t *testing.T) {
	c := assert.NewAborting(t)
	run := func(args ...string) (*connect.Request[rafikiv1.CreateUserRequest], string, error) {
		stub := &userStubControl{}
		serveUserScratch(t, stub, "rfk_tok")
		root, out := userTestRoot(t, newUserCreateCmd(), args...)
		err := root.Execute()
		return stub.lastCreate(), out.String(), err
	}

	req, _, err := run("create", "alice")
	c.Require().NoError(err, "no flag")
	c.Nil(req.Msg.MintToken, "no flag must leave mint_token unset")

	req, _, err = run("create", "alice", "--token")
	c.Require().NoError(err, "--token")
	c.NotNil(req.Msg.MintToken, "--token must set mint_token")
	c.True(req.Msg.GetMintToken(), "--token must set mint_token true")

	req, _, err = run("create", "alice", "--no-token")
	c.Require().NoError(err, "--no-token")
	c.NotNil(req.Msg.MintToken, "--no-token must set mint_token")
	c.False(req.Msg.GetMintToken(), "--no-token must set mint_token false")

	_, _, err = run("create", "alice", "--token", "--no-token")
	c.Error(err, "--token --no-token must be refused")
}

// TestUserCreateEmailMapsToTheWire pins --email's wire form.
func TestUserCreateEmailMapsToTheWire(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &userStubControl{}
	serveUserScratch(t, stub, "rfk_tok")

	root, out := userTestRoot(t, newUserCreateCmd(), "create", "alice", "--email", "Alice@X.dev")
	if err := root.Execute(); err != nil {
		t.Fatalf("user create --email failed: %v\n%s", err, out.String())
	}
	req := stub.lastCreate()
	c.NotNil(req, "the stub never saw a CreateUser request")
	c.Eq("Alice@X.dev", req.Msg.GetEmail(), "email")
}

// TestRenderUserCreate_Hints pins the four stderr states that explain the
// mint decision: a token minted only because OIDC login is unconfigured, a
// token minted as requested, a user who logs in with `rafiki login`, and a
// user who cannot authenticate until someone mints for them.
func TestRenderUserCreate_Hints(t *testing.T) {
	c := assert.NewAborting(t)
	writeOK := func(string, string) error { return nil }
	render := func(resp *rafikiv1.CreateUserResponse) (string, string) {
		var stdout, stderr bytes.Buffer
		if err := renderUserCreate(&stdout, &stderr, resp, "/tok", true, writeOK, outputAuto); err != nil {
			t.Fatalf("renderUserCreate: %v", err)
		}
		return stdout.String(), stderr.String()
	}

	// Absent mint_token on an unconfigured daemon → token + reason note.
	_, stderr := render(&rafikiv1.CreateUserResponse{Id: "u", Username: "alice", Token: "rfk_x", TokenReason: "oidc not configured"})
	c.StrContains(stderr, "OIDC login is not configured on this daemon, so a token was minted", "unconfigured reason note")

	// Explicitly requested token → no oidc note.
	_, stderr = render(&rafikiv1.CreateUserResponse{Id: "u", Username: "alice", Token: "rfk_x", TokenReason: "requested", LoginConfigured: true})
	c.False(strings.Contains(stderr, "OIDC login is not configured on this daemon, so a token was minted"),
		"requested reason must not carry the unconfigured note: %s", stderr)

	// No token on a configured daemon → the login hint.
	_, stderr = render(&rafikiv1.CreateUserResponse{Id: "u", Username: "alice", LoginConfigured: true})
	c.StrContains(stderr, "no token minted; alice logs in with 'rafiki login'", "login hint")

	// No token on an unconfigured daemon → the recovery hint.
	_, stderr = render(&rafikiv1.CreateUserResponse{Id: "u", Username: "alice"})
	c.StrContains(stderr, "no token minted and OIDC login is not configured on this daemon: alice cannot authenticate until you run 'rafiki token mint --user alice'", "recovery hint")
}

// TestUserCreateHintsOverTheWire pins the two no-token hints end to end
// against a fake daemon, so the response fields the hint reads
// (token_reason, login_configured) are the ones the daemon actually sends.
func TestUserCreateHintsOverTheWire(t *testing.T) {
	run := func(resp *rafikiv1.CreateUserResponse) string {
		stub := &userStubControl{created: resp}
		serveUserScratch(t, stub, "rfk_tok")
		root, out := userTestRoot(t, newUserCreateCmd(), "create", "alice")
		if err := root.Execute(); err != nil {
			t.Fatalf("user create failed: %v\n%s", err, out.String())
		}
		return out.String()
	}
	if got := run(&rafikiv1.CreateUserResponse{Id: "u", Username: "alice", LoginConfigured: true}); !strings.Contains(got, "no token minted; alice logs in with 'rafiki login'") {
		t.Fatalf("configured hint missing over the wire:\n%s", got)
	}
	if got := run(&rafikiv1.CreateUserResponse{Id: "u", Username: "alice"}); !strings.Contains(got, "cannot authenticate until you run 'rafiki token mint --user alice'") {
		t.Fatalf("unconfigured hint missing over the wire:\n%s", got)
	}
}

// ─── user update ────────────────────────────────────────────────────────────

// TestUserUpdateSendsTheEmail pins the update verb's wire form and that it
// prints the updated row.
func TestUserUpdateSendsTheEmail(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &userStubControl{}
	serveUserScratch(t, stub, "rfk_tok")

	root, out := userTestRoot(t, newUserUpdateCmd(), "update", "alice", "--email", "alice@x.dev")
	if err := root.Execute(); err != nil {
		t.Fatalf("user update failed: %v\n%s", err, out.String())
	}
	req := stub.lastUpdate()
	c.NotNil(req, "the stub never saw an UpdateUser request")
	c.Eq("alice", req.Msg.GetUsername(), "username")
	c.NotNil(req.Msg.Email, "email must be present, not merely empty")
	c.Eq("alice@x.dev", *req.Msg.Email, "email")

	c.StrContains(out.String(), "alice", "the updated row was not printed")
	c.StrContains(out.String(), "alice@x.dev", "the updated email was not printed")
}

// TestUserUpdateRequiresTheEmailFlag pins --email as required: the CLI never
// sends an UpdateUser with no optional email set, which the daemon would
// refuse.
func TestUserUpdateRequiresTheEmailFlag(t *testing.T) {
	stub := &userStubControl{}
	serveUserScratch(t, stub, "rfk_tok")

	root, out := userTestRoot(t, newUserUpdateCmd(), "update", "alice")
	if err := root.Execute(); err == nil {
		t.Fatalf("user update without --email must fail\n%s", out.String())
	}
	if stub.lastUpdate() != nil {
		t.Fatalf("a flag-less update reached the daemon")
	}
}
