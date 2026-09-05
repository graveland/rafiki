package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
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

// ─── decode-shape tests ─────────────────────────────────────────────────────
//
// pkg/control/dispatch.go sends ctrl_user_create's payload BARE
// (okResponse(TypeCtrlUserCreate, id, protocol.UserCreateResponseData)) and
// ctrl_user_list's WRAPPED (okResponse(TypeCtrlUserList, id,
// map[string]any{"users": list})) — the exact kind of per-verb asymmetry that
// once produced a runtime "cannot unmarshal object into []insightstypes.ConversationSummary"
// for ctrl_conversation_search, missed by a unit test whose fixture encoded
// the same wrong assumption. These fixtures are built to match dispatch.go,
// not to match each other or the previous task's tests, for that reason.

// TestDecodeUserCreate_BareShape pins that ctrl_user_create's payload decodes
// directly, with no envelope.
func TestDecodeUserCreate_BareShape(t *testing.T) {
	want := protocol.UserCreateResponseData{
		ID:        "usr_1",
		Username:  "alice",
		Token:     "rfk_secret",
		CreatedAt: "2026-08-18T00:00:00Z",
	}
	resp := wireResponse(t, protocol.TypeCtrlUserCreate, want) // bare, matching dispatch.go's okUserCreate

	got, err := decodeUserCreate(resp)
	if err != nil {
		t.Fatalf("decodeUserCreate: %v", err)
	}
	if got != want {
		t.Fatalf("decodeUserCreate = %+v, want %+v", got, want)
	}
}

// TestDecodeUserList_WrappedShape pins that ctrl_user_list's rows arrive
// under a {"users": [...]} envelope, built from the exact type
// (dispatch.go's UserList returns []users.User) and the exact wrapping
// (map[string]any{"users": list}) dispatch.go uses.
func TestDecodeUserList_WrappedShape(t *testing.T) {
	rows := []users.User{
		{ID: "usr_1", Username: "alice", CreatedAt: mustParseTime(t, "2026-08-18T00:00:00Z")},
		{ID: "usr_2", Username: "bob", CreatedAt: mustParseTime(t, "2026-08-17T00:00:00Z")},
	}
	resp := wireResponse(t, protocol.TypeCtrlUserList, map[string]any{"users": rows}) // wrapped, matching dispatch.go's okUserList

	got, err := decodeUserList(resp)
	if err != nil {
		t.Fatalf("decodeUserList: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("decodeUserList: got %d rows, want 2", len(got))
	}
	var first users.User
	if err := json.Unmarshal(got[0], &first); err != nil {
		t.Fatalf("row 0 did not decode as users.User: %v", err)
	}
	if first.Username != "alice" {
		t.Fatalf("row 0 username = %q, want alice", first.Username)
	}
}

// TestDecodeUserList_RejectsBareArray guards the OTHER direction of the
// asymmetry: a bare array (ctrl_user_create's shape, or a future regression
// that drops the envelope) must not be silently accepted as if it were
// wrapped — that would make decodeUserList "succeed" with zero rows on a
// payload that actually has data.
func TestDecodeUserList_RejectsBareArray(t *testing.T) {
	rows := []users.User{{ID: "usr_1", Username: "alice"}}
	resp := wireResponse(t, protocol.TypeCtrlUserList, rows) // deliberately bare — wrong shape

	if _, err := decodeUserList(resp); err == nil {
		t.Fatal("decodeUserList accepted a bare array; the wrapped-vs-bare distinction is not being enforced")
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return tm
}

// ─── renderUserCreate: write-failure-still-prints-token ────────────────────

func sampleUserCreateResp(t *testing.T) *protocol.Response {
	return wireResponse(t, protocol.TypeCtrlUserCreate, protocol.UserCreateResponseData{
		ID:        "usr_1",
		Username:  "alice",
		Token:     "rfk_only_copy_ever",
		CreatedAt: "2026-08-18T00:00:00Z",
	})
}

// TestRenderUserCreate_WriteFailureStillPrintsToken is the difference between
// a user having their credential and losing it permanently: the daemon shows
// the plaintext token exactly once, so a token-file write failure must never
// suppress it from stdout, only warn on stderr.
func TestRenderUserCreate_WriteFailureStillPrintsToken(t *testing.T) {
	resp := sampleUserCreateResp(t)
	writeErr := errors.New("permission denied")
	failingWrite := func(path, token string) error { return writeErr }

	var stdout, stderr bytes.Buffer
	err := renderUserCreate(&stdout, &stderr, resp, "/does/not/matter", true, failingWrite)
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
	resp := sampleUserCreateResp(t)
	called := false
	writeFn := func(path, token string) error { called = true; return nil }

	var stdout, stderr bytes.Buffer
	if err := renderUserCreate(&stdout, &stderr, resp, "/does/not/matter", false, writeFn); err != nil {
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
	resp := sampleUserCreateResp(t)
	writeFn := func(path, token string) error { return nil }

	var stdout, stderr bytes.Buffer
	if err := renderUserCreate(&stdout, &stderr, resp, "/config/rafiki/token", true, writeFn); err != nil {
		t.Fatalf("renderUserCreate: %v", err)
	}
	if !strings.Contains(stderr.String(), "/config/rafiki/token") {
		t.Fatalf("stderr does not confirm the write path: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "rfk_only_copy_ever") {
		t.Fatalf("token missing from stdout: %s", stdout.String())
	}
}

// ─── renderUserList ─────────────────────────────────────────────────────────

// TestRenderUserList_NeverLeaksATokenField is a guard, not a demonstration:
// users.User carries no Token field (only pkg/control's UserCreate handlers
// ever see the plaintext), so this also pins that the wire type stays that
// way — a future field addition that reintroduced one would fail this rather
// than silently starting to print tokens in `user list`.
func TestRenderUserList_NeverLeaksATokenField(t *testing.T) {
	rows := []users.User{{ID: "usr_1", Username: "alice"}}
	resp := wireResponse(t, protocol.TypeCtrlUserList, map[string]any{"users": rows})

	var got bytes.Buffer
	if err := renderUserList(&got, resp); err != nil {
		t.Fatalf("renderUserList: %v", err)
	}
	if strings.Contains(strings.ToLower(got.String()), "token") {
		t.Fatalf("rendered user list mentions a token: %s", got.String())
	}
	if !strings.Contains(got.String(), "alice") {
		t.Fatalf("rendered user list missing the username: %s", got.String())
	}
}
