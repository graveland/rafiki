// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// claudeDeliveryHarness drives two real things and nothing hand-built: the
// OnUserReplay callback installChildHooks installs (exactly what pkg/child
// fires from its stdout goroutine), and the delivery funnel deliverInbox uses.
// The write itself is a seam, because a real claude child's echo arrives on its
// own goroutine and the ordering under test — the echo landing BEFORE the write
// returns — can only be produced by a write that calls the hook.
type claudeDeliveryHarness struct {
	c      *Controller
	events <-chan *rafikiv1.Event
	evlog  *eventlog.Memory
	store  inbox.Store
}

func newClaudeDeliveryHarness(t *testing.T, childIDs ...string) *claudeDeliveryHarness {
	t.Helper()
	reg := nativebus.New()
	events, cancel := reg.SubscribeAll()
	t.Cleanup(cancel)
	evlog := eventlog.NewMemory()
	store := inbox.NewMemory()
	c := &Controller{st: childstore.New(), cm: newChildManager(), native: reg, evlog: evlog}
	c.inbox = c.newInboxQueue(store)
	for _, id := range childIDs {
		c.st.Insert(&childstore.Session{ChildID: id, Kind: protocol.KindClaude, Status: protocol.StatusIdle})
	}
	return &claudeDeliveryHarness{c: c, events: events, evlog: evlog, store: store}
}

// launch runs the production launch wiring for one claude child and returns the
// OnUserReplay callback it installed plus the instance the generation was
// attached to — the same three steps Spawn, Resume and RespawnChild take:
// installChildHooks mints the generation, child.Spawn produces the instance,
// and attach binds one to the other.
//
// The instance is a zero-value *child.Child standing in for the process: these
// tests never run one, and the registry keys on its identity alone.
func (h *claudeDeliveryHarness) launch(t *testing.T, childID string) (func(string), *child.Child) {
	t.Helper()
	spec := child.SpawnSpec{ChildID: childID}
	gen := h.c.installChildHooks(&spec, protocol.KindClaude)
	if spec.OnUserReplay == nil {
		t.Fatalf("installChildHooks installed no OnUserReplay for claude child %s", childID)
	}
	if gen == 0 {
		t.Fatalf("installChildHooks minted no launch generation for claude child %s", childID)
	}
	ch := &child.Child{}
	h.c.claudeDeliveries.attach(ch, gen)
	return spec.OnUserReplay, ch
}

// writeEcho replaces the write with one that behaves like a claude child
// accepting the message: it reads the frame's delivery uuid and fires the
// launch's hook before returning. Every frame written is recorded in order.
func (h *claudeDeliveryHarness) writeEcho(hook func(string), written *[]string, beforeEcho func(frame json.RawMessage)) {
	h.c.sendFrameFn = func(_ string, frame json.RawMessage) error {
		u := frameDeliveryUUID(frame)
		*written = append(*written, u)
		if beforeEcho != nil {
			beforeEcho(frame)
		}
		if hook != nil && u != "" {
			hook(u)
		}
		return nil
	}
}

// send persists and delivers one direct prompt through the real funnel.
func (h *claudeDeliveryHarness) send(t *testing.T, childID, text string) string {
	t.Helper()
	id, err := h.c.acceptAndDeliver(context.Background(), inbox.Inbound{
		ChildID: childID, Mode: inbox.ModePrompt, Text: text,
	})
	if err != nil {
		t.Fatalf("acceptAndDeliver(%s, %q): %v", childID, text, err)
	}
	return id
}

func (h *claudeDeliveryHarness) drain() []*rafikiv1.Event { return drainEvents(h.events) }

// drainEvents empties a native-bus subscription without waiting: every publish
// this package makes is synchronous into the subscriber's channel.
func drainEvents(events <-chan *rafikiv1.Event) []*rafikiv1.Event {
	var got []*rafikiv1.Event
	for {
		select {
		case ev := <-events:
			got = append(got, ev)
		default:
			return got
		}
	}
}

func (h *claudeDeliveryHarness) pending(t *testing.T, childID string) []inbox.Inbound {
	t.Helper()
	rows, err := h.store.Pending(context.Background(), childID)
	if err != nil {
		t.Fatalf("Pending(%s): %v", childID, err)
	}
	return rows
}

