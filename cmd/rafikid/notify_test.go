// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/childstoredb"
	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/inboxdb"
	"go.graveland.dev/rafiki/pkg/notify"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// notifyFixture is a Controller over a scratch database: real child rows (the
// target lookup is SQL — child_sibling_name_live's predicate) and a real inbox
// store as the event buffer's accepter (a durable notification is a row).
type notifyFixture struct {
	ctrl *Controller
	pool *pgxpool.Pool
	st   *childstoredb.Store
	in   *inboxdb.Store
}

func newNotifyFixture(t *testing.T) *notifyFixture {
	t.Helper()
	pool := scratchPool(t) // skips without RAFIKI_TEST_DSN
	ctrl := newTestController(t)
	ctrl.children = childstoredb.New(pool)
	in := inboxdb.New(pool)
	// The production accepter — the inbox queue over the durable store — with
	// delivery stubbed out: these tests read the rows a notification left, they
	// never run a turn.
	ctrl.inbox = ctrl.newInboxQueue(in)
	ctrl.wireEventBuffer()
	ctrl.evbuf.SetFlush(func(string, string, []inbox.Inbound) {})
	return &notifyFixture{ctrl: ctrl, pool: pool, st: childstoredb.New(pool), in: in}
}

// seedNotifyOwner creates a real user row: conversations.child.owner_user_id is
// a foreign key, so an owner has to exist before a child can claim it.
func (f *notifyFixture) seedOwner(t *testing.T, prefix string) string {
	t.Helper()
	return seedControllerUser(t, f.pool, uniqueNotifyName(prefix))
}

// uniqueNotifyName is per-run, so nothing depends on a name an earlier run left
// behind in a database that was ever reused.
func uniqueNotifyName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// insertChild writes a live child: the row NameHolder resolves and, because a
// live child is in both, the in-memory session the inbox queue validates
// against. parent "" makes it top level; owner "" makes it ownerless
// (anonymous).
func (f *notifyFixture) insertChild(t *testing.T, childID, owner, parent, name string, status protocol.Status) {
	t.Helper()
	labels := map[string]string{}
	if parent != "" {
		labels[childstore.LabelParent] = parent
	}
	now := time.Now()
	assert.NewAborting(t).Require().NoError(f.st.Upsert(context.Background(), childstore.ChildRecord{
		ChildID:      childID,
		OwnerUserID:  owner,
		Kind:         protocol.KindFundi,
		Name:         name,
		Status:       string(status),
		SpawnedAt:    now,
		LastActivity: now,
		Labels:       labels,
	}), "upsert child %s", childID)
	f.ctrl.st.Insert(&childstore.Session{
		ChildID: childID, Kind: protocol.KindFundi, Name: name, Status: status,
		StartedAt: now, LastActivity: now, Labels: labels,
	})
}

// pending returns the child's pending inbox rows, oldest first.
func (f *notifyFixture) pending(t *testing.T, childID string) []inbox.Inbound {
	t.Helper()
	rows, err := f.in.Pending(context.Background(), childID)
	assert.NewAborting(t).Require().NoError(err, "read pending rows for %s", childID)
	return rows
}

var receivedRE = regexp.MustCompile(`"received":"([^"]*)"`)

// assertFragmentMatchesRender compares a stored fragment against a fresh
// notify.Render at the received time the fragment itself carries. Every other
// byte must match exactly, so the element shape, the escaping and the received
// format are all pinned.
func assertFragmentMatchesRender(t *testing.T, got, source, key string, fields map[string]string) {
	t.Helper()
	ck := assert.NewAborting(t)
	m := receivedRE.FindStringSubmatch(got)
	ck.Require().True(len(m) == 2, "stored fragment carries no received stamp: %q", got)
	received, err := time.Parse(time.RFC3339, m[1])
	ck.Require().NoError(err, "received stamp %q is not RFC3339", m[1])
	want, err := notify.Render(source, key, fields, received)
	ck.Require().NoError(err, "Render")
	ck.Eq(want, got, "stored fragment")
}

