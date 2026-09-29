// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/tui/rail"
)

// The shared engine behind `logs` and `tail`: an optional GetHistory backfill,
// then an optional StreamEvents follow. Both commands render through the one
// renderer in eventrender.go; the raw modes emit protojson Events through
// emitProto.

// conversationTypes are the durable types GetHistory serves (see
// eventconv.EventsFromMessages: message rows plus the compaction-boundary
// marker row). The single-child default shows these.
var conversationTypes = []string{
	"user_message",
	"assistant_message",
	"compaction_boundary",
}

// lifecycleTypes is the multi-child default — the rail's filter set, which is
// exactly what `watch` used to subscribe with. rail.Types() is the authority;
// the names it returns are the wire names eventlog.TypeName produces.
func lifecycleTypes() []string {
	return rail.Types()
}

// defaultTypeSet is the default `types` filter for a subscription, resolved
// by SUBJECT shape: multi-child (no id, or labels) → lifecycle only; a single
// child → conversation events plus lifecycle. Sorted so request bytes and
// tests are deterministic.
func defaultTypeSet(singleChild bool) []string {
	var types []string
	if singleChild {
		types = append(types, conversationTypes...)
	}
	types = append(types, lifecycleTypes()...)
	slices.Sort(types)
	return slices.Compact(types)
}

// allNativeTypes lists every classified event type name, sorted — the
// completion candidates and the validation set for --types.
func allNativeTypes() []string {
	out := eventlog.AllTypeNames()
	slices.Sort(out)
	return out
}

// resolveTypeFilter turns the command's type flags into a server-side type
// list and tier.
//
//   - --all-types → nil types (empty means every type in the tier) at tier
//     EVENT_TIER_ALL, so the ephemeral deltas are admitted.
//   - --types a,b → exactly those names at tier DURABLE; each is validated
//     against the native vocabulary, because an unknown name would otherwise
//     filter to silence rather than error. Naming an ephemeral type (the
//     deltas) bumps the tier to ALL — otherwise the request would match
//     nothing, the same silently-empty trap an unvalidated name does.
//   - neither → the subject-shaped default (defaultTypeSet) at tier DURABLE.
func resolveTypeFilter(explicit []string, allTypes, singleChild bool) ([]string, rafikiv1.EventTier, error) {
	switch {
	case allTypes:
		return nil, rafikiv1.EventTier_EVENT_TIER_ALL, nil
	case len(explicit) > 0:
		known := make(map[string]bool)
		for _, n := range allNativeTypes() {
			known[n] = true
		}
		ephemeral := false
		for _, t := range explicit {
			if !known[t] {
				return nil, 0, fmt.Errorf("unknown event type %q (known: %s)", t, strings.Join(allNativeTypes(), ", "))
			}
			// The one ephemeral type today: content_block_delta.
			if t == "content_block_delta" {
				ephemeral = true
			}
		}
		tier := rafikiv1.EventTier_EVENT_TIER_DURABLE
		if ephemeral {
			tier = rafikiv1.EventTier_EVENT_TIER_ALL
		}
		return slices.Clone(explicit), tier, nil
	default:
		return defaultTypeSet(singleChild), rafikiv1.EventTier_EVENT_TIER_DURABLE, nil
	}
}

// prefixCompletions filters candidates by prefix for a completion handler
// (which must never print, never fail, and never block).
func prefixCompletions(candidates []string, toComplete string) []string {
	out := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if strings.HasPrefix(c, toComplete) {
			out = append(out, c)
		}
	}
	return out
}

// childSubject names one child, itself.
func childSubject(childID string) *rafikiv1.EventSubject {
	return &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: childID}}
}

// allSubject names everything the caller is entitled to, narrowed by an
// optional label selector (the comma-joined grammar from control.proto; a
// malformed selector excludes server-side).
func allSubject(selector string) *rafikiv1.EventSubject {
	s := &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_All{All: true}}
	if selector != "" {
		s.LabelSelector = selector
	}
	return s
}

// eventQuery is what a command resolved before the engine runs. Defaults
// differ by frontend: tail sets follow=true, tailN=20; logs sets follow=false,
// tailN=-1.
type eventQuery struct {
	childID string                 // resolved child, "" for the multi-child subject
	subject *rafikiv1.EventSubject // which children the stream covers
	types   []string               // nil = every type in the tier
	tier    rafikiv1.EventTier
	tailN   int // backfill count: -1 = all, 0 = none, >0 = last N
	follow  bool
	raw     bool
	mode    outputMode
}

// eventPrinter renders one event per line in the query's mode. The machine
// modes (-j, -J, -r) emit the canonical protojson of the Event — pretty for
// -j, one compact line per event for -J, and for -r too (raw means protojson
// per line on the default output mode, which has no table shape for a
// stream). The human mode goes through the renderer.
type eventPrinter struct {
	w    io.Writer
	mode outputMode
	raw  bool
	r    *eventRenderer
}