// frameDeliveryUUID reads the delivery uuid a frame carries, or "".
func frameDeliveryUUID(frame json.RawMessage) string {
	var f struct {
		DeliveryUUID string `json:"delivery_uuid"`
	}
	// A frame that is not JSON is a bug the test's own uuid assertion catches;
	// this helper is called from inside a write, where failing the test from
	// another goroutine's stack would hide which frame was wrong.
	if err := json.Unmarshal(frame, &f); err != nil {
		return ""
	}
	return f.DeliveryUUID
}

// receipts lists the inbox row ids the published message_delivered events name,
// in publish order.
func receipts(evs []*rafikiv1.Event) []string {
	var out []string
	for _, ev := range evs {
		if p := ev.GetMessageDelivered(); p != nil {
			out = append(out, p.GetMessageId())
		}
	}
	return out
}

// assertUUIDv7 pins the identity rule the plan chose: a time-ordered v7, never
// a hash of the text (two deliberate sends of the same words must be two
// deliveries).
func assertUUIDv7(t *testing.T, s string) {
	t.Helper()
	u, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("delivery uuid %q is not a uuid: %v", s, err)
	}
	if got := int(u.Version()); got != 7 {
		t.Fatalf("delivery uuid %q is version %d, want 7", s, got)
	}
	if u.Variant() != uuid.RFC4122 {
		t.Fatalf("delivery uuid %q is not RFC4122: %v", s, u.Variant())
	}
}

// The echo can land while the write is still in progress — the child's stdout
// goroutine is independent of the daemon's write — so the receipt must not
// depend on anything that happens after sendFrame returns. Here the write fires
// the hook itself, and the row is still pending at that moment: the receipt is
// published anyway, and it goes through the durable path.
func TestClaudeDeliverySynchronousEchoPublishesDuringTheWrite(t *testing.T) {
	ck := assert.NewAborting(t)
	h := newClaudeDeliveryHarness(t, "c_deliver")
	hook, _ := h.launch(t, "c_deliver")

	var written []string
	pendingAtEcho := -1
	h.writeEcho(hook, &written, func(json.RawMessage) {
		pendingAtEcho = len(h.pending(t, "c_deliver"))
	})

	rowID := h.send(t, "c_deliver", "hello")

	ck.Len(written, 1, "frames written = %v, want one", written)
	assertUUIDv7(t, written[0])
	ck.Eq(1, pendingAtEcho, "the echo must arrive before the row is consumed; pending rows at echo")
	ck.EqDiff([]string{rowID}, receipts(h.drain()), "receipts")

	// Durable: appended to the child's log with an ordinal, which is what makes
	// the receipt survive a resume.
	recs, err := h.evlog.Read(context.Background(), "c_deliver", -1, 10)
	ck.Require().NoError(err, "event log read")
	ck.Len(recs, 1, "durable receipts")
	stored, err := recs[0].Decode()
	ck.Require().NoError(err, "decode the stored receipt")
	ck.Eq(rowID, stored.GetMessageDelivered().GetMessageId(), "durable receipt row id")
}

// Consume-on-write is unchanged: the row is consumed when the frame is written,
// and the echo that arrives afterwards still names it. Nothing here may move a
// claude row to 'sent' or wait for the capture.
func TestClaudeDeliveryDelayedEchoAfterTheRowIsConsumed(t *testing.T) {
	ck := assert.NewAborting(t)
	h := newClaudeDeliveryHarness(t, "c_deliver")
	hook, _ := h.launch(t, "c_deliver")

	var written []string
	h.writeEcho(nil, &written, nil) // no echo during the write
	rowID := h.send(t, "c_deliver", "hello")

	ck.Len(written, 1, "frames written")
	ck.Empty(h.pending(t, "c_deliver"), "a claude row is consumed on the write, not held for the ack")
	ck.Empty(receipts(h.drain()), "a receipt before the echo would be a receipt for a write, not an acceptance")

	hook(written[0])

	ck.EqDiff([]string{rowID}, receipts(h.drain()), "receipts after the echo")
}

