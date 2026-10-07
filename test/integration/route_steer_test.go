// SPDX-License-Identifier: Apache-2.0

package integration_test

// End-to-end proof of routing STEERING: a mid-run `rafiki route set` changes
// the provider object on the fundi child's NEXT upstream request, and a child
// credential may steer its own subtree's prefer/sort/quant but never only=.
//
// The assertions are on the recorded UPSTREAM REQUEST BODY, not on CLI output
// or on the child's stored spec: the whole point of the feature is that the
// steer reaches the wire, and only the body proves it did. The fake
// OpenRouter is an in-process httptest server the daemon dials through a
// providers.toml entry (kind = "anthropic-openrouter", so the non-standard
// top-level "provider" field is built at all); it records every /v1/messages
// body verbatim and answers a canned SSE end_turn reply (a plain JSON body
// parses as an empty stream and fatals the turn — see restart_resume_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// recordingOpenRouter is a stand-in for OpenRouter that records every
// /v1/messages request body and answers a minimal SSE end_turn stream.
type recordingOpenRouter struct {
	srv *httptest.Server

	mu     sync.Mutex
	bodies [][]byte
}

func newRecordingOpenRouter(t *testing.T) *recordingOpenRouter {
	t.Helper()
	f := &recordingOpenRouter{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, ev := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_fake_1","type":"message","role":"assistant","model":"mini","content":[],"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n",
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}` + "\n\n",
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n",
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}` + "\n\n",
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
		} {
			_, _ = w.Write([]byte(ev))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// requests returns a copy of every recorded request body, in order.
func (f *recordingOpenRouter) requests() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.bodies))
	copy(out, f.bodies)
	return out
}

// waitForRequests polls until at least n bodies have been recorded and
// returns them all, so a test never asserts on a turn still in flight.
func (f *recordingOpenRouter) waitForRequests(t *testing.T, n int, timeout time.Duration) [][]byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last int
	for time.Now().Before(deadline) {
		if got := f.requests(); len(got) >= n {
			return got
		} else {
			last = len(got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("fake OpenRouter saw %d request(s), want at least %d", last, n)
	return nil
}

// providerObject decodes the request body's top-level "provider" object.
func providerObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("upstream request body %s is not JSON: %v", body, err)
	}
	p, ok := m["provider"].(map[string]any)
	if !ok {
		t.Fatalf("upstream request body carries no provider object: %s", body)
	}
	return p
}

// providerObjectWithoutGlobalBans is providerObject with the "ignore" key
// removed.
//
// Provider bans are GLOBAL state in the shared disposable database: a daemon
// rehydrates every live ban into the provider object of every request it
// serves. TestCLI_ProviderBanRoundTrip holds a ban for the length of its round
// trip and runs in PARALLEL with this file, so an "ignore" entry naming some
// other test's provider slug can appear here at any moment without the routing
// under test having changed. Excluding that one key keeps the comparison exact
// on everything this test actually controls — which is the point: the steer's
// own keys must match precisely, including the ABSENCE of "order" before the
// steer is applied.
func providerObjectWithoutGlobalBans(t *testing.T, body []byte) map[string]any {
	t.Helper()
	p := providerObject(t, body)
	delete(p, "ignore")
	return p
}

// writeRecordingProviders writes a providers.toml whose "fakeor" provider is
// an anthropic-openrouter entry pointed at the recording server. The file
// REPLACES the shipped registry, so the shipped anthropic entry is spelled out
// too; an invalid registry silently falls back to the shipped defaults, which
// would strand the test on the real OpenRouter.
func writeRecordingProviders(t *testing.T, baseURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.toml")
	body := fmt.Sprintf(`default_provider = "anthropic"

[providers.anthropic]
kind = "anthropic"
api_key_env = "ANTHROPIC_API_KEY"

[providers.fakeor]
kind = "anthropic-openrouter"
base_url = %q
`, baseURL)
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte(body), 0o600), "write providers fixture")
	return path
}

// sendPrompt sends one user prompt to a fundi child over the daemon's control
// socket, which drives a real model turn.
func sendPrompt(t *testing.T, client rafikiv1connect.ControlClient, childID, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := client.Send(ctx, connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: childID,
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
		}},
	}))
	assert.NewAborting(t).NoError(err, "Send(%q)", text)
}