func newEventPrinter(w io.Writer, mode outputMode, raw bool) *eventPrinter {
	return &eventPrinter{w: w, mode: mode, raw: raw, r: newEventRenderer()}
}

func (p *eventPrinter) emit(ev *rafikiv1.Event) error {
	switch {
	case p.mode == outputJSON:
		return emitProto(p.w, ev, outputJSON)
	case p.mode == outputJSONL, p.raw:
		return emitProto(p.w, ev, outputJSONL)
	default:
		if line := p.r.observe(ev, time.Now()); line != "" {
			_, err := fmt.Fprintln(p.w, line)
			return err
		}
		return nil
	}
}

// runEventQuery runs the backfill-then-follow engine for `logs` and `tail`.
//
// Backfill (single-child only; the multi-child subject has no history to
// load): GetHistory returns the whole conversation, the type filter applies
// client-side, and the last N events print. The cursor for the follow phase
// is the last history ordinal, so the stream resumes from where the history
// ended rather than from now.
//
// Follow: StreamEvents with the query's subject, tier and types. The stream
// ends the command except on the named child's own child_exited, which is the
// `logs`/`tail <id>` completion signal (a multi-child stream never exits on
// one child's exit — others may still be running).
func runEventQuery(
	ctx context.Context,
	ep connectEndpoint,
	client rafikiv1connect.ControlClient,
	q eventQuery,
	out io.Writer,
	notes io.Writer,
) error {
	printer := newEventPrinter(out, q.mode, q.raw)

	// Names and first-known statuses, before anything renders. Best-effort:
	// failures are noted on stderr and the rows read "[unnamed]" instead.
	if q.childID != "" {
		printer.r.seedChild(ctx, notes, client, q.childID, ep.describe)
	} else {
		printer.r.seed(ctx, notes, client, ep.describe)
	}

	// Backfill, and the resume cursor taken from where it ended.
	var cursor *rafikiv1.EventCursor
	if q.childID != "" && q.tailN != 0 {
		// A script child has no conversation: backfill from the DURABLE EVENT
		// LOG instead (StreamEvents replay from ordinal 0). Its events ARE the
		// transcript -- script_output/script_report -- and they live in the
		// same ordinal space the follow resumes from, so one watermark serves
		// both. GetHistory would answer NotFound and fall into the "no history
		// yet" dead end for a child whose whole output is in the log.
		var evs []*rafikiv1.Event
		empty := false
		var err error
		// A script child's summary also carries latest_ordinal, which bounds
		// the replay below.
		script := scriptChildSummary(ctx, client, q.childID)
		if script != nil {
			evs, err = fetchScriptLog(ctx, ep, client, q.childID, latestOrdinal(script))
		} else {
			evs, empty, err = fetchHistory(ctx, ep, client, q.childID)
		}
		if err != nil {
			return err
		}
		// The cursor is the last ordinal of the WHOLE history — of what ended
		// it, not of what the filter chose to show. Ordinal presence is read
		// from the FIELD: a durable event can arrive unstamped when the log
		// append failed, and such an event cannot advance a cursor.
		var lastOrd int32 = -1
		for _, ev := range evs {
			if ev.Ordinal == nil {
				continue
			}
			if o := ev.GetOrdinal(); o > lastOrd {
				lastOrd = o
			}
		}
		if lastOrd >= 0 {
			cursor = &rafikiv1.EventCursor{Ordinals: map[string]int32{q.childID: lastOrd}}
		}
		// The type filter applies client-side to the CONVERSATION backfill;
		// a script replay's events are already exactly scriptLogReplayTypes,
		// and filtering them through the conversation default would drop every
		// script_output it just fetched.
		printed := evs
		if script == nil {
			printed = filterByTypes(evs, q.types)
		}
		if q.tailN > 0 && len(printed) > q.tailN {
			printed = printed[len(printed)-q.tailN:]
		}
		for _, ev := range printed {
			if err := printer.emit(ev); err != nil {
				return err
			}
		}
		if len(printed) == 0 && empty && !q.follow {
			fmt.Fprintf(notes, "no history yet for %s\n", q.childID)
		}
	}

	if !q.follow {
		return nil
	}
	req := &rafikiv1.StreamEventsRequest{
		Subject: q.subject,
		Tier:    q.tier,
		Types:   q.types,
	}
	if cursor != nil {
		req.Cursor = cursor
	}
	stream, err := client.StreamEvents(ctx, connect.NewRequest(req))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	for stream.Receive() {
		ev := stream.Msg()
		if err := printer.emit(ev); err != nil {
			return err
		}
		// The named child's own exit ends the follow. Multi-child subjects
		// never exit on one child's exit.
		if ev.GetChildExited() != nil && q.childID != "" && ev.GetChildId() == q.childID {
			return nil
		}
	}
	if err := stream.Err(); err != nil && ctx.Err() == nil {
		return diagnoseConnectError(err, ep.describe)
	}
	return nil
}