// Two deliberate sends of the SAME text are two deliveries: the uuid is minted
// per delivery, so neither the text nor a content hash can collapse them.
func TestClaudeDeliveryDuplicateTextsGetDistinctUUIDs(t *testing.T) {
	ck := assert.NewAborting(t)
	h := newClaudeDeliveryHarness(t, "c_deliver")
	hook, _ := h.launch(t, "c_deliver")

	var written []string
	h.writeEcho(hook, &written, nil)
	first := h.send(t, "c_deliver", "same words")
	second := h.send(t, "c_deliver", "same words")

	ck.Len(written, 2, "frames written")
	assertUUIDv7(t, written[0])
	assertUUIDv7(t, written[1])
	ck.False(written[0] == written[1], "two sends of the same text shared the uuid %q", written[0])
	ck.False(first == second, "the two sends must be two inbox rows")

	// Each echo retired its own message, in the order they were accepted.
	ck.EqDiff([]string{first, second}, receipts(h.drain()), "receipts")
}

// An echo only ever names a message written to THIS child's CURRENT launch. An
// unknown uuid, another child's uuid, and a uuid from a launch this daemon has
// since replaced all publish nothing.
func TestClaudeDeliveryIgnoresForeignChildAndRetiredGeneration(t *testing.T) {
	ck := assert.NewAborting(t)
	h := newClaudeDeliveryHarness(t, "c_one", "c_two")
	hookOne, instOne := h.launch(t, "c_one")
	hookTwo, _ := h.launch(t, "c_two")

	var written []string
	h.writeEcho(nil, &written, nil)
	h.send(t, "c_one", "for one")
	h.send(t, "c_two", "for two")
	ck.Len(written, 2, "frames written")

	// A uuid that belongs to nobody.
	hookOne("0198e5f2-9c3a-7def-8a1b-4c2d9e0f1a2b")
	ck.Empty(receipts(h.drain()), "an unknown uuid published a receipt")

	// c_two's uuid, reported by c_one's callback.
	hookOne(written[1])
	ck.Empty(receipts(h.drain()), "another child's uuid published a receipt")

	// The retired generation: a new launch replaces the one that minted it.
	hookCurrent, _ := h.launch(t, "c_one")
	hookOne(written[0])
	ck.Empty(receipts(h.drain()), "a retired launch's echo published a receipt")

	// The current launch still works, for its own deliveries.
	var after []string
	h.writeEcho(hookCurrent, &after, nil)
	rowID := h.send(t, "c_one", "after the relaunch")
	ck.Len(after, 1, "frames written after the relaunch")
	ck.EqDiff([]string{rowID}, receipts(h.drain()), "receipts after the relaunch")

	// c_two's own launch is untouched by any of c_one's relaunches.
	_ = instOne
	var two []string
	h.writeEcho(hookTwo, &two, nil)
	rowTwo := h.send(t, "c_two", "still its own child")
	ck.Len(two, 1, "frames written for c_two")
	ck.EqDiff([]string{rowTwo}, receipts(h.drain()), "c_two's receipt")
}

// A write that definitively failed leaves its row pending for the retry, so its
// uuid must be forgotten: a late echo for a frame the child never received
// would otherwise publish a receipt for a message it never got.
func TestClaudeDeliveryWriteFailureDropsOnlyThatDelivery(t *testing.T) {
	ck := assert.NewAborting(t)
	h := newClaudeDeliveryHarness(t, "c_deliver")
	hook, _ := h.launch(t, "c_deliver")

	var written []string
	h.c.sendFrameFn = func(_ string, frame json.RawMessage) error {
		written = append(written, frameDeliveryUUID(frame))
		return errors.New("stdin closed")
	}

	_, err := h.c.acceptAndDeliver(context.Background(), inbox.Inbound{
		ChildID: "c_deliver", Mode: inbox.ModePrompt, Text: "hello",
	})
	ck.Require().NoError(err, "a failed delivery is not the sender's error: the message is durably accepted")
	ck.Len(written, 1, "frames attempted")
	assertUUIDv7(t, written[0])
	ck.Len(h.pending(t, "c_deliver"), 1, "the row must stay pending for the retry")

	hook(written[0])
	ck.Empty(receipts(h.drain()), "a failed write's uuid published a receipt")

	// The retry mints its own uuid and is acknowledged normally.
	var retried []string
	h.writeEcho(hook, &retried, nil)
	rows := h.pending(t, "c_deliver")
	ck.Require().Len(rows, 1, "pending rows for the retry")
	if err := h.c.inbox.Deliver(context.Background(), "c_deliver", ""); err != nil {
		t.Fatalf("retry delivery: %v", err)
	}
	ck.Len(retried, 1, "frames on the retry")
	ck.False(retried[0] == written[0], "the retry reused the failed delivery's uuid")
	ck.EqDiff([]string{rows[0].ID}, receipts(h.drain()), "the retry's receipt")
}