// requireControllerError asserts err is a *connectapi.ControllerError with the
// given protocol code (the Connect handler is what turns it into a status).
func requireControllerError(t *testing.T, err error, code, msgContains string) {
	t.Helper()
	ck := assert.NewAborting(t)
	ck.Require().Error(err, "want a %s refusal, got nil", code)
	var ce *connectapi.ControllerError
	ck.Require().True(errors.As(err, &ce), "error %v is not a *connectapi.ControllerError", err)
	ck.Eq(code, ce.Code, "error code (message: %s)", ce.Message)
	ck.StrContains(ce.Message, msgContains, "error message")
}

// The happy path: the named live top-level agent gets one pending prompt-mode
// row under the notification's own eventbuf source, carrying the rendered
// fragment.
func TestNotifyDeliversFragmentToNamedTopLevelAgent(t *testing.T) {
	ck := assert.NewAborting(t)
	f := newNotifyFixture(t)
	owner, childID := f.seedOwner(t, "owner-deliver"), "c_notify_deliver"
	name := uniqueNotifyName("deliver")
	f.insertChild(t, childID, owner, "", name, protocol.StatusIdle)

	fields := map[string]string{"repo": "rafiki", "status": "passed", "note": "<b>&</b>"}
	got, err := f.ctrl.Notify(t.Context(), owner, false, name, "ci", "run.1", fields)
	ck.Require().NoError(err, "Notify")
	ck.Eq(childID, got, "delivered child id")

	rows := f.pending(t, childID)
	ck.Require().Len(rows, 1, "pending rows for the target")
	ck.Eq("notify:ci", rows[0].Source, "row source")
	ck.Eq("run.1", rows[0].Key, "row coalesce key")
	ck.Eq(inbox.ModePrompt, rows[0].Mode, "row mode")
	assertFragmentMatchesRender(t, rows[0].Text, "ci", "run.1", fields)
}

// A name nobody live holds is NotFound, and the message names the name so the
// gateway's sender learns what to fix.
func TestNotifyUnknownTargetNotFound(t *testing.T) {
	f := newNotifyFixture(t)
	name := uniqueNotifyName("nobody")
	_, err := f.ctrl.Notify(t.Context(), f.seedOwner(t, "owner-unknown"), false, name, "ci", "", nil)
	requireControllerError(t, err, protocol.ErrNotFound, fmt.Sprintf("no live top-level agent named %q", name))
}

// Only TOP-LEVEL children are addressable: a subagent named x is not the agent
// named x, and resolving by name must never reach down the tree.
func TestNotifyIgnoresNonTopLevel(t *testing.T) {
	ck := assert.NewAborting(t)
	f := newNotifyFixture(t)
	owner := f.seedOwner(t, "owner-nested")
	name := uniqueNotifyName("nested")
	f.insertChild(t, "c_notify_nested_parent", owner, "", uniqueNotifyName("parent"), protocol.StatusStreaming)
	f.insertChild(t, "c_notify_nested", owner, "c_notify_nested_parent", name, protocol.StatusStreaming)

	_, err := f.ctrl.Notify(t.Context(), owner, false, name, "ci", "", nil)
	requireControllerError(t, err, protocol.ErrNotFound, "no live top-level agent named")

	ck.Empty(f.pending(t, "c_notify_nested"), "pending rows for the subagent")
}

