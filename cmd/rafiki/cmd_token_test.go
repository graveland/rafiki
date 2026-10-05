package main

// The token verbs' flag→request mapping, the mint output contract (the
// plaintext token, once, and nothing else in table mode) and the list table's
// columns, against the same fake Connect harness the user verbs use.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func sampleTokenRows() []*rafikiv1.TokenRow {
	expires := time.Unix(1800003600, 0).UTC()
	revoked := time.Unix(1800000600, 0).UTC()
	return []*rafikiv1.TokenRow{
		{Id: "tok_1", Username: "alice", Name: "ci laptop", Origin: "service", CreatedAt: timestamppb.New(time.Unix(1800000000, 0))},
		{Id: "tok_2", Username: "alice", Name: "oidc session", Origin: "oidc", CreatedAt: timestamppb.New(time.Unix(1800000050, 0)), ExpiresAt: timestamppb.New(expires), RevokedAt: timestamppb.New(revoked)},
	}
}

// TestTokenMintSendsTheFlags pins --name/--ttl/--user onto the wire request
// and the table-mode output contract: the plaintext token on stdout, once,
// and nothing else beside it.
func TestTokenMintSendsTheFlags(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &userStubControl{}
	serveUserScratch(t, stub, "rfk_tok")

	root, out := userTestRoot(t, newTokenMintCmd(), "mint", "--name", "ci", "--ttl", "720h", "--user", "alice")
	if err := root.Execute(); err != nil {
		t.Fatalf("token mint failed: %v\n%s", err, out.String())
	}
	req := stub.lastMint()
	c.Require().NotNil(req, "the stub never saw a MintToken request")
	c.Eq("ci", req.Msg.GetName(), "name")
	c.Eq("alice", req.Msg.GetUsername(), "username")
	c.Eq(int64(720*3600), req.Msg.GetTtlSeconds(), "ttl seconds")

	c.Eq("rfk_fresh\n", out.String(), "table mode must print the token and nothing else")
}

// TestTokenMintDefaultsNameToTheHostname pins the default token name, which
// exists so a human can tell which machine minted what.
func TestTokenMintDefaultsNameToTheHostname(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &userStubControl{}
	serveUserScratch(t, stub, "rfk_tok")

	root, out := userTestRoot(t, newTokenMintCmd(), "mint")
	if err := root.Execute(); err != nil {
		t.Fatalf("token mint failed: %v\n%s", err, out.String())
	}
	req := stub.lastMint()
	c.Require().NotNil(req, "the stub never saw a MintToken request")
	c.Eq(int64(0), req.Msg.GetTtlSeconds(), "no --ttl means never expires")
	c.Eq("", req.Msg.GetUsername(), "no --user means the caller")
	if host, err := os.Hostname(); err == nil && host != "" {
		c.Eq("cli "+host, req.Msg.GetName(), "default name")
	} else {
		c.StrContains(req.Msg.GetName(), "cli ", "default name even without a hostname")
	}
}

// TestTokenMintRefusesABadTTL pins the local parse: a malformed duration is a
// user-input error, refused before any connection.
func TestTokenMintRefusesABadTTL(t *testing.T) {
	stub := &userStubControl{}
	serveUserScratch(t, stub, "rfk_tok")

	root, out := userTestRoot(t, newTokenMintCmd(), "mint", "--ttl", "forever")
	if err := root.Execute(); err == nil {
		t.Fatalf("a malformed --ttl must fail\n%s", out.String())
	}
	if stub.lastMint() != nil {
		t.Fatalf("a malformed --ttl reached the daemon")
	}
}

