// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
)

const (
	// doctorPrompt asks for any reply at all — the check is that an agent
	// answers, not that it answers a particular word.
	doctorPrompt = "Reply with a short greeting."
	// doctorAgentDeadline bounds the whole spawn-send-follow cycle.
	doctorAgentDeadline = 3 * time.Minute
	// doctorCloseTimeout bounds the best-effort kill and close.
	doctorCloseTimeout = 30 * time.Second
)

func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that rafiki works end to end on this machine",
		Long: `Check, against the current profile: the daemon answers, a live executor
can launch claude agents (only when a claude agent is checked), and each
requested agent replies to one prompt.

--agent picks the agents (repeatable): "claude" (the default) spawns a claude
child; "fundi:<provider/model>" spawns a fundi child on that model, e.g.
--agent fundi:openrouter/~deepseek/deepseek-flash-latest.

Each agent check costs tokens on your Claude login or provider, waits up to 3
minutes for the first reply, then closes the child. Each check prints ok or
FAIL and, on failure, the next thing to look at; doctor exits non-zero if any
check failed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmdCtx(cmd), cmd, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringSlice("agent", []string{"claude"}, `agent to check (repeatable): "claude" or "fundi:<provider/model>"`)
	return cmd
}

// doctorAgent is one parsed --agent value.
type doctorAgent struct {
	kind  string
	model string // fundi only
}

// label names the agent in doctor's output: "agent" for claude, so the default
// run reads as it always has, and "agent fundi:<model>" otherwise.
func (a doctorAgent) label() string {
	if a.kind == protocol.KindClaude {
		return "agent"
	}
	return "agent " + a.kind + ":" + a.model
}

// parseDoctorAgents parses --agent values: "claude", or "fundi:<model>" with a
// non-empty model (the daemon itself refuses an unqualified one).
func parseDoctorAgents(specs []string) ([]doctorAgent, error) {
	agents := make([]doctorAgent, 0, len(specs))
	for _, spec := range specs {
		switch {
		case spec == protocol.KindClaude:
			agents = append(agents, doctorAgent{kind: protocol.KindClaude})
		case strings.HasPrefix(spec, protocol.KindFundi+":") && len(spec) > len(protocol.KindFundi)+1:
			agents = append(agents, doctorAgent{kind: protocol.KindFundi, model: strings.TrimPrefix(spec, protocol.KindFundi+":")})
		default:
			return nil, fmt.Errorf("invalid --agent %q: want claude or fundi:<provider/model>", spec)
		}
	}
	return agents, nil
}

// runDoctor runs its checks in order against the current profile
// (newConnectEndpoint(cmd)), printing one line per check, and returns
// fmt.Errorf("doctor: %d check(s) failed", n) when any failed. A failed daemon
// check skips everything after it; a failed executor check skips only the
// claude agents, since a fundi agent needs no launch executor.
func runDoctor(ctx context.Context, cmd *cobra.Command, out io.Writer) error {
	specs, _ := cmd.Flags().GetStringSlice("agent")
	agents, err := parseDoctorAgents(specs)
	if err != nil {
		return err
	}
	wantClaude := false
	for _, a := range agents {
		if a.kind == protocol.KindClaude {
			wantClaude = true
		}
	}
	skipAll := func(executor bool) {
		if executor && wantClaude {
			fmt.Fprintln(out, "skip  executor")
		}
		for _, a := range agents {
			fmt.Fprintf(out, "skip  %s\n", a.label())
		}
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		fmt.Fprintf(out, "FAIL  daemon: %v\n", err)
		fmt.Fprintln(out, "      next: rafiki service status")
		skipAll(true)
		return fmt.Errorf("doctor: %d check(s) failed", 1)
	}
	client := ep.control()

	if _, err := client.Status(ctx, connect.NewRequest(&rafikiv1.StatusRequest{})); err != nil {
		fmt.Fprintf(out, "FAIL  daemon: %v\n", err)
		fmt.Fprintln(out, "      next: rafiki service status")
		skipAll(true)
		return fmt.Errorf("doctor: %d check(s) failed", 1)
	}
	fmt.Fprintln(out, "ok    daemon: answering")

	failed := 0
	executorOK := true
	if wantClaude {
		ref, err := resolveLaunchExecutor(ctx, cmd, "", protocol.KindClaude)
		switch {
		case err != nil:
			fmt.Fprintf(out, "FAIL  executor: %v\n", err)
		case ref == "":
			fmt.Fprintln(out, "FAIL  executor: no executor can launch claude agents")
		default:
			fmt.Fprintf(out, "ok    executor: %s can launch claude agents\n", ref)
		}
		if err != nil || ref == "" {
			fmt.Fprintln(out, "      next: rafiki executor setup")
			executorOK = false
			failed++
		}
	}

	for _, a := range agents {
		if a.kind == protocol.KindClaude && !executorOK {
			fmt.Fprintf(out, "skip  %s\n", a.label())
			continue
		}
		if err := runDoctorAgent(ctx, client, out, a); err != nil {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("doctor: %d check(s) failed", failed)
	}
	return nil
}

// runDoctorAgent is doctor's agent check: spawn a child of a's kind (and
// model), ask it for a short greeting, and pass when its first turn produces
// any non-empty reply. The child is closed on every path.
func runDoctorAgent(ctx context.Context, client rafikiv1connect.ControlClient, out io.Writer, a doctorAgent) error {
	// ONE deadline for the whole spawn -> send -> follow sequence: a hung
	// daemon must not block `rafiki doctor` forever. The deferred Close below
	// uses its own context, so it still runs when this one is done.
	ctx, cancel := context.WithTimeout(ctx, doctorAgentDeadline)
	defer cancel()

	home, _ := os.UserHomeDir()
	spawnResp, err := client.Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Kind:   a.kind,
		Model:  a.model,
		Cwd:    home,
		Labels: map[string]string{"rafiki-doctor": "agent"},
	}))
	if err != nil {
		fmt.Fprintf(out, "FAIL  %s: %v\n", a.label(), err)
		return err
	}
	childID := spawnResp.Msg.GetChildId()

	// Close the child no matter how the check ends — a leftover child would
	// keep costing money and clutter the fleet.
	defer closeDoctorChild(client, childID, out)

	// Read the child's latest ordinal BEFORE Send, then stream from it with a
	// cursor: the daemon replays everything after the floor, so the prompt's
	// turn is seen however fast it runs. Subscribing first and sending second
	// deadlocks instead: StreamEvents returns only once the daemon sends
	// response headers, which it does with the first event — and a freshly
	// spawned child emits none until it is prompted.
	got, err := client.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
	if err != nil {
		fmt.Fprintf(out, "FAIL  %s: %v\n", a.label(), err)
		return err
	}
	cursor := &rafikiv1.EventCursor{}
	if got.Msg.GetChild().LatestOrdinal != nil {
		cursor.Ordinals = map[string]int32{childID: got.Msg.GetChild().GetLatestOrdinal()}
	}

	fmt.Fprintln(out, "      waiting for the agent's first reply (up to 3m)…")
	if _, err := client.Send(ctx, connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: childID,
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{{
			Index: 0,
			Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: doctorPrompt}},
		}},
	})); err != nil {
		fmt.Fprintf(out, "FAIL  %s: %v\n", a.label(), err)
		return err
	}

	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: childID}},
		Tier:    rafikiv1.EventTier_EVENT_TIER_DURABLE,
		Cursor:  cursor,
	}))
	if err != nil {
		fmt.Fprintf(out, "FAIL  %s: %v\n", a.label(), err)
		fmt.Fprintf(out, "      next: rafiki logs %s\n", childID)
		return err
	}
	defer func() { _ = stream.Close() }()

	text, err := followFirstTurn(ctx, stream)
	if err != nil {
		fmt.Fprintf(out, "FAIL  %s: %v\n", a.label(), err)
		fmt.Fprintf(out, "      next: rafiki logs %s\n", childID)
		return err
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintf(out, "FAIL  %s: no reply (child %s)\n", a.label(), childID)
		fmt.Fprintf(out, "      next: rafiki logs %s\n", childID)
		return fmt.Errorf("agent %s did not reply", childID)
	}
	fmt.Fprintf(out, "ok    %s: replied\n", a.label())
	return nil
}

// closeDoctorChild stops and closes a child exactly as `rafiki close` does
// (closeChildConnect: the daemon refuses Close on a running child), with its
// own context so a cancelled or timed-out parent context still closes it. A
// failure does not fail the check, but it is reported: a child left running
// keeps costing money.
func closeDoctorChild(client rafikiv1connect.ControlClient, childID string, out io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), doctorCloseTimeout)
	defer cancel()
	if err := closeChildConnect(ctx, client, childID, rafikiv1.DescendantScope_DESCENDANT_SCOPE_TARGET_ONLY, 0, 0); err != nil {
		fmt.Fprintf(out, "      warning: could not close child %s (%v); close it with: rafiki close %s\n", childID, err, childID)
	}
}

// followFirstTurn reads a child event stream, opened from a cursor taken before
// the prompt was sent, until the first turn completes and returns the
// accumulated assistant text. The caller owns closing the stream.
func followFirstTurn(ctx context.Context, stream *connect.ServerStreamForClient[rafikiv1.Event]) (string, error) {
	var text strings.Builder
	for stream.Receive() {
		ev := stream.Msg()
		if am := ev.GetAssistantMessage(); am != nil {
			for _, b := range am.GetContent() {
				if t := b.GetText(); t != nil {
					text.WriteString(t.GetText())
				}
			}
		}
		if ev.GetTurnEnd() != nil || ev.GetChildExited() != nil {
			return text.String(), nil
		}
	}
	if err := stream.Err(); err != nil && ctx.Err() == nil {
		return text.String(), fmt.Errorf("doctor stream: %w", err)
	}
	return text.String(), nil
}