// The owner is the caller's, taken from the identity: another owner's agent of
// the same name is invisible, and an anonymous caller only reaches the
// ownerless rows.
func TestNotifyOwnerScoped(t *testing.T) {
	ck := assert.NewAborting(t)
	f := newNotifyFixture(t)
	name := uniqueNotifyName("shared")
	ownerA, ownerB := f.seedOwner(t, "owner-a"), f.seedOwner(t, "owner-b")
	f.insertChild(t, "c_notify_owner_a", ownerA, "", name, protocol.StatusIdle)

	_, err := f.ctrl.Notify(t.Context(), ownerB, false, name, "ci", "", nil)
	requireControllerError(t, err, protocol.ErrNotFound, "no live top-level agent named")

	_, err = f.ctrl.Notify(t.Context(), "", true, name, "ci", "", nil)
	requireControllerError(t, err, protocol.ErrNotFound, "no live top-level agent named")

	got, err := f.ctrl.Notify(t.Context(), ownerA, false, name, "ci", "", nil)
	ck.Require().NoError(err, "the owner's own agent")
	ck.Eq("c_notify_owner_a", got, "resolved child")

	// The ownerless row is reachable only by the anonymous caller — and still
	// invisible to a named owner.
	f.insertChild(t, "c_notify_owner_none", "", "", name, protocol.StatusIdle)
	got, err = f.ctrl.Notify(t.Context(), "", true, name, "ci", "", nil)
	ck.Require().NoError(err, "anonymous Notify")
	ck.Eq("c_notify_owner_none", got, "anonymous resolved child")

	_, err = f.ctrl.Notify(t.Context(), ownerB, false, name, "ci", "", nil)
	requireControllerError(t, err, protocol.ErrNotFound, "no live top-level agent named")
}

// A non-anonymous caller with an EMPTY user id must not be resolved as the
// NULL owner: ("", false) is not the unix socket's local trust, and treating it
// as ownerless would let such a connection notify another owner's namespace.
// The refusal happens before the store is touched.
func TestNotifyEmptyOwnerNonAnonymousRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	f := newNotifyFixture(t)
	const childID = "c_notify_empty_owner"
	name := uniqueNotifyName("emptyowner")
	// An ownerless agent by that name: resolving ("", false) as the NULL owner
	// would find it and succeed, so a success here means the guard never ran.
	f.insertChild(t, childID, "", "", name, protocol.StatusIdle)
	st := &countingChildStore{ChildStore: f.ctrl.children}
	f.ctrl.children = st

	_, err := f.ctrl.Notify(t.Context(), "", false, name, "ci", "", nil)
	requireControllerError(t, err, protocol.ErrInvalidArgs, "requires an owner")
	ck.Eq(0, st.nameHolderCalls(), "NameHolder calls: the refusal must precede the lookup")
	ck.Empty(f.pending(t, childID), "pending rows after a refused notification")

	// The SAME empty owner is the unix socket's local trust when the caller is
	// anonymous, and still reaches the ownerless agent.
	got, err := f.ctrl.Notify(t.Context(), "", true, name, "ci", "", nil)
	ck.Require().NoError(err, "anonymous Notify")
	ck.Eq(childID, got, "anonymous resolved child")
}

// countingChildStore records NameHolder calls, so a refusal can be shown to
// happen BEFORE the lookup rather than merely to return the right code.
type countingChildStore struct {
	childstore.ChildStore

	mu    sync.Mutex
	calls int
}

func (s *countingChildStore) NameHolder(ctx context.Context, ownerUserID, parentID, name string) (string, string, bool, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.ChildStore.NameHolder(ctx, ownerUserID, parentID, name)
}

func (s *countingChildStore) nameHolderCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// A field violating pkg/notify's rules is refused as invalid arguments, never
// truncated, and nothing reaches the target's queue.
func TestNotifyInvalidFieldsRefused(t *testing.T) {
	ck := assert.NewAborting(t)
	f := newNotifyFixture(t)
	owner, childID := f.seedOwner(t, "owner-invalid"), "c_notify_invalid"
	name := uniqueNotifyName("invalid")
	f.insertChild(t, childID, owner, "", name, protocol.StatusIdle)

	_, err := f.ctrl.Notify(t.Context(), owner, false, name, "ci", "",
		map[string]string{"note": "before\x00after"})
	requireControllerError(t, err, protocol.ErrInvalidArgs, "NUL")

	ck.Empty(f.pending(t, childID), "pending rows after a refused notification")

	_, err = f.ctrl.Notify(t.Context(), owner, false, name, "Not A Source", "", nil)
	requireControllerError(t, err, protocol.ErrInvalidArgs, "invalid source")
}