// TestRouteSteerChangesNextRequestProviderObject is the end-to-end steer: a
// fundi child spawned with sort=price sends its first request carrying
// provider {"sort":"price"}; after `rafiki route set <child> prefer=fireworks`
// its NEXT request carries both {"order":["fireworks"]} and {"sort":"price"};
// and the first, already-sent request body is byte-for-byte unchanged (a steer
// is never retroactive).
func TestRouteSteerChangesNextRequestProviderObject(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	fake := newRecordingOpenRouter(t)
	providers := writeRecordingProviders(t, fake.srv.URL)

	d := bootDaemonDB(t, nextDaemonID(), append(noRealProviderEnv(),
		"RAFIKI_EXECUTORS_ENABLED=0",
		"RAFIKI_PROVIDERS="+providers,
		"RAFIKI_EVENTBUF_DEBOUNCE_MS=250",
		"RAFIKI_EVENTBUF_MAX_WAIT_MS=1500",
	)...)
	t.Cleanup(func() {
		d.stopDaemonNoRemove()
		os.RemoveAll(d.homeDir)
	})

	client := d.connectClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.Spawn(ctx, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:   "/tmp",
		Kind:  protocol.KindFundi,
		Model: "fakeor/mini[sort=price]",
		Name:  "steer-target",
	}))
	c.NoError(err, "spawn")
	child := resp.Msg.GetChildId()
	c.NotEq("", child, "spawn returned no childId")

	// 1. First request: the spawn-time spec's sort reaches the wire.
	sendPrompt(t, client, child, "first turn")
	first := fake.waitForRequests(t, 1, 60*time.Second)
	firstBody := append([]byte(nil), first[0]...)
	c.EqDeep(map[string]any{"sort": "price"}, providerObjectWithoutGlobalBans(t, firstBody),
		"first request provider object, body = %s", firstBody)

	// 2. Steer the running child. Merge semantics leave sort=price in place.
	out, err := cliCmd(t, d, "route", "set", child, "prefer=fireworks").CombinedOutput()
	c.NoError(err, "rafiki route set failed: %v\noutput: %s", err, out)

	// 3. Second request: the steer applies to the NEXT request.
	sendPrompt(t, client, child, "second turn")
	second := fake.waitForRequests(t, 2, 60*time.Second)
	wantSecond := map[string]any{"order": []any{"fireworks"}, "sort": "price"}
	c.EqDeep(wantSecond, providerObjectWithoutGlobalBans(t, second[1]),
		"second request provider object, body = %s", second[1])

	// 4. The steer is not retroactive: the first recorded body is untouched.
	c.True(bytes.Equal(firstBody, second[0]),
		"the first recorded request body changed after the steer:\nbefore: %s\nafter:  %s", firstBody, second[0])

	// Cleanup: kill the child so the daemon's shutdown does not wait it out.
	kctx, kcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer kcancel()
	_, _ = client.Kill(kctx, connect.NewRequest(&rafikiv1.KillRequest{
		ChildId:         child,
		ShutdownTimeout: durationpb.New(2 * time.Second),
		KillTimeout:     durationpb.New(2 * time.Second),
	}))
}

// TestRouteSteerChildCannotSetOnlyOverConnect pins the child-credential
// authority boundary for steering, on the real daemon: a per-child MCP token
// (the same credential connect_childscope_test.go uses) may steer a descendant
// with prefer=, but only= is refused PermissionDenied — it would sidestep the
// operator's provider bans.
func TestRouteSteerChildCannotSetOnlyOverConnect(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)

	d, dumps := bootMCPChildDaemon(t)
	userToken := d.createMCPUser(t)
	userSess := mcpConnect(t, d.proxyURL, userToken)

	// The caller: a real claude child with a real per-child MCP secret.
	childA := mcpSpawnClaudeChild(t, userSess, "steer-scoped-a")
	dump := waitClaudeDump(t, d, dumps, childA)
	mcpToken := dump.envValue("RAFIKI_MCP_TOKEN")
	c.NotEq("", mcpToken, "the spawned claude child's environment carries no RAFIKI_MCP_TOKEN")

	// The target: the caller's own descendant.
	kid := d.spawnChildUnder(t, childA)

	client := d.connectClient()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// only= from a child credential: refused, even on its own descendant.
	onlyReq := connect.NewRequest(&rafikiv1.SetRoutingRequest{ChildId: kid, Delta: "only=fireworks"})
	onlyReq.Header().Set("Authorization", "Bearer "+mcpToken)
	if _, err := client.SetRouting(ctx, onlyReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("child SetRouting only= = %v, want %v", err, connect.CodePermissionDenied)
	}

	// prefer= from the same credential: allowed, and stored canonically.
	preferReq := connect.NewRequest(&rafikiv1.SetRoutingRequest{ChildId: kid, Delta: "prefer=fireworks"})
	preferReq.Header().Set("Authorization", "Bearer "+mcpToken)
	resp, err := client.SetRouting(ctx, preferReq)
	c.NoError(err, "child SetRouting prefer=")
	c.Eq("prefer=fireworks", resp.Msg.GetRouting(), "stored spec after the child steer")
	c.Eq(kid, resp.Msg.GetChildId(), "SetRouting echoed the wrong child id")

	// A sibling tree the caller must never reach: the subtree gate, not the
	// only= rule, is what refuses this one.
	outsider := d.spawnChild(t)
	siblingReq := connect.NewRequest(&rafikiv1.SetRoutingRequest{ChildId: outsider, Delta: "prefer=fireworks"})
	siblingReq.Header().Set("Authorization", "Bearer "+mcpToken)
	if _, err := client.SetRouting(ctx, siblingReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("child SetRouting on a sibling = %v, want %v", err, connect.CodePermissionDenied)
	}
}
