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
	// doctorPrompt asks for any reply at all — the check is that a claude
	// agent answers, not that it answers a particular word.
	doctorPrompt = "Reply with a short greeting."
	// doctorAgentDeadline bounds the whole spawn-send-follow cycle.
	doctorAgentDeadline = 3 * time.Minute
	// doctorCloseTimeout bounds the best-effort Close.
	doctorCloseTimeout = 10 * time.Second
)

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that rafiki works end to end on this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmdCtx(cmd), cmd, cmd.OutOrStdout())
		},
	}
}

// runDoctor runs three checks in order against the current profile
// (newConnectEndpoint(cmd)), printing one line per check, and returns
// fmt.Errorf("doctor: %d check(s) failed", n) when any failed.
func runDoctor(ctx context.Context, cmd *cobra.Command, out io.Writer) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		fmt.Fprintf(out, "FAIL  daemon: %v\n", err)
		fmt.Fprintln(out, "      next: rafiki service status")
		fmt.Fprintln(out, "skip  executor")
		fmt.Fprintln(out, "skip  agent")
		return fmt.Errorf("doctor: %d check(s) failed", 1)
	}
	client := ep.control()

	if _, err := client.Status(ctx, connect.NewRequest(&rafikiv1.StatusRequest{})); err != nil {
		fmt.Fprintf(out, "FAIL  daemon: %v\n", err)
		fmt.Fprintln(out, "      next: rafiki service status")
		fmt.Fprintln(out, "skip  executor")
		fmt.Fprintln(out, "skip  agent")
		return fmt.Errorf("doctor: %d check(s) failed", 1)
	}
	fmt.Fprintln(out, "ok    daemon: answering")

	ref, err := resolveLaunchExecutor(ctx, cmd, "", protocol.KindClaude)
	if err != nil {
		fmt.Fprintf(out, "FAIL  executor: %v\n", err)
		fmt.Fprintln(out, "      next: rafiki executor setup")
		fmt.Fprintln(out, "skip  agent")
		return fmt.Errorf("doctor: %d check(s) failed", 1)
	}
	if ref == "" {
		fmt.Fprintln(out, "FAIL  executor: no executor can launch claude agents")
		fmt.Fprintln(out, "      next: rafiki executor setup")
		fmt.Fprintln(out, "skip  agent")
		return fmt.Errorf("doctor: %d check(s) failed", 1)
	}
	fmt.Fprintf(out, "ok    executor: %s can launch claude agents\n", ref)

	if err := runDoctorAgent(ctx, client, out); err != nil {
		return fmt.Errorf("doctor: %d check(s) failed", 1)
	}
	return nil
}

// runDoctorAgent is doctor's third check: spawn a claude child, ask it for a
// short greeting, and pass when its first turn produces any non-empty reply.
// The child is closed on every path.
func runDoctorAgent(ctx context.Context, client rafikiv1connect.ControlClient, out io.Writer) error {
	// ONE deadline for the whole spawn -> send -> follow sequence: a hung
	// daemon must not block `rafiki doctor` forever. The deferred Close below
	// uses its own context, so it still runs when this one is done.
	ctx, cancel := context.WithTimeout(ctx, doctorAgentDeadline)
	defer cancel()

	home, _ := os.UserHomeDir()
	spawnResp, err := client.Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Kind:   protocol.KindClaude,
		Cwd:    home,
		Labels: map[string]string{"rafiki-doctor": "agent"},
	}))
	if err != nil {
		fmt.Fprintf(out, "FAIL  agent: %v\n", err)
		return err
	}
	childID := spawnResp.Msg.GetChildId()

	// Close the child no matter how the check ends — a leftover child would
	// keep costing money and clutter the fleet.
	defer closeDoctorChild(client, childID)

	// Open the durable event stream BEFORE Send: with no cursor there is no
	// replay, so a turn that finished before we subscribed would be missed
	// entirely. Subscribe first, then send, then read.
	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: childID}},
		Tier:    rafikiv1.EventTier_EVENT_TIER_DURABLE,
	}))
	if err != nil {
		fmt.Fprintf(out, "FAIL  agent: %v\n", err)
		return err
	}
	defer func() { _ = stream.Close() }()

	fmt.Fprintln(out, "      waiting for the agent's first reply (up to 3m)…")
	if _, err := client.Send(ctx, connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: childID,
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{{
			Index: 0,
			Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: doctorPrompt}},
		}},
	})); err != nil {
		fmt.Fprintf(out, "FAIL  agent: %v\n", err)
		return err
	}

	text, err := followFirstTurn(ctx, stream)
	if err != nil {
		fmt.Fprintf(out, "FAIL  agent: %v\n", err)
		fmt.Fprintf(out, "      next: rafiki logs %s\n", childID)
		return err
	}
	if strings.TrimSpace(text) == "" {
		fmt.Fprintf(out, "FAIL  agent: no reply (child %s)\n", childID)
		fmt.Fprintf(out, "      next: rafiki logs %s\n", childID)
		return fmt.Errorf("agent %s did not reply", childID)
	}
	fmt.Fprintln(out, "ok    agent: replied")
	return nil
}

// closeDoctorChild closes a child with its own short-lived context, so a
// cancelled or timed-out parent context still closes it. Errors are
// deliberately ignored: this is best-effort cleanup.
func closeDoctorChild(client rafikiv1connect.ControlClient, childID string) {
	ctx, cancel := context.WithTimeout(context.Background(), doctorCloseTimeout)
	defer cancel()
	_, _ = client.Close(ctx, connect.NewRequest(&rafikiv1.CloseRequest{ChildId: childID}))
}

// followFirstTurn reads an already-open child event stream until the first turn
// completes and returns the accumulated assistant text. The caller opens the
// stream BEFORE Send (so no event is missed) and owns closing it.
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
