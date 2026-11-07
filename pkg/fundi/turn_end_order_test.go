// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/llm"

	"github.com/multigres/testkit/assert"
)

// endFramesEmitted reports whether the frontend output already carries the
// frames that close a turn (agent_end / agent_settled). The daemon derives the
// child's idle transition from them, so OnTurnEnded — which records the
// outcome and LastTurnError the spawner-notify gate reads — must run before
// either appears.
func endFramesEmitted(t *testing.T, out *syncBuffer) bool {
	t.Helper()
	for _, typ := range frameTypes(t, out.String()) {
		if typ == "agent_end" || typ == "agent_settled" {
			return true
		}
	}
	return false
}

// turnEndRecorder is an OnTurnEnded hook that notes, at call time, whether the
// turn's closing frames had been emitted.
type turnEndRecorder struct {
	out    *syncBuffer
	t      *testing.T
	calls  int
	early  bool
	failed bool
}

func (r *turnEndRecorder) hook(o TurnOutcome) {
	r.calls++
	r.failed = !o.Clean
	r.early = !endFramesEmitted(r.t, r.out)
}

func TestTurnEndedPrecedesAgentEndOnTheTurnPath(t *testing.T) {
	cases := []struct {
		name   string
		bodies []string
		clean  bool
	}{
		{"clean turn", []string{sampleEndTurn}, true},
		// One body for a two-call turn: the second call finds the scripted
		// sender exhausted and fails the turn.
		{"failed turn", []string{sampleResp}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			rec := &turnEndRecorder{t: t}
			ts := fakeToolSet{"bash": func(context.Context, json.RawMessage) (string, error) { return "ok", nil }}
			eng, out := newTestEngineWithConfig(t, ts, scriptedSender(t, tc.bodies...), func(cfg *EngineConfig) {
				cfg.OnTurnEnded = rec.hook
			})
			rec.out = out

			eng.HandlePrompt("go")
			eng.Wait()

			ck.Eq(1, rec.calls, "OnTurnEnded fired %d times, want 1", rec.calls)
			ck.Eq(tc.clean, !rec.failed, "outcome cleanliness")
			ck.True(rec.early, "OnTurnEnded ran after agent_end/agent_settled was emitted; out:\n%s", out.String())
			ck.True(endFramesEmitted(t, out), "the turn emitted no agent_end/agent_settled; out:\n%s", out.String())
		})
	}
}

func TestTurnEndedPrecedesAgentEndOnTheStartupResumePath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fail  bool
		clean bool
	}{
		{"clean resume", false, true},
		{"failed resume", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ck := assert.NewAborting(t)
			silenceSlog(t)
			pool, _ := dbTestPool(t)
			ctx := context.Background()
			ref := "turn-end-order-" + strings.ReplaceAll(tc.name, " ", "-")

			seed, err := llm.NewClient(
				llm.WithProviderSender("anthropic", scriptedSender(t, sampleEndTurn)),
				llm.WithStore(pool),
				llm.WithDefaultModel("claude-x"))
			ck.NoError(err)
			conv1, err := seed.Conversation(ctx, llm.Entrypoint("agent"), llm.ByExternalRef(ref))
			ck.NoError(err)
			_, err = conv1.AppendUser(ctx, llm.UserText("interrupted work"))
			ck.NoError(err)

			var sender llm.Sender = scriptedSender(t, sampleEndTurn)
			if tc.fail {
				sender = &failFirstSender{inner: sender, err: errors.New("agent: scripted turns exhausted")}
			}
			client, err := llm.NewClient(
				llm.WithProviderSender("anthropic", sender),
				llm.WithStore(pool),
				llm.WithDefaultModel("claude-x"))
			ck.NoError(err)
			out := &syncBuffer{}
			rec := &turnEndRecorder{t: t, out: out}
			fe := NewFrontend(strings.NewReader(""), out, nil)
			eng, err := NewEngine(EngineConfig{
				Client:      client,
				Tools:       fakeToolSet{},
				Provider:    "anthropic",
				ModelID:     "claude-x",
				Name:        "w1",
				AutoResume:  true,
				OnTurnEnded: rec.hook,
				ConvOpts:    []llm.ConvOption{llm.Entrypoint("agent"), llm.ByExternalRef(ref)},
			}, fe)
			ck.NoError(err)
			eng.Start()
			fe.handler = eng

			deadline := time.Now().Add(10 * time.Second)
			for !endFramesEmitted(t, out) && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			eng.Wait()

			ck.Require().True(endFramesEmitted(t, out), "the resumed turn never closed; out:\n%s", out.String())
			ck.Eq(1, rec.calls, "OnTurnEnded fired %d times, want 1", rec.calls)
			ck.Eq(tc.clean, !rec.failed, "outcome cleanliness")
			ck.True(rec.early, "OnTurnEnded ran after agent_end/agent_settled was emitted; out:\n%s", out.String())
		})
	}
}
