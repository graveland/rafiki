package llm

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/providers"
)

// TestForkRoutesEveryWireSessionUse pins that a forked conversation's shared
// routing id reaches every place a session id goes on the wire: the ChatGPT
// session-id header (the one that backend routes its cache on) and
// prompt_cache_key, and a provider's declared session header (OpenRouter's
// x-session-id). A non-fork sends its own conversation id at each of them.
func TestForkRoutesEveryWireSessionUse(t *testing.T) {
	model := minimalRequest().Model
	sum := sha256.Sum256([]byte("node-1\x00" + model))
	forkID := fmt.Sprintf("fork-%x", sum[:16])
	for _, tc := range []struct {
		name, node, want string
	}{
		{"fork", "node-1", forkID},
		{"non-fork", "", "conv-own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			ctx := WithSessionID(context.Background(), "conv-own")
			if tc.node != "" {
				ctx = context.WithValue(ctx, forkCacheNodeKey{}, tc.node)
			}

			var (
				mu      sync.Mutex
				chatHdr http.Header
				chatRaw []byte
				orHdr   string
			)
			chat := newChatGPTResponsesSenderForTest(t, &recordingTokenSource{bearer: "tok"}, nil, func(w http.ResponseWriter, r *http.Request) {
				body := readRequestBody(t, r)
				mu.Lock()
				chatHdr, chatRaw = r.Header.Clone(), body
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, responsesSSE(responsesCompleted(``)))
			})
			_, err := callSender(ctx, chat, minimalRequest(), nil)
			c.Require().NoError(err, "chatgpt send")

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				orHdr = r.Header.Get("x-session-id")
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, anthropicOK)
			}))
			defer srv.Close()
			or, err := SenderForKey(providers.Provider{Name: "openrouter", Kind: providers.KindAnthropicOpenRouter, BaseURL: srv.URL}, "sk-or-1", nil)
			c.Require().NoError(err, "SenderForKey")
			_, err = callSender(ctx, or, minimalRequest(), nil)
			c.Require().NoError(err, "openrouter send")

			mu.Lock()
			defer mu.Unlock()
			c.Eq(tc.want, chatHdr.Get("session-id"), "chatgpt session-id header")
			c.EqDiff(any(tc.want), decodeMap(c, chatRaw, "chatgpt body")["prompt_cache_key"], "prompt_cache_key")
			c.Eq(tc.want, orHdr, "openrouter x-session-id")
			c.True(len(tc.want) <= 64, "prompt_cache_key cap")
		})
	}
}