// TestTokenMintRefusesASubSecondTTL pins the truncation guard: whole-second
// conversion would round 0.5s down to 0, which the wire reads as "never
// expires" — a fail-open that turns a short-lived credential into a permanent
// one. The error must name the flag, and nothing may reach the daemon.
func TestTokenMintRefusesASubSecondTTL(t *testing.T) {
	stub := &userStubControl{}
	serveUserScratch(t, stub, "rfk_tok")

	root, out := userTestRoot(t, newTokenMintCmd(), "mint", "--ttl", "0.5s")
	err := root.Execute()
	if err == nil {
		t.Fatalf("a sub-second --ttl must fail\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "--ttl") {
		t.Fatalf("the error must name the flag, got: %v", err)
	}
	if stub.lastMint() != nil {
		t.Fatalf("a sub-second --ttl reached the daemon")
	}
}

// TestTokenMintJSONCarriesTheToken pins -j/-J: the canonical protojson of the
// response, token included — the plaintext is the response's payload and the
// JSON modes are its other rendering, not a redaction.
func TestTokenMintJSONCarriesTheToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"json", []string{"mint", "-j"}},
		{"jsonl", []string{"mint", "-J"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			stub := &userStubControl{}
			serveUserScratch(t, stub, "rfk_tok")
			root, out := userTestRoot(t, newTokenMintCmd(), tc.args...)
			if err := root.Execute(); err != nil {
				t.Fatalf("token mint failed: %v\n%s", err, out.String())
			}
			c.StrContains(out.String(), "rfk_fresh", "token missing from JSON output")
			var got map[string]any
			c.NoError(json.Unmarshal([]byte(strings.TrimSpace(out.String())), &got), "output is not JSON: %s", out.String())
			c.Eq("rfk_fresh", got["token"], "protojson token field")
		})
	}
}

// TestTokenLsSendsTheFlags pins --user/--all/--revoked onto the wire request
// and the table's columns.
func TestTokenLsSendsTheFlags(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &userStubControl{tokenRows: sampleTokenRows()}
	serveUserScratch(t, stub, "rfk_tok")

	root, out := userTestRoot(t, newTokenLsCmd(), "ls", "--user", "bob", "--all", "--revoked")
	if err := root.Execute(); err != nil {
		t.Fatalf("token ls failed: %v\n%s", err, out.String())
	}
	req := stub.lastListTokens()
	c.Require().NotNil(req, "the stub never saw a ListTokens request")
	c.Eq("bob", req.Msg.GetUsername(), "username")
	c.True(req.Msg.GetAllUsers(), "all_users")
	c.True(req.Msg.GetIncludeRevoked(), "include_revoked")

	got := out.String()
	for _, want := range []string{"ID", "USER", "NAME", "ORIGIN", "CREATED", "EXPIRES", "REVOKED", "tok_1", "tok_2", "service", "oidc"} {
		c.StrContains(got, want, "table output missing")
	}
}

// TestTokenLsJSONLIsOneRowPerLine pins the -J contract for token rows.
func TestTokenLsJSONLIsOneRowPerLine(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &userStubControl{tokenRows: sampleTokenRows()}
	serveUserScratch(t, stub, "rfk_tok")

	root, out := userTestRoot(t, newTokenLsCmd(), "ls", "-J")
	if err := root.Execute(); err != nil {
		t.Fatalf("token ls -J failed: %v\n%s", err, out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	c.Len(lines, 2, "got %d lines, want one row per line:\n%s", len(lines), out.String())
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("line 0 is not a row object: %v\n%s", err, out.String())
	}
	c.Eq("tok_1", row["id"], "line 0")
}

// TestTokenRevokeSendsTheID pins the revoke verb's wire form and its stderr
// confirmation.
func TestTokenRevokeSendsTheID(t *testing.T) {
	c := assert.NewAborting(t)
	stub := &userStubControl{revoked: &rafikiv1.TokenRow{Id: "tok_1"}}
	serveUserScratch(t, stub, "rfk_tok")

	root, out := userTestRoot(t, newTokenRevokeCmd(), "revoke", "tok_1")
	if err := root.Execute(); err != nil {
		t.Fatalf("token revoke failed: %v\n%s", err, out.String())
	}
	req := stub.lastRevoke()
	c.NotNil(req, "the stub never saw a RevokeToken request")
	c.Eq("tok_1", req.Msg.GetId(), "id")
	c.StrContains(out.String(), "revoked tok_1", "revoke did not confirm:\n")
}

// TestTokenCmdRegistersItsVerbs pins the tree, so a refactor cannot silently
// orphan a verb.
func TestTokenCmdRegistersItsVerbs(t *testing.T) {
	cmd := newTokenCmd()
	for _, name := range []string{"mint", "ls", "revoke"} {
		_, _, err := cmd.Find([]string{name})
		assert.NewAborting(t).NoError(err, "token %s missing", name)
	}
}