// Teardown retires the launch's pending receipts and nothing else: the next
// launch (a resume) starts a new generation whose deliveries are tracked again.
func TestClaudeDeliveryTeardownRetiresOnlyItsGeneration(t *testing.T) {
	ck := assert.NewAborting(t)
	h := newClaudeDeliveryHarness(t, "c_deliver")
	hook, inst := h.launch(t, "c_deliver")

	var written []string
	h.writeEcho(nil, &written, nil)
	h.send(t, "c_deliver", "hello")
	ck.Len(written, 1, "frames written")

	// Exactly what handleChildExit does: retire the generation the instance was
	// launched under.
	h.c.claudeDeliveries.endLaunch("c_deliver", inst)

	hook(written[0])
	ck.Empty(receipts(h.drain()), "a receipt was published after teardown")

	// No launch is current any more, so a delivery carries no uuid at all —
	// nothing could acknowledge it.
	var afterTeardown []string
	h.writeEcho(hook, &afterTeardown, nil)
	h.send(t, "c_deliver", "after teardown")
	ck.EqDiff([]string{""}, afterTeardown, "a delivery with no live launch must carry no uuid")
}

// A resume is a new launch of the same child: the previous generation's
// outstanding uuid is retired, and the resumed child's deliveries are tracked
// under the new one.
func TestClaudeDeliveryResumeRetiresThePreviousLaunch(t *testing.T) {
	ck := assert.NewAborting(t)
	h := newClaudeDeliveryHarness(t, "c_deliver")
	beforeResume, _ := h.launch(t, "c_deliver")

	var written []string
	h.writeEcho(nil, &written, nil)
	h.send(t, "c_deliver", "hello")
	ck.Len(written, 1, "frames written before the resume")

	// The resume: a fresh SpawnSpec, hence a fresh generation.
	afterResume, _ := h.launch(t, "c_deliver")
	beforeResume(written[0])
	ck.Empty(receipts(h.drain()), "the pre-resume uuid published a receipt after the resume")

	var resumed []string
	h.writeEcho(afterResume, &resumed, nil)
	rowID := h.send(t, "c_deliver", "after the resume")
	ck.Len(resumed, 1, "frames written after the resume")
	ck.False(resumed[0] == written[0], "the resumed launch reused the retired uuid")
	ck.EqDiff([]string{rowID}, receipts(h.drain()), "receipts after the resume")
}

// A callback from a retired launch racing a new launch never names the wrong
// row and never publishes twice, and once the new launch owns the child it is
// dead for good.
//
// The two goroutines are released together by a barrier, so the interleaving is
// the scheduler's: whichever way it falls, the old uuid either resolved while
// its launch was still current — a real acceptance, one receipt — or found the
// generation retired and published nothing. Both are correct; two receipts, or a
// receipt for another row, are not.
func TestClaudeDeliveryOldLaunchCallbackRacingANewLaunchPublishesNothing(t *testing.T) {
	ck := assert.NewAborting(t)
	h := newClaudeDeliveryHarness(t, "c_deliver")
	oldHook, _ := h.launch(t, "c_deliver")

	var written []string
	h.writeEcho(nil, &written, nil)
	rowID := h.send(t, "c_deliver", "hello")
	ck.Require().Len(written, 1, "frames written")
	uuidUnderTest := written[0]

	// The SpawnSpec is built HERE, on the test goroutine: the goroutine below
	// runs the production launch wiring only, because a t.Fatalf there would
	// runtime.Goexit that goroutine and hang wg.Wait until the package times
	// out instead of reporting the cause.
	resumeSpec := child.SpawnSpec{ChildID: "c_deliver"}

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		oldHook(uuidUnderTest)
	}()
	go func() {
		defer wg.Done()
		<-start
		gen := h.c.installChildHooks(&resumeSpec, protocol.KindClaude) // the new launch
		h.c.claudeDeliveries.attach(&child.Child{}, gen)
	}()
	close(start)
	wg.Wait()

	// No sleep: a receipt is published synchronously inside the callback, and
	// the barrier has already joined both goroutines.
	got := receipts(h.drain())
	ck.True(len(got) <= 1, "the racing callbacks published %d receipts, want at most one", len(got))
	if len(got) == 1 {
		ck.Eq(rowID, got[0], "a racing callback named a row other than the delivery's")
	}

	// Now the new launch owns the child, so the old callback can never publish
	// again — whatever the race above resolved to.
	oldHook(uuidUnderTest)
	ck.Empty(receipts(h.drain()), "a retired launch's callback published a receipt after the new launch took over")
}

