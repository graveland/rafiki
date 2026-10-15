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

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
)

const (
	// smokePrompt is a question whose only correct answer is one word.
	smokePrompt = "Reply with exactly the word OK and nothing else."
	// smokeDeadline bounds the whole spawn-send-follow cycle.
	smokeDeadline = 3 * time.Minute
	// smokeCloseTimeout bounds the best-effort Close.
	smokeCloseTimeout = 10 * time.Second
)

// runSmokeStep spawns a claude child, sends it smokePrompt, follows its events
// until its first turn ends, and checks the assistant text for "OK". The child
// is closed in every case. --no-smoke skips it entirely.
func runSmokeStep(ctx context.Context, w *wizard, out io.Writer, profileName string) error {
	if noSmoke, _ := w.cmd.Flags().GetBool("no-smoke"); noSmoke {
		fmt.Fprintln(out, "    skipped (--no-smoke)")
		return nil
	}

	ep, err := setupEndpoint(profileName)
	if err != nil {
		return err
	}
	client := ep.control()

	home, _ := os.UserHomeDir()
	spawnResp, err := client.Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Kind:   protocol.KindClaude,
		Cwd:    home,
		Labels: map[string]string{"rafiki-setup": "smoke"},
	}))
	if err != nil {
		return fmt.Errorf("smoke spawn: %w", err)
	}
	childID := spawnResp.Msg.GetChildId()

	// Close the child no matter how the smoke ends — a leftover child would
	// keep costing money and clutter the fleet.
	defer closeSetupChild(client, childID)

	if _, err := client.Send(ctx, connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: childID,
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{{
			Index: 0,
			Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: smokePrompt}},
		}},
	})); err != nil {
		return fmt.Errorf("smoke send: %w", err)
	}

	passed, err := followFirstTurn(ctx, client, childID)
	if err != nil {
		return err
	}
	if !passed {
		fmt.Fprintf(out, "    warning: smoke test failed; inspect the child with: rafiki logs %s\n", childID)
		return fmt.Errorf("smoke test failed: the child did not reply OK (child %s)", childID)
	}
	fmt.Fprintln(out, "    ok (agent replied OK)")
	return nil
}

// closeSetupChild closes a child with its own short-lived context, so a
// cancelled or timed-out parent context still closes it. Errors are
// deliberately ignored: this is best-effort cleanup.
func closeSetupChild(client rafikiv1connect.ControlClient, childID string) {
	ctx, cancel := context.WithTimeout(context.Background(), smokeCloseTimeout)
	defer cancel()
	_, _ = client.Close(ctx, connect.NewRequest(&rafikiv1.CloseRequest{ChildId: childID}))
}

// followFirstTurn follows a single child's durable event stream until its first
// turn completes and returns whether the assistant text contains "OK". It uses
// the same StreamEvents stream `rafiki tail` follows — never a Get poll.
func followFirstTurn(ctx context.Context, client rafikiv1connect.ControlClient, childID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, smokeDeadline)
	defer cancel()

	stream, err := client.StreamEvents(ctx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
		Subject: &rafikiv1.EventSubject{Scope: &rafikiv1.EventSubject_Child{Child: childID}},
		Tier:    rafikiv1.EventTier_EVENT_TIER_DURABLE,
	}))
	if err != nil {
		return false, fmt.Errorf("smoke stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

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
			return assistantSaidOK(text.String()), nil
		}
	}
	if err := stream.Err(); err != nil && ctx.Err() == nil {
		return false, fmt.Errorf("smoke stream: %w", err)
	}
	return assistantSaidOK(text.String()), nil
}

// assistantSaidOK reports whether the trimmed, upper-cased assistant text
// contains "OK".
func assistantSaidOK(text string) bool {
	return strings.Contains(strings.ToUpper(strings.TrimSpace(text)), "OK")
}