// fetchHistory loads a child's full event history. A child with no
// conversation (a claude child that has not produced one yet, or an exited
// one whose conversation is gone) is empty history, not an error: `logs -f`
// on a just-spawned child must follow rather than die.
func fetchHistory(ctx context.Context, ep connectEndpoint, client rafikiv1connect.ControlClient, childID string) ([]*rafikiv1.Event, bool, error) {
	resp, err := client.GetHistory(ctx, connect.NewRequest(&rafikiv1.GetHistoryRequest{ChildId: childID}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, true, nil
		}
		return nil, false, diagnoseConnectError(err, ep.describe)
	}
	return resp.Msg.GetEvents(), false, nil
}

// scriptLogReplayTypes is the durable set a script child's `logs`/`tail`
// backfill reads from the event log. The cockpit's script pane reads the same
// log through the focus stream's replay (pkg/tui fetchHistoryOnce), so this
// vocabulary must keep covering every type a script child's transcript is
// made of.
var scriptLogReplayTypes = []string{
	"script_output",
	"script_report",
	"agent_status",
	"child_spawned",
	"child_exited",
	"error",
}

// scriptChildSummary resolves childID, best-effort: a script child returns
// its ChildSummary (which carries latest_ordinal); an unreachable, unknown or
// non-script child returns nil (the ordinary GetHistory path then runs, and
// its own error handling reports whatever is actually wrong).
func scriptChildSummary(ctx context.Context, client rafikiv1connect.ControlClient, childID string) *rafikiv1.ChildSummary {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
	if err != nil {
		return nil
	}
	if resp.Msg.GetChild().GetKind() != "script" {
		return nil
	}
	return resp.Msg.GetChild()
}

// latestOrdinal reads the watermark off a script child's summary. An unset
// latest_ordinal means the log has nothing to replay.
func latestOrdinal(s *rafikiv1.ChildSummary) int32 {
	if s == nil || s.LatestOrdinal == nil {
		return 0
	}
	return *s.LatestOrdinal
}

// fetchScriptLog replays a script child's durable event log from ordinal 0 —
// a script has no conversation rows, so GetHistory is not the record to read
// (its NotFound fallback would fetch the same log through the sequential
// error path and then print a misleading "no history yet"). The events come
// back in ordinal order and carry their own ordinals, so the returned slice
// is the backfill and its last ordinal is the follow's resume cursor.
//
// StreamEvents REPLAYS the log and then FOLLOWS live — the server never ends
// the stream on its own (for an exited script, Subscribe creates a fresh bus
// nobody closes), so this loop stops itself: at this child's child_exited, or
// once the ordinal reaches the summary's latest_ordinal watermark, whichever
// comes first, and then cancels the stream context to end the server's
// follow. Events logged after the watermark are left to the follow phase,
// which resumes from the last ordinal. A watermark of 0 means there is
// nothing to replay: the call is skipped entirely.
func fetchScriptLog(ctx context.Context, ep connectEndpoint, client rafikiv1connect.ControlClient, childID string, watermark int32) ([]*rafikiv1.Event, error) {
	if watermark <= 0 {
		return nil, nil
	}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req := &rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: childID}},
		Tier:    rafikiv1.EventTier_EVENT_TIER_DURABLE,
		Types:   scriptLogReplayTypes,
		Cursor:  &rafikiv1.EventCursor{Ordinals: map[string]int32{childID: -1}},
	}
	stream, err := client.StreamEvents(streamCtx, connect.NewRequest(req))
	if err != nil {
		return nil, diagnoseConnectError(err, ep.describe)
	}
	defer func() { _ = stream.Close() }()
	var evs []*rafikiv1.Event
	for stream.Receive() {
		ev := stream.Msg()
		// An event logged after the watermark is not part of the replay: stop
		// here and leave it to the follow phase, which resumes from the last
		// ordinal.
		if ev.Ordinal != nil && ev.GetOrdinal() > watermark {
			cancel()
			break
		}
		evs = append(evs, ev)
		stop := ev.GetChildExited() != nil && ev.GetChildId() == childID
		if !stop && ev.Ordinal != nil && ev.GetOrdinal() >= watermark {
			stop = true
		}
		if stop {
			cancel() // end the server's live follow
			break
		}
	}
	// A stream ended by our own cancel reports context.Canceled; that is the
	// normal exit, not an error.
	if err := stream.Err(); err != nil && streamCtx.Err() == nil {
		return nil, diagnoseConnectError(err, ep.describe)
	}
	return evs, nil
}

// filterByTypes keeps the events whose wire type is in the filter; nil (or
// empty) admits everything.
func filterByTypes(evs []*rafikiv1.Event, types []string) []*rafikiv1.Event {
	if len(types) == 0 {
		return evs
	}
	want := make(map[string]bool, len(types))
	for _, t := range types {
		want[t] = true
	}
	out := make([]*rafikiv1.Event, 0, len(evs))
	for _, ev := range evs {
		if want[eventlog.TypeName(ev)] {
			out = append(out, ev)
		}
	}
	return out
}