// A Resume can start while the previous exit is still finishing: MarkExited
// runs near the TOP of handleChildExit and the receipt teardown near the
// bottom, and resumeClaimed only asks that the row read exited. So the exit must
// retire the generation ITS instance was launched under, never whatever is
// current — retiring the successor's would silently stop every receipt for the
// resumed launch, with no error anywhere.
func TestClaudeDeliveryStaleExitDoesNotRetireANewLaunch(t *testing.T) {
	ck := assert.NewAborting(t)
	h := newClaudeDeliveryHarness(t, "c_deliver")
	oldHook, oldInst := h.launch(t, "c_deliver")

	var written []string
	h.writeEcho(nil, &written, nil)
	h.send(t, "c_deliver", "hello")
	ck.Require().Len(written, 1, "frames written")

	// The Resume: a new generation for the same child, minted while the old
	// exit has not run its teardown yet.
	newHook, newInst := h.launch(t, "c_deliver")

	// The old exit now finishes, exactly as handleChildExit does.
	h.c.claudeDeliveries.endLaunch("c_deliver", oldInst)

	// The resumed launch still owns the child: a delivery is tracked and its
	// receipt publishes.
	var resumed []string
	h.writeEcho(newHook, &resumed, nil)
	resumedRow := h.send(t, "c_deliver", "after the resume")
	ck.Require().Len(resumed, 1, "frames written for the resumed launch")
	assertUUIDv7(t, resumed[0])
	ck.EqDiff([]string{resumedRow}, receipts(h.drain()), "the resumed launch's receipt")

	// And the dead launch's own outstanding uuid is gone.
	oldHook(written[0])
	ck.Empty(receipts(h.drain()), "the dead launch's uuid published a receipt")

	// The successor's teardown still works.
	h.c.claudeDeliveries.endLaunch("c_deliver", newInst)
	_, live := h.c.claudeDeliveries.register("c_deliver", "0198e5f2-9c3a-7def-8a1b-4c2d9e0f1a2b", "row")
	ck.False(live, "the successor's own teardown left its generation live")
}

// A spawn that never produced a Child is torn down by abortSpawn, which is not
// handleChildExit: without its own retirement the generation installChildHooks
// minted would stay in the map for a child that no longer exists.
func TestClaudeDeliveryAbortedSpawnRetiresItsGeneration(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)

	// The child.Spawn FAILED, so there is no instance: the generation is
	// retired by "current", which is safe only because the id was minted for
	// this spawn moments ago.
	spec := child.SpawnSpec{ChildID: "c_abort_nil"}
	ck.Require().True(ctrl.installChildHooks(&spec, protocol.KindClaude) != 0, "generation")
	_, ok := ctrl.claudeDeliveries.register("c_abort_nil", "uuid-1", "row-1")
	ck.Require().True(ok, "register")
	ctrl.abortSpawn("c_abort_nil", nil)
	ck.Eq(0, ctrl.claudeDeliveries.entryCount("c_abort_nil"), "entries after abortSpawn")
	_, live := ctrl.claudeDeliveries.register("c_abort_nil", "uuid-2", "row-2")
	ck.False(live, "abortSpawn left a refused spawn's generation live")

	// The child.Spawn SUCCEEDED and a later step refused the spawn: the
	// generation is retired through the instance it was attached to. A real
	// spawned child, because abortSpawn shuts the process down.
	childID := spawnTestChild(t, ctrl, nil)
	inst, liveChild := ctrl.cm.Get(childID)
	ck.Require().True(liveChild, "the spawned child is not live")
	_, ok = ctrl.claudeDeliveries.register(childID, "uuid-3", "row-3")
	ck.Require().True(ok, "register")
	ctrl.abortSpawn(childID, inst)
	ck.Eq(0, ctrl.claudeDeliveries.entryCount(childID), "entries after abortSpawn")
	_, live = ctrl.claudeDeliveries.register(childID, "uuid-4", "row-4")
	ck.False(live, "abortSpawn left the aborted instance's generation live")
}

