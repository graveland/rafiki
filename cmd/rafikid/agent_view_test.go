package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/users"
)

// agentViewDBFixture is a seeded STORED conversation behind c_grandchild (the
// one descendant of the caller c_mine), shaped to exercise every mode:
//
//   - 56 ordinary prompt/reply rows, so the newest page is not the whole log
//     and prints a `before=` marker;
//   - one user row carrying two tool_results, so one row renders as two
//     entries and `<sub>` has to do real work;
//   - one 5 000-char assistant reply whose word "zebra" sits past character
//     3 000 — the only way to see it whole is entry=, and the only way to find
//     it is find= (the page form is cut at 2 000);
//   - one final assistant row the model did not finish (max_tokens), which must
//     say so rather than look like the tool stopped reading.
type agentViewDBFixture struct {
	ctrl   *Controller
	pool   *pgxpool.Pool
	convID string
	long   string // the long reply's text as entry= renders it (trimmed)
}

func agentViewDBFixtureFor(t *testing.T) agentViewDBFixture {
	t.Helper()
	ck := assert.NewAborting(t)
	pool := scratchPool(t)
	ctx := context.Background()

	var convID string
	ck.NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		 VALUES ('test', 'server') RETURNING id::text`).Scan(&convID), "insert conversation")

	msgs := store.NewMessages(pool)
	appendRow := func(param anthropic.MessageParam, meta *store.AssistantMeta) {
		_, aerr := msgs.AppendNode(ctx, convID, param, meta, "")
		ck.NoError(aerr, "append row")
	}
	user := func(text string) anthropic.MessageParam {
		return anthropic.NewUserMessage(anthropic.NewTextBlock(text))
	}
	assistant := func(text string) anthropic.MessageParam {
		return anthropic.NewAssistantMessage(anthropic.NewTextBlock(text))
	}
	for i := 0; i < 28; i++ {
		appendRow(user(fmt.Sprintf("prompt-%d", i)), nil)
		appendRow(assistant(fmt.Sprintf("reply-%d", i)), nil)
	}
	appendRow(anthropic.NewUserMessage(
		anthropic.NewToolResultBlock("tu_1", "first result", false),
		anthropic.NewToolResultBlock("tu_2", "second result", false),
	), nil)

	var long strings.Builder
	for long.Len() < 3000 {
		long.WriteString("filler line for the long reply\n")
	}
	long.WriteString("the word zebra sits here past char 3000\n")
	for long.Len() < 5000 {
		long.WriteString("trailing context line\n")
	}
	appendRow(assistant(long.String()), nil)
	appendRow(assistant("reply-final"), &store.AssistantMeta{StopReason: "max_tokens"})

	ctrl := spawnerFixture(t)
	ctrl.pool = pool
	ck.NoError(ctrl.st.Update("c_grandchild", func(s *childstore.Session) {
		s.SessionID = convID
	}), "point c_grandchild at the seeded conversation")

	return agentViewDBFixture{ctrl: ctrl, pool: pool, convID: convID, long: strings.TrimSpace(long.String())}
}

// markerCursor reads the cursor out of the first "<key>=<cursor>]" marker in
// text — exactly what a reader would copy, which is the contract: the caller
// never builds a cursor.
func markerCursor(t *testing.T, text, key string) string {
	t.Helper()
	i := strings.Index(text, key+"=")
	if i < 0 {
		t.Fatalf("no %s= marker in:\n%s", key, text)
	}
	rest := text[i+len(key)+1:]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		t.Fatalf("unterminated %s= marker in:\n%s", key, text)
	}
	return rest[:end]
}

// hitCursor reads the cursor off the first find hit line: every page line
// starts with its entry's cursor, hits included.
func hitCursor(t *testing.T, text string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "m:") {
			return strings.Fields(line)[0]
		}
	}
	t.Fatalf("no hit line carrying a cursor in:\n%s", text)
	return ""
}

// TestAgentViewPagesAStoredConversation is the end-to-end db path: every mode
// of agent_view reaches real stored text through the spawner the tool calls.
func TestAgentViewPagesAStoredConversation(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := agentViewDBFixtureFor(t)
	sp := newControllerSpawner(f.ctrl, "c_mine")
	ctx := context.Background()
	target := "c_grandchild"

	newest, err := sp.View(ctx, tools.ViewSpec{ChildID: target})
	ck.Require().NoError(err, "plain view")
	ck.StrContains(newest, "entries · showing", "the newest page must be headed with where it stands; got:\n%s", newest)
	ck.StrContains(newest, "earlier — before=", "the newest page must name how to read further back; got:\n%s", newest)
	ck.StrContains(newest, endedEarlyMarker, "the max_tokens row must say the MODEL stopped; got:\n%s", newest)

	// Follow before= back to the beginning. Nothing may be unreachable.
	page, earlier := newest, markerCursor(t, newest, "before")
	for i := 0; ; i++ {
		if i > 20 {
			t.Fatalf("before= did not reach the start of the conversation:\n%s", page)
		}
		page, err = sp.View(ctx, tools.ViewSpec{ChildID: target, Before: earlier})
		ck.Require().NoError(err, "before=%s", earlier)
		ck.StrContains(page, "entries · showing", "before= must render a page; got:\n%s", page)
		if !strings.Contains(page, "earlier — before=") {
			break
		}
		earlier = markerCursor(t, page, "before")
	}
	ck.StrContains(page, "user: prompt-0", "following before= must reach the first prompt; got:\n%s", page)

	// find= sees past the 2 000-char page cap, and names the cursor to expand.
	found, err := sp.View(ctx, tools.ViewSpec{ChildID: target, Find: "ZEBRA"})
	ck.Require().NoError(err, "find=ZEBRA")
	ck.StrContains(found, "zebra", "a find hit must show the match in its snippet; got:\n%s", found)
	cursor := hitCursor(t, found)
	ck.True(strings.HasPrefix(cursor, "m:"), "a hit's line must start with its cursor; got %q", cursor)

	// entry= returns the whole 5 000-char reply, uncut, with real newlines.
	full, err := sp.View(ctx, tools.ViewSpec{ChildID: target, Entry: cursor})
	ck.Require().NoError(err, "entry=%s", cursor)
	ck.StrContains(full, f.long, "entry= must return the whole reply, not the page form; got %d bytes:\n%s", len(full), full)
	ck.StrContains(full, "\n", "entry= must keep real newlines")
	ck.False(strings.Contains(full, "⏎"), "entry= must not rewrite newlines; got:\n%s", full)
	ck.False(strings.Contains(full, "…(truncated)"), "entry= must not truncate; got:\n%s", full)
}

// TestAgentViewAuthorizesBeforeReading pins the gate's POSITION: authorize runs
// before any load, render or cursor parse, so a non-descendant learns nothing
// and cannot make the daemon read somebody else's conversation by guessing a
// cursor. The error-text assertions are what make that observable — a cursor
// parsed first would answer "no entry m:0.0", and a load first would answer
// "loading …'s conversation".
func TestAgentViewAuthorizesBeforeReading(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := agentViewDBFixtureFor(t)
	sp := newControllerSpawner(f.ctrl, "c_mine")
	ctx := context.Background()

	modes := []struct {
		name string
		spec tools.ViewSpec
	}{
		{"plain", tools.ViewSpec{}},
		{"before", tools.ViewSpec{Before: "m:0.0"}},
		{"after", tools.ViewSpec{After: "m:0.0"}},
		{"entry", tools.ViewSpec{Entry: "m:0.0"}},
		{"find", tools.ViewSpec{Find: "zebra"}},
	}
	for _, m := range modes {
		m.spec.ChildID = "c_stranger"
		_, err := sp.View(ctx, m.spec)
		ck.Require().Error(err, "%s view of a stranger must refuse", m.name)
		ck.StrContains(err.Error(), "descendant", "%s: want the not-a-descendant refusal; got %v", m.name, err)
		ck.False(strings.Contains(err.Error(), "entries"), "%s: the refusal must not leak the transcript; got %v", m.name, err)
		ck.False(strings.Contains(err.Error(), "no entry"), "%s: the cursor must not be parsed before authorize; got %v", m.name, err)
	}
}

// TestAgentViewRefusesCursorsOnANonDBChild covers the live/exited branch: a
// child with no stored conversation keeps the newest page, labelled with its
// source, and every cursor mode names why it cannot continue.
func TestAgentViewRefusesCursorsOnANonDBChild(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := spawnerFixture(t) // no pool: nothing to page
	ck.NoError(c.st.Update("c_grandchild", func(s *childstore.Session) {
		s.Kind = protocol.KindClaude
	}), "make c_grandchild a claude child with no conversation")
	sp := newControllerSpawner(c, "c_mine")
	ctx := context.Background()
	target := "c_grandchild"

	for _, spec := range []tools.ViewSpec{
		{ChildID: target, Before: "m:0.0"},
		{ChildID: target, After: "m:0.0"},
		{ChildID: target, Entry: "m:0.0"},
		{ChildID: target, Find: "x"},
	} {
		_, err := sp.View(ctx, spec)
		ck.Require().Error(err, "%+v must be refused", spec)
		ck.StrContains(err.Error(), "not pageable yet", "%+v must name why; got %v", spec, err)
		ck.StrContains(err.Error(), "exited", "%+v must name the source; got %v", spec, err)
	}

	out, err := sp.View(ctx, tools.ViewSpec{ChildID: target})
	ck.Require().NoError(err, "the plain call must still work on a non-db child")
	ck.True(strings.HasPrefix(out, "[exited child · newest entries only — not pageable]"),
		"the newest page must be labelled with its source; got:\n%s", out)
}

// TestAgentViewUserFaceIsPageable pins that the user credential's surface gets
// the same modes: a caller passing before= and having it ignored would be the
// silent dead end the paging modes remove.
func TestAgentViewUserFaceIsPageable(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := agentViewDBFixtureFor(t)
	us := newUserSpawner(f.ctrl, users.Identity{})
	ctx := context.Background()
	target := "c_grandchild"

	newest, err := us.View(ctx, tools.ViewSpec{ChildID: target})
	ck.Require().NoError(err, "plain user view")
	ck.StrContains(newest, "entries · showing", "got:\n%s", newest)

	older, err := us.View(ctx, tools.ViewSpec{ChildID: target, Before: markerCursor(t, newest, "before")})
	ck.Require().NoError(err, "before= on the user face must page, not refuse")
	ck.StrContains(older, "entries · showing", "got:\n%s", older)
	ck.False(strings.Contains(older, "paging is not available yet"), "the user face still refuses paging: %s", older)
}
