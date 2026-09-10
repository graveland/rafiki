// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/rail"
	"go.graveland.dev/rafiki/pkg/tui/streams"
)

func newWatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "watch [id|name]",
		Short: "Stream agent lifecycle events to stdout as they happen",
		Long: `Watch agents start, work, settle and exit — a flat event stream, not a TUI.

Subscribes to the same durable lifecycle events the cockpit's rail is built
from (child_spawned, agent_status, turn_end, child_exited, error, retry), so it
doubles as a window on what the rail should be showing. Every agent_status line
carries how long the previous state lasted, so

    status  c_01ABC  impl-auth  streaming → idle (1m12s)

reads as the duration of the turn's work. With an argument the subscription is
that child's subtree including itself; with none it is every child you can see.

Live only — there is no replay of past events. Event lines go to stdout; notes
about connecting and reconnecting go to stderr, so a pipe carries exactly the
events. -J emits one JSON event per line instead.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runWatch,
	}
	cmd.Flags().Bool("all-types", false, "include every durable event type (user_message, assistant_message, tool events) — verbose")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runWatch(cmd *cobra.Command, args []string) error {
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	allTypes, _ := cmd.Flags().GetBool("all-types")

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	client := ep.control()

	subject := subjectFor("")
	if len(args) == 1 {
		childID, err := resolveChildConnect(cmdCtx(cmd), ep, client, args[0])
		if err != nil {
			return err
		}
		subject = subjectFor(childID)
	}

	fmt.Fprintf(cmd.ErrOrStderr(), "watching %s — Ctrl-C to stop\n", ep.describe)

	tracker := newWatchTracker()
	out := cmd.OutOrStdout()
	backoffAttempt := 0
	for {
		if cmdCtx(cmd).Err() != nil {
			return nil
		}
		// On every (re)connect, learn the current roster: child_spawned does
		// not replay (the cursor is absent and the events are in the past),
		// so without this a reconnect loses every name it had. Durations
		// measured before (re)connection are unknown and read from connect
		// time — that is the best a live-only stream can say.
		tracker.seed(cmdCtx(cmd), cmd.ErrOrStderr(), client, ep.describe)

		req := &rafikiv1.StreamEventsRequest{
			Subject: subject,
			Tier:    rafikiv1.EventTier_EVENT_TIER_DURABLE,
		}
		if !allTypes {
			// The rail's own filter set, deliberately: the point is a window
			// on what the rail is fed. --all-types widens to every durable
			// type, message content included.
			req.Types = rail.Types()
		}

		stream, err := client.StreamEvents(cmdCtx(cmd), connect.NewRequest(req))
		if err != nil {
			if cmdCtx(cmd).Err() != nil {
				return nil
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "# connect failed: %v\n", diagnoseConnectError(err, ep.describe))
		} else {
			backoffAttempt = 0
			for stream.Receive() {
				ev := stream.Msg()
				if mode == outputJSONL {
					if b, err := protojson.Marshal(ev); err == nil {
						fmt.Fprintln(out, string(b))
					}
					continue
				}
				if line := tracker.observe(ev, time.Now()); line != "" {
					fmt.Fprintln(out, line)
				}
			}
			if err := stream.Err(); err != nil && cmdCtx(cmd).Err() == nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "# stream ended: %v\n", err)
			}
		}

		if cmdCtx(cmd).Err() != nil {
			return nil
		}
		select {
		case <-cmdCtx(cmd).Done():
			return nil
		case <-time.After(streams.BackoffFor(backoffAttempt)):
		}
		backoffAttempt++
	}
}

// watchTracker renders the event stream into one line per event, carrying the
// per-child state a single event does not have: names, and how long each
// state lasted. It is deliberately pure (a clock is passed in) so the
// durations — the part that makes status lines worth reading — are testable
// without a daemon.
type watchTracker struct {
	names    map[string]string
	status   map[string]string
	statusAt map[string]time.Time
	bornAt   map[string]time.Time
}

func newWatchTracker() *watchTracker {
	return &watchTracker{
		names:    make(map[string]string),
		status:   make(map[string]string),
		statusAt: make(map[string]time.Time),
		bornAt:   make(map[string]time.Time),
	}
}

// rosterSource is the one RPC seed() needs — a real ControlClient satisfies
// it, and so does a test stub without the other thirty methods.
type rosterSource interface {
	ListChildren(ctx context.Context, req *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error)
}

// seed records the current roster from ListChildren. Failures are noted on
// `notes` (stderr in production) and are not fatal: the stream still works, the
// rows just read "unnamed" until a child_spawned or a later reconnect names
// them.
func (t *watchTracker) seed(ctx context.Context, notes io.Writer, client rosterSource, describe string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.ListChildren(ctx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	if err != nil {
		fmt.Fprintf(notes, "# roster unavailable at %s: %v\n", describe, err)
		return
	}
	now := time.Now()
	for _, ch := range resp.Msg.GetChildren() {
		if id := ch.GetChildId(); id != "" {
			if _, known := t.names[id]; !known && ch.GetName() != "" {
				t.names[id] = ch.GetName()
			}
			if _, known := t.status[id]; !known {
				t.status[id] = ch.GetStatus()
				t.statusAt[id] = now
				t.bornAt[id] = now
			}
		}
	}
}

// observe folds one event and returns its line, or "" when there is nothing
// worth printing (nil events, events with no child id, payloads with no
// lifecycle meaning).
func (t *watchTracker) observe(ev *rafikiv1.Event, now time.Time) string {
	if ev == nil || ev.GetChildId() == "" {
		return ""
	}
	id := ev.GetChildId()

	var verb string
	var detail string
	switch p := ev.GetPayload().(type) {
	case *rafikiv1.Event_ChildSpawned:
		verb = "spawn"
		if name := p.ChildSpawned.GetName(); name != "" {
			t.names[id] = name
		}
		if parent := p.ChildSpawned.GetParentId(); parent != "" {
			detail = " parent=" + parent
		}
		t.status[id] = "spawning"
		t.statusAt[id] = now
		t.bornAt[id] = now
	case *rafikiv1.Event_AgentStatus:
		verb = "status"
		prev := t.status[id]
		if prev != "" && prev != p.AgentStatus.GetState() {
			detail = fmt.Sprintf(" %s → %s (%s)", prev, p.AgentStatus.GetState(), fmtDur(now.Sub(t.statusAt[id])))
		} else {
			detail = " " + p.AgentStatus.GetState()
		}
		t.status[id] = p.AgentStatus.GetState()
		t.statusAt[id] = now
	case *rafikiv1.Event_ChildExited:
		verb = "exit"
		detail = " " + exitSummary(p.ChildExited)
		if born, ok := t.bornAt[id]; ok {
			detail += fmt.Sprintf(" (lifetime %s)", fmtDur(now.Sub(born)))
		}
		t.status[id] = "exited"
	case *rafikiv1.Event_TurnEnd:
		verb = "turn"
		te := p.TurnEnd
		// cost_usd is optional: absent reads as "not reported", while 0 is a
		// real reading worth printing on a free model.
		if te.CostUsd != nil {
			detail = fmt.Sprintf(" cost=$%.4f", te.GetCostUsd())
		}
		if te.GetStopReason() != rafikiv1.StopReason_STOP_REASON_UNSPECIFIED {
			// Trim the proto prefix; "stop=END_TURN" reads, "STOP_REASON_"
			// does not.
			detail += " stop=" + strings.TrimPrefix(te.GetStopReason().String(), "STOP_REASON_")
		}
		// The turn's duration, as far as status transitions can see it: how
		// long the child has been away from idle. Absent when it never
		// reported a working state on this connection.
		if st := t.status[id]; rail.Working(st) {
			detail += fmt.Sprintf(" (%s)", fmtDur(now.Sub(t.statusAt[id])))
		}
	case *rafikiv1.Event_Error:
		verb = "error"
		detail = fmt.Sprintf(" %s: %s", p.Error.GetCode(), p.Error.GetMessage())
	case *rafikiv1.Event_Retry:
		verb = "retry"
		detail = fmt.Sprintf(" attempt=%d", p.Retry.GetAttempt())
		if p.Retry.GetWillRetry() {
			detail += " will-retry"
		} else {
			detail += " no-retry"
		}
		if r := p.Retry.GetReason(); r != "" {
			detail += " " + r
		}
	default:
		// An unrecognized durable payload still gets a line in JSONL mode
		// (which dumps the raw event before this runs); in the human format a
		// verb with nothing to say is noise.
		return ""
	}
	return fmt.Sprintf("%s  %-6s %s  %s%s", now.Format("15:04:05"), verb, id, t.nameOf(id), detail)
}

func (t *watchTracker) nameOf(id string) string {
	if name, ok := t.names[id]; ok {
		return name
	}
	// An unnamed child is itself signal — it is exactly what the rail looks
	// like when discovery misses — so say so rather than print an empty cell.
	return "[unnamed]"
}

func exitSummary(p *rafikiv1.ChildExited) string {
	// ExitCode is optional and absence is meaningful: a signalled child has no
	// exit code, and 0 means success. Read the FIELD for presence -- the
	// generated getter collapses absent and 0 into the same int32.
	if p.ExitCode != nil {
		return fmt.Sprintf("code=%d", p.GetExitCode())
	}
	if p.GetSignal() != "" {
		return "signal=" + p.GetSignal()
	}
	return "code=?"
}

// fmtDur renders a duration the way a status line wants it: 3.2s, 1m12s,
// 1h02m.
func fmtDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