// A durable accept that fails is reported as Unavailable and leaves nothing
// behind: the sender retries, so a fragment queued here would double-deliver on
// the retry that succeeds.
func TestNotifyStoreFailureUnavailable(t *testing.T) {
	ck := assert.NewAborting(t)
	f := newNotifyFixture(t)
	owner, childID := f.seedOwner(t, "owner-storefail"), "c_notify_storefail"
	name := uniqueNotifyName("storefail")
	f.insertChild(t, childID, owner, "", name, protocol.StatusIdle)

	clk := clock.NewFakeClock(time.Unix(0, 0))
	acc := &failingNotifyAccepter{err: errors.New("inbox store unavailable")}
	var mu sync.Mutex
	var flushes int
	// Replace the buffer so the debounce is observable on a fake clock and a
	// flush is recorded instead of being delivered.
	b := eventbuf.New(eventbuf.Config{Debounce: time.Second, MaxWait: time.Minute}, clk)
	b.SetAccepter(acc)
	b.SetFlush(func(string, string, []inbox.Inbound) {
		mu.Lock()
		defer mu.Unlock()
		flushes++
	})
	b.SetBusy(func(string) bool { return false })
	f.ctrl.evbuf = b

	_, err := f.ctrl.Notify(t.Context(), owner, false, name, "ci", "", map[string]string{"k": "v"})
	requireControllerError(t, err, protocol.ErrUnavailable, "notification not stored")
	// A ControllerError carries the code and message a Connect error is built
	// from, not a wrapped cause, so the store's own text is what survives.
	ck.StrContains(err.Error(), "inbox store unavailable", "the store's error in the refusal")

	ck.Eq(1, acc.count(), "accepter calls")
	ck.False(b.Pending(childID), "Pending after a failed durable accept")

	clk.Advance(2 * time.Minute)
	mu.Lock()
	got := flushes
	mu.Unlock()
	ck.Eq(0, got, "flushes after the debounce; a failed push armed an orphan")
	ck.Empty(f.pending(t, childID), "durable rows after a failed store")
}

// A killed target keeps its name and its queue: Kill exits the child and leaves
// its row live, so the notification is still accepted and still pending — not
// dropped because the target is not currently running.
func TestNotifyKilledTargetKeepsRow(t *testing.T) {
	ck := assert.NewAborting(t)
	f := newNotifyFixture(t)
	owner, childID := f.seedOwner(t, "owner-killed"), "c_notify_killed"
	name := uniqueNotifyName("killed")
	f.insertChild(t, childID, owner, "", name, protocol.StatusIdle)
	// A native child has no process to signal, so Kill takes its end-the-child
	// branch and leaves exactly what a real kill leaves: an exited row.
	ck.Require().NoError(f.ctrl.st.Update(childID, func(s *childstore.Session) { s.Native = true }),
		"mark the target native")

	_, err := f.ctrl.Kill(t.Context(), childID, time.Second, time.Second)
	ck.Require().NoError(err, "Kill")
	snap, ok := f.ctrl.st.Get(childID)
	ck.Require().True(ok, "the killed child left the live store")
	ck.Eq(protocol.StatusExited, snap.Status, "status after Kill")

	var closedAt *time.Time
	ck.Require().NoError(f.pool.QueryRow(context.Background(),
		`SELECT closed_at FROM conversations.child WHERE child_id = $1`, childID).Scan(&closedAt),
		"read the killed child's row")
	ck.Nil(closedAt, "Kill must not close the row; closing a child is what frees its name")

	got, err := f.ctrl.Notify(t.Context(), owner, false, name, "ci", "", map[string]string{"k": "v"})
	ck.Require().NoError(err, "Notify a killed target")
	ck.Eq(childID, got, "delivered child id")

	rows := f.pending(t, childID)
	ck.Require().Len(rows, 1, "pending rows for the killed target")
	ck.Eq("notify:ci", rows[0].Source, "row source")
	assertFragmentMatchesRender(t, rows[0].Text, "ci", "", map[string]string{"k": "v"})
}