// handleChildExit is where BOTH registry call sites live, and neither is
// reachable from the pure functions above: deleting either call leaves those
// tests green. This drives the real exit path — a spawned child killed through
// the controller, so monitorChild calls handleChildExit — with a delivery
// registered against the live launch.
func TestClaudeDeliveryExitRetiresTheLaunchGeneration(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	childID := spawnTestChild(t, ctrl, nil)

	_, ok := ctrl.claudeDeliveries.register(childID, "0198e5f2-9c3a-7def-8a1b-4c2d9e0f1a2b", "row_1")
	ck.Require().True(ok, "a spawned claude child has no launch generation")
	ck.Eq(1, ctrl.claudeDeliveries.entryCount(childID), "registered entries")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := ctrl.Kill(ctx, childID, 5*time.Second, 5*time.Second); err != nil {
		t.Fatalf("Kill(%s): %v", childID, err)
	}
	ck.Require().True(waitForChildRemoval(ctrl.cm, childID, 15*time.Second),
		"handleChildExit never finished for %s", childID)

	ck.Eq(0, ctrl.claudeDeliveries.entryCount(childID),
		"handleChildExit left the launch's outstanding receipts registered")
	_, live := ctrl.claudeDeliveries.register(childID, "another", "row_2")
	ck.False(live, "handleChildExit left the child's launch generation live")
}

// The other handleChildExit call site: a claude child that died at launch
// publishes the binary's own words, with the upgrade hint when they name a
// rejected flag. Driven through handleChildExit, not the pure builder, so
// deleting the publish leaves this red.
func TestClaudeDeliveryExitPublishesARejectedFlag(t *testing.T) {
	ck := assert.NewAborting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: this test needs a real process")
	}
	ctrl := newTestController(t)
	dir := t.TempDir()
	binary := filepath.Join(dir, "old-claude.sh")
	script := "#!/bin/sh\necho \"error: unknown option '--replay-user-messages'\" >&2\nexit 1\n"
	ck.NoError(os.WriteFile(binary, []byte(script), 0o700), "write the fake binary")

	const childID = "c_old_claude"
	ch, err := child.Spawn(context.Background(), child.SpawnSpec{
		ChildID:  childID,
		Cwd:      dir,
		PiBinary: "/bin/sh",
		Argv:     []string{binary},
		Provider: child.ClaudeProvider{},
	})
	ck.Require().NoError(err, "Spawn")
	<-ch.Done()

	ctrl.st.Insert(&childstore.Session{ChildID: childID, Kind: protocol.KindClaude, Status: protocol.StatusStreaming})
	ctrl.cm.Add(childID, ch)

	events, cancel := ctrl.native.SubscribeAll()
	defer cancel()

	ctrl.handleChildExit(childID, ch)

	var got *rafikiv1.Event
	for _, ev := range drainEvents(events) {
		if p := ev.GetError(); p != nil && p.GetCode() == "claude_launch_failed" {
			got = ev
		}
	}
	ck.Require().True(got != nil, "handleChildExit published no claude_launch_failed event")
	ck.StrContains(got.GetError().GetMessage(), "unknown option '--replay-user-messages'",
		"the child's own stderr must reach the event")
	ck.StrContains(got.GetError().GetMessage(), claudeUnsupportedFlagHint, "the upgrade hint")
}

