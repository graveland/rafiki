package fundi

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/store"
)

type cacheRecordingSender struct {
	llm.Sender
	options []msg.EncodeOptions
}

func (s *cacheRecordingSender) Send(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (*msg.Response, error) {
	s.options = append(s.options, opts)
	return s.Sender.Send(ctx, req, opts)
}

func (s *cacheRecordingSender) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	s.options = append(s.options, opts)
	return s.Sender.Stream(ctx, req, opts)
}

func TestForkRoutingIDSurvivesEngineRebuild(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%t", streaming), func(t *testing.T) {
			c := assert.NewAborting(t)
			silenceSlog(t)
			pool, _ := dbTestPool(t)
			ctx := context.Background()
			var sourceID string
			c.NoError(pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by) VALUES ('cache-test', 'server') RETURNING id::text`).Scan(&sourceID))
			appended, err := conv.NewStore(store.NewConvBackend(pool), conv.Options{}).Open(sourceID, conv.AsOwner()).Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: "shared prefix"}}}}}})
			c.NoError(err)
			node := appended.Nodes[0].ID

			build := func(model, ref, fork string) (*Engine, msg.EncodeOptions) {
				sender := &cacheRecordingSender{Sender: scriptedSender(t, sampleEndTurn)}
				client, err := llm.NewClient(llm.WithProviderSender("anthropic", sender), llm.WithDefaultModel(model), llm.WithStore(pool))
				c.NoError(err)
				opts := []llm.ConvOption{llm.NewConversation("", "agent"), llm.ByExternalRef(ref)}
				if fork != "" {
					opts = append(opts, llm.ForkFrom(fork))
				}
				eng, err := NewEngine(EngineConfig{Client: client, Tools: fakeToolSet{}, ModelID: model, Provider: "anthropic", ConvOpts: opts}, NewFrontend(strings.NewReader(""), &syncBuffer{}, nil))
				c.NoError(err)
				eng.Start()
				t.Cleanup(eng.Close)
				if streaming {
					eng.runTurn("review", nil)
				} else {
					_, err = eng.conv.Send(ctx, llm.UserText("review"))
					c.NoError(err)
				}
				c.Len(sender.options, 1)
				c.Eq(eng.conv.ID, eng.State().SessionID, "the engine's own session stays its conversation")
				return eng, sender.options[0]
			}
			ref := "fork-cache-" + sourceID
			first, a := build("claude-x", ref, node)
			sibling, b := build("claude-x", ref+"-sibling", node)
			c.NotEq(first.conv.ID, sibling.conv.ID)
			sum := sha256.Sum256([]byte(node + "\x00claude-x"))
			c.Eq(fmt.Sprintf("fork-%x", sum[:16]), a.SessionID, "a fork sends the shared routing id")
			c.Eq(a.SessionID, b.SessionID, "siblings of one node on one model share it")
			_, other := build("claude-y", ref+"-other-model", node)
			c.NotEq(a.SessionID, other.SessionID, "another model routes on its own")
			rebuilt, resumed := build("claude-x", ref, "")
			c.Eq(first.conv.ID, rebuilt.conv.ID, "rebuild must reopen the stored fork")
			c.Eq(a.SessionID, resumed.SessionID, "rebuild has no spawn-time fork option")
			plain, normal := build("claude-x", ref+"-plain", "")
			c.Eq(plain.conv.ID, normal.SessionID, "a non-fork sends its own conversation id")
		})
	}
}