// Two notifications on the same key coalesce to the LAST text — a webhook's
// update supersedes its create — while notifications with no key all survive.
func TestNotifyCoalescesByKey(t *testing.T) {
	ck := assert.NewAborting(t)
	f := newNotifyFixture(t)
	owner, childID := f.seedOwner(t, "owner-coalesce"), "c_notify_coalesce"
	name := uniqueNotifyName("coalesce")
	f.insertChild(t, childID, owner, "", name, protocol.StatusIdle)

	push := func(key, value string) {
		t.Helper()
		_, err := f.ctrl.Notify(t.Context(), owner, false, name, "ci", key, map[string]string{"event": value})
		ck.Require().NoError(err, "Notify %s/%s", key, value)
	}
	push("pr-7", "opened")
	push("pr-7", "merged")
	push("", "first-unkeyed")
	push("", "second-unkeyed")

	batches := inbox.Coalesce(f.pending(t, childID), inbox.BatchConfig{})
	ck.Require().Len(batches, 1, "batches for the target")
	ck.Eq("notify:ci", batches[0].Source, "batch source")
	ck.Require().Len(batches[0].Frags, 3, "effective fragments")

	// The keyed fragment is the second write, in the keyed position; the two
	// unkeyed ones follow in arrival order.
	assertFragmentMatchesRender(t, batches[0].Frags[0], "ci", "pr-7", map[string]string{"event": "merged"})
	assertFragmentMatchesRender(t, batches[0].Frags[1], "ci", "", map[string]string{"event": "first-unkeyed"})
	assertFragmentMatchesRender(t, batches[0].Frags[2], "ci", "", map[string]string{"event": "second-unkeyed"})
}

// A notification whose rendered fragment is bigger than the event buffer's
// per-fragment cap is refused as INVALID ARGUMENTS, never truncated and never
// Unavailable: the gateway's sender retries an Unavailable forever, and the
// cap is the sender's to get under. Three 4096-byte values are inside
// pkg/notify's own rules (each under its per-value cap, and the encoded JSON
// under 16384) and still exceed an 8192-byte fragment cap, so they reach the
// buffer and are refused there.
func TestNotifyOversizeFragmentInvalidArgs(t *testing.T) {
	ck := assert.NewAborting(t)
	f := newNotifyFixture(t)
	owner, childID := f.seedOwner(t, "owner-oversize"), "c_notify_oversize"
	name := uniqueNotifyName("oversize")
	f.insertChild(t, childID, owner, "", name, protocol.StatusIdle)

	const cap = 8192
	b := eventbuf.New(eventbuf.Config{Debounce: time.Second, MaxBytesPerFrag: cap}, clock.NewFakeClock(time.Unix(0, 0)))
	acc := &failingNotifyAccepter{err: errors.New("the buffer must refuse before the store")}
	b.SetAccepter(acc)
	b.SetFlush(func(string, string, []inbox.Inbound) {})
	b.SetBusy(func(string) bool { return false })
	f.ctrl.evbuf = b

	big := strings.Repeat("z", 4096)
	fields := map[string]string{"a": big, "b": big, "c": big}
	// The fragment itself is what has to be over the cap, not just the JSON.
	rendered, err := notify.Render("ci", "", fields, time.Now())
	ck.Require().NoError(err, "Render must accept these fields; they are inside pkg/notify's own caps")
	ck.Require().True(len(rendered) > cap, "fixture fragment is %d bytes; it must exceed the %d-byte cap", len(rendered), cap)

	_, err = f.ctrl.Notify(t.Context(), owner, false, name, "ci", "", fields)
	requireControllerError(t, err, protocol.ErrInvalidArgs, "per-fragment limit")
	ck.StrContains(err.Error(), fmt.Sprint(cap), "the refusal must name the buffer's limit")
	ck.StrContains(err.Error(), fmt.Sprint(len(rendered)), "the refusal must name the rendered size")

	ck.Eq(0, acc.count(), "accepter calls; an over-cap notification must never reach the store")
	ck.False(b.Pending(childID), "Pending after an over-cap notification")
	ck.Empty(f.pending(t, childID), "durable rows after an over-cap notification")
}

// failingNotifyAccepter refuses every durable accept, which is the only way to
// observe that Notify reports the failure instead of queueing an orphan.
type failingNotifyAccepter struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (a *failingNotifyAccepter) Accept(context.Context, inbox.Inbound) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	return "", a.err
}

func (a *failingNotifyAccepter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}