// Only a direct claude prompt or steer of exactly one row carries a delivery
// uuid. Everything else — a daemon notification, a coalesced batch, an abort, a
// fundi child, an unknown child — writes none, so nothing can be acknowledged
// that nobody is waiting for.
func TestClaudeDeliveryUntrackedBatchesCarryNoUUID(t *testing.T) {
	ck := assert.NewCollecting(t)
	direct := inbox.Batch{ChildID: "c_claude", Mode: inbox.ModePrompt, IDs: []string{"row"}, Frags: []string{"hi"}}

	for _, tc := range []struct {
		name string
		kind string
		b    inbox.Batch
		want bool
	}{
		{"a direct claude prompt", protocol.KindClaude, direct, true},
		{"a direct claude steer", protocol.KindClaude, inbox.Batch{ChildID: "c_claude", Mode: inbox.ModeSteer, IDs: []string{"row"}}, true},
		{"a daemon-injected notification", protocol.KindClaude, inbox.Batch{ChildID: "c_claude", Mode: inbox.ModePrompt, Source: "subagents", IDs: []string{"row"}}, false},
		{"a coalesced batch names two rows", protocol.KindClaude, inbox.Batch{ChildID: "c_claude", Mode: inbox.ModePrompt, IDs: []string{"a", "b"}}, false},
		{"an abort is a signal", protocol.KindClaude, inbox.Batch{ChildID: "c_claude", Mode: inbox.ModeAbort, IDs: []string{"row"}}, false},
		{"a fundi child acknowledges its own way", protocol.KindFundi, direct, false},
		{"a script child is never written to", protocol.KindScript, direct, false},
	} {
		ck.Eq(tc.want, tracksClaudeDelivery(tc.kind, tc.b), "%s: tracksClaudeDelivery = %v, want", tc.name, !tc.want)
	}

	// And the funnel honours it: a sourced batch for a live claude child is
	// written with no uuid, and its echo path stays empty.
	h := newClaudeDeliveryHarness(t, "c_deliver")
	hook, _ := h.launch(t, "c_deliver")
	var written []string
	h.writeEcho(hook, &written, nil)

	ctx := context.Background()
	_, err := h.store.Accept(ctx, inbox.Inbound{
		ChildID: "c_deliver", Mode: inbox.ModePrompt, Text: "a worker settled", Source: "subagents",
	})
	ck.Require().NoError(err, "Accept")
	rows, err := h.store.Pending(ctx, "c_deliver")
	ck.Require().NoError(err, "Pending")
	batches := inbox.Coalesce(rows, inbox.BatchConfig{MaxFragments: 30, MaxBytesPerFlush: 65536})
	ck.Require().Len(batches, 1, "batches")
	awaitAck, err := h.c.deliverInbox(ctx, batches[0])
	ck.Require().NoError(err, "deliverInbox")
	ck.False(awaitAck, "a claude batch never awaits an ack")
	ck.EqDiff([]string{""}, written, "a notification must carry no delivery uuid")
	ck.Empty(receipts(h.drain()), "a notification published a receipt")

	// The orphan path (no rows at all) writes none either.
	written = nil
	h.c.deliverOrphans("c_deliver", []inbox.Inbound{{
		ChildID: "c_deliver", Mode: inbox.ModePrompt, Text: "lost its row",
	}})
	ck.EqDiff([]string{""}, written, "an orphan must carry no delivery uuid")
}

// A claude child that dies at LAUNCH carries its binary's own words, and — when
// those words are a rejected flag — the actionable fix. Never a probe, never a
// silent relaunch: rafiki launches every headless child with the replay flag and
// an older Claude Code must be upgraded.
func TestClaudeDeliveryLaunchFailureSurfacesTheRejectedFlag(t *testing.T) {
	ck := assert.NewAborting(t)

	rejected := []byte("error: unknown option '--replay-user-messages'\n")
	ev := claudeLaunchFailure(protocol.KindClaude, "", child.ShutdownResult{ExitCode: 1}, rejected)
	ck.Require().True(ev != nil, "a claude child that died at launch published nothing")
	ck.Eq("claude_launch_failed", ev.GetCode(), "error code")
	ck.StrContains(ev.GetMessage(), "unknown option '--replay-user-messages'",
		"the binary's own error must be carried verbatim, not summarised")
	ck.StrContains(ev.GetMessage(), claudeUnsupportedFlagHint, "the upgrade hint")
	ck.StrContains(ev.GetMessage(), "--replay-user-messages", "the hint must name the flag")

	// The UPGRADE path is a resumed child, not a fresh one: an existing child
	// always carries a session id (that is what --resume uses), so gating the
	// hint on "no session yet" would miss every child a daemon upgrade breaks.
	// A rejected flag is a rejected flag on both shapes.
	resumed := claudeLaunchFailure(protocol.KindClaude, "sess-1", child.ShutdownResult{ExitCode: 1}, rejected)
	ck.Require().True(resumed != nil, "a RESUMED claude child whose binary rejected a flag published nothing")
	ck.StrContains(resumed.GetMessage(), "unknown option '--replay-user-messages'", "the binary's own error on a resume")
	ck.StrContains(resumed.GetMessage(), claudeUnsupportedFlagHint, "the upgrade hint on a resume")
	ck.StrContains(resumed.GetMessage(), "sess-1", "the event must name the session it failed to resume")

	// Anything else carries its stderr without claiming a cause it did not name.
	auth := claudeLaunchFailure(protocol.KindClaude, "", child.ShutdownResult{ExitCode: 1},
		[]byte("Error: invalid API key"))
	ck.Require().True(auth != nil, "a launch failure with other stderr published nothing")
	ck.StrContains(auth.GetMessage(), "invalid API key", "stderr tail")
	ck.False(containsHint(auth.GetMessage()), "the upgrade hint was added to a failure that did not name a flag: %q", auth.GetMessage())

	for _, tc := range []struct {
		name      string
		kind      string
		sessionID string
		res       child.ShutdownResult
		stderr    []byte
	}{
		// A resumed child that exits nonzero with stderr naming no flag is an
		// ordinary turn/auth failure, not a launch failure.
		{"a resumed child that ran a turn and stopped", protocol.KindClaude, "sess-1", child.ShutdownResult{ExitCode: 1}, []byte("Error: invalid API key")},
		{"a killed child", protocol.KindClaude, "", child.ShutdownResult{ExitCode: 137, Signal: "killed"}, rejected},
		{"a resumed child that was killed", protocol.KindClaude, "sess-1", child.ShutdownResult{ExitCode: 137, Signal: "killed"}, rejected},
		{"a clean exit", protocol.KindClaude, "", child.ShutdownResult{ExitCode: 0}, rejected},
		{"no stderr to report", protocol.KindClaude, "", child.ShutdownResult{ExitCode: 1}, nil},
		{"another kind", protocol.KindFundi, "", child.ShutdownResult{ExitCode: 1}, rejected},
	} {
		ck.True(claudeLaunchFailure(tc.kind, tc.sessionID, tc.res, tc.stderr) == nil,
			"%s: published a launch failure it should not have", tc.name)
	}
}

