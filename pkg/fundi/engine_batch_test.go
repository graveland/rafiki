package fundi

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
)

// blockingBatcher is a fake llm.Batcher whose Park blocks until released —
// the deterministic stand-in for a provider batch that takes minutes.
type blockingBatcher struct {
	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
	parked  int
}

func newBlockingBatcher() *blockingBatcher {
	return &blockingBatcher{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (b *blockingBatcher) Park(ctx context.Context, _ string, _ string, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	b.mu.Lock()
	b.parked++
	b.mu.Unlock()
	close(b.started) //Park is called once per test; close signals arrival
	<-b.release
	return batchEngineReply(), nil
}

// batchEngineReply is the assistant message a parked batch call delivers.
func batchEngineReply() *anthropic.Message {
	msg := &anthropic.Message{}
	raw := `{"id":"msg_b","type":"message","role":"assistant","model":"z-ai/glm-5.3-flash:batch",
		"content":[{"type":"text","text":"batched"}],
		"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`
	if err := json.Unmarshal([]byte(raw), msg); err != nil {
		panic(err)
	}
	return msg
}

// batchEngineClient builds a store-less client over an openrouter provider
// whose model is the :batch id, wired with batcher b.
func batchEngineClient(t *testing.T, b llm.Batcher) *llm.Client {
	t.Helper()
	client, err := llm.NewClient(
		llm.WithProviderSender("openrouter", neverSender{}),
		llm.WithDefaultModel("openrouter/z-ai/glm-5.3-flash:batch"),
		llm.WithBatcher(b),
	)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// neverSender fails the test if the live sender is ever reached — on a :batch
// FIRST call the request must park, never go live.
type neverSender struct{}

func (neverSender) New(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	return nil, errNeverSent{}
}

type errNeverSent struct{}

func (errNeverSent) Error() string { return "test: live sender must never be reached on the park path" }

func TestEngineBatchWaitFrames(t *testing.T) {
	silenceSlog(t)
	ts := fakeToolSet{}
	b := newBlockingBatcher()
	client := batchEngineClient(t, b)
	out := &syncBuffer{}
	fe := NewFrontend(strings.NewReader(""), out, nil)
	eng, err := NewEngine(EngineConfig{
		Client:   client,
		Tools:    ts,
		Provider: "openrouter",
		ModelID:  "z-ai/glm-5.3-flash:batch",
		Name:     "w1",
		ConvOpts: []llm.ConvOption{llm.NewConversation("", "agent")},
	}, fe)
	if err != nil {
		t.Fatal(err)
	}
	eng.Start()
	fe.handler = eng

	eng.HandlePrompt("go")
	// Park is reached before any frame bracket can end: wait for it, then
	// release. The frame ORDER below is the assertion — the bracket wraps
	// exactly the park.
	<-b.started
	close(b.release)
	eng.Wait()

	// batch_wait_start must precede batch_wait_end, and batch_wait_end must
	// precede the assistant turn. The full expected sequence for this
	// tool-less single-call turn is:
	//   user echo pair, agent_start, batch_wait_start, batch_wait_end,
	//   assistant triple, agent_end, agent_settled.
	assertFrameTypes(t, out.String(), []string{
		"message_start", "message_end", // user echo
		"agent_start",
		"batch_wait_start",                               // parked
		"batch_wait_end",                                 // unparked
		"message_start", "message_update", "message_end", // assistant turn
		"agent_end", "agent_settled"})
}

// recordPark captures Park's arguments, for the resume-adopts test.
type recordPark struct {
	mu     sync.Mutex
	ids    []string
	models []string
	// parkFrom, when non-nil, is called in place of a reply.
	parkFrom func() (*anthropic.Message, error)
}

func (r *recordPark) Park(_ context.Context, customID, model string, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	r.mu.Lock()
	r.ids = append(r.ids, customID)
	r.models = append(r.models, model)
	r.mu.Unlock()
	if r.parkFrom != nil {
		return r.parkFrom()
	}
	return batchEngineReply(), nil
}

// TestEngineBatchResumeAdopts pins "resume adopts": a parked child's engine is
// stopped mid-park (its turn ctx cancelled), and a NEW engine with AutoResume
// on the same conversation re-issues the first call — which must reach Park
// with the SAME customID. Continue's ordinal is history-derived (nextOrdinal
// over the persisted rows), so the re-issued call reproduces the id exactly;
// this fails the moment the customID ever includes anything per-process.
func TestEngineBatchResumeAdopts(t *testing.T) {
	silenceSlog(t)
	if testing.Short() {
		t.Skip("resume-adopts needs a database")
	}
	pool, _ := dbTestPool(t)
	ctx := context.Background()

	const ref = "batch-adopts-test"

	// Engine 1: parks, then is cancelled mid-park — the shape of a daemon
	// restart while a child sits on a batch.
	b1 := &recordPark{parkFrom: func() (*anthropic.Message, error) {
		return nil, context.Canceled // released by the cancel below
	}}
	client1, err := llm.NewClient(
		llm.WithProviderSender("openrouter", neverSender{}),
		llm.WithDefaultModel("openrouter/z-ai/glm-5.3-flash:batch"),
		llm.WithBatcher(b1),
		llm.WithStore(pool),
	)
	if err != nil {
		t.Fatal(err)
	}
	out1 := &syncBuffer{}
	fe1 := NewFrontend(strings.NewReader(""), out1, nil)
	eng1, err := NewEngine(EngineConfig{
		Client:   client1,
		Tools:    fakeToolSet{},
		Provider: "openrouter",
		ModelID:  "z-ai/glm-5.3-flash:batch",
		Name:     "w1",
		ConvOpts: []llm.ConvOption{llm.Entrypoint("agent"), llm.ByExternalRef(ref)},
	}, fe1)
	if err != nil {
		t.Fatal(err)
	}
	eng1.Start()
	fe1.handler = eng1

	eng1.HandlePrompt("go")

	// Wait until Park has actually been reached (the user row is persisted
	// before the send, so by Park time ordinal N is settled), then cancel.
	deadline := time.Now().Add(10 * time.Second)
	for {
		b1.mu.Lock()
		n := len(b1.ids)
		b1.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("engine 1 never reached Park")
		}
		time.Sleep(10 * time.Millisecond)
	}
	firstID := b1.ids[0]
	eng1.HandleAbort()
	eng1.Wait()
	eng1.Close()

	// The conversation must carry the user row (engine 1's write-ahead) —
	// that row is what makes the resumed Continue re-issue ordinal N.
	clientResume, err := llm.NewClient(
		llm.WithProviderSender("openrouter", neverSender{}),
		llm.WithDefaultModel("openrouter/z-ai/glm-5.3-flash:batch"),
		llm.WithBatcher(&recordPark{}), // placeholder; engine 2 gets its own below
		llm.WithStore(pool),
	)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := clientResume.Conversation(ctx,
		llm.Entrypoint("agent"), llm.ByExternalRef(ref))
	if err != nil {
		t.Fatal(err)
	}
	history, err := probe.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) == 0 {
		t.Fatal("engine 1 left no persisted history; the resume test is void")
	}

	// Engine 2: AutoResume on the same conversation. Its Continue re-issues
	// the pending turn and must land on Park with the SAME customID.
	b2 := &recordPark{}
	client2, err := llm.NewClient(
		llm.WithProviderSender("openrouter", neverSender{}),
		llm.WithDefaultModel("openrouter/z-ai/glm-5.3-flash:batch"),
		llm.WithBatcher(b2),
		llm.WithStore(pool),
	)
	if err != nil {
		t.Fatal(err)
	}
	out2 := &syncBuffer{}
	fe2 := NewFrontend(strings.NewReader(""), out2, nil)
	eng2, err := NewEngine(EngineConfig{
		Client:     client2,
		Tools:      fakeToolSet{},
		Provider:   "openrouter",
		ModelID:    "z-ai/glm-5.3-flash:batch",
		Name:       "w1",
		AutoResume: true,
		ConvOpts:   []llm.ConvOption{llm.Entrypoint("agent"), llm.ByExternalRef(ref)},
	}, fe2)
	if err != nil {
		t.Fatal(err)
	}
	eng2.Start()
	fe2.handler = eng2

	deadline = time.Now().Add(10 * time.Second)
	for {
		b2.mu.Lock()
		n := len(b2.ids)
		b2.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine 2 never reached Park (engine 1's id was %q)", firstID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := b2.ids[0]; got != firstID {
		t.Errorf("resume parked with customID %q, want the SAME id %q (resume must adopt)", got, firstID)
	}
	if got := b2.models[0]; got != "z-ai/glm-5.3-flash:batch" {
		t.Errorf("resume parked with model %q, want the :batch id", got)
	}
	eng2.Close()
}