func containsHint(msg string) bool {
	return strings.HasSuffix(msg, claudeUnsupportedFlagHint)
}

// The registry is the only place a uuid is resolved, and it must not leak: an
// entry is removed by the echo that resolves it, so a duplicate echo (claude
// may replay a frame) publishes one receipt, not two.
func TestClaudeDeliveryDuplicateEchoPublishesOneReceipt(t *testing.T) {
	ck := assert.NewAborting(t)
	h := newClaudeDeliveryHarness(t, "c_deliver")
	hook, inst := h.launch(t, "c_deliver")

	var written []string
	h.writeEcho(nil, &written, nil)
	rowID := h.send(t, "c_deliver", "hello")
	ck.Require().Len(written, 1, "frames written")

	hook(written[0])
	hook(written[0])
	ck.EqDiff([]string{rowID}, receipts(h.drain()), "a duplicated echo must publish exactly one receipt")

	// Nothing is left behind for the uuid, and nothing at all once the launch
	// is torn down.
	ck.Eq(0, h.c.claudeDeliveries.entryCount("c_deliver"), "entries left after the echo")
	h.c.claudeDeliveries.endLaunch("c_deliver", inst)
	ck.Eq(0, h.c.claudeDeliveries.entryCount("c_deliver"), "entries left after teardown")
}

// A claude child whose process cannot be exec'd never reaches abortSpawn: there
// is no instance for the generation installChildHooks minted to attach to, so
// Spawn's own early return has to retire it. Without that a coordinator retrying
// a failed spawn leaves one dead generation per attempt.
func TestClaudeDeliveryFailedExecLeavesNoGeneration(t *testing.T) {
	ck := assert.NewAborting(t)
	ctrl := newTestController(t)
	req := protocol.SpawnRequest{
		Kind:      protocol.KindClaude,
		Cwd:       t.TempDir(),
		PiBinary:  filepath.Join(t.TempDir(), "no-such-claude-binary"),
		NoSession: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := ctrl.Spawn(ctx, req, users.Identity{})
	ck.Require().Error(err, "a spawn whose binary does not exist must fail")
	ck.Eq(0, ctrl.claudeDeliveries.generationCount(),
		"a spawn that never exec'd left its launch generation behind")
}

// entryCount is a test-only view of one child's outstanding entries, so a test
// can assert the registry does not leak without reaching into its maps.
func (r *claudeDeliveryRegistry) entryCount(childID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for k := range r.rows {
		if k.childID == childID {
			n++
		}
	}
	return n
}

// generationCount is a test-only view of how many launch generations the
// registry still holds, so a test can prove a failed launch left none behind.
func (r *claudeDeliveryRegistry) generationCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.gens)
}
