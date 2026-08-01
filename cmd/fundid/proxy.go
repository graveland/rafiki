package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/server"
)

// proxyFace is the daemon's own rafiki HTTP face: the same /v1/messages and
// /v1/chat/completions handlers `rafiki serve` mounts, served in-process.
//
// The point is that fundid IS rafiki. The agent kind already reaches the
// library directly through pkg/llm and pkg/routing; pi and claude are separate
// processes and can only speak HTTP, so they need a face to point at — but that
// face has no business being a second daemon someone has to remember to start.
// Serving it here means a child cannot outlive its proxy, there is no
// "unreachable" case to handle, and a child that talks to a provider directly
// stops being something that can happen by accident.
type proxyFace struct {
	URL   string // http://127.0.0.1:<port>
	Token string
	srv   *http.Server
}

// startProxyFace binds an ephemeral loopback port and serves the proxy on it.
//
// Ephemeral by design: nothing outside this process needs to predict the
// address, because the daemon injects it into each child's environment. A fixed
// port would only add a way for two daemons, or a stray `rafiki serve`, to
// collide.
//
// pool may be nil, in which case turns are proxied but not captured — the
// routing, failover and model resolution are still worth having, and refusing
// to start over a missing database would take pi and claude children down for a
// feature they may not use.
func startProxyFace(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (*proxyFace, error) {
	anthropicKey := paths.Get("ANTHROPIC_API_KEY")
	openrouterKey := paths.Get("OPENROUTER_API_KEY")
	if anthropicKey == "" {
		// Not fatal, but the face cannot reach Anthropic without it. Say so
		// once, loudly, naming the fix — the alternative is a child that
		// spawns fine and fails on its first turn with an upstream 401.
		logger.Warn("no ANTHROPIC_API_KEY in the daemon's environment; the proxy face will fail upstream. "+
			"Put it in the environment file, which is not world-readable",
			"env_file", paths.ServiceEnvFile())
	}

	var captureStore *routing.CaptureStore
	if pool != nil {
		captureStore = routing.NewCaptureStore(pool)
	} else {
		logger.Warn("no agent database; proxied turns will be routed but NOT captured", "env", paths.AgentDB)
	}

	llmOpts := []llm.ClientOption{
		llm.WithUpstream(llm.UpstreamAnthropic, llm.Anthropic(anthropicKey)),
		llm.WithLogger(logger),
	}
	if m := paths.Get(paths.DefaultModel); m != "" {
		llmOpts = append(llmOpts, llm.WithDefaultModel(m))
	}
	if pool != nil {
		llmOpts = append(llmOpts, llm.WithStore(pool))
	}
	if openrouterKey != "" {
		llmOpts = append(llmOpts,
			llm.WithUpstream(llm.UpstreamOpenRouter, llm.OpenRouter(openrouterKey)),
			llm.WithBreaker(15*time.Minute),
		)
	}
	client, err := llm.NewClient(llmOpts...)
	if err != nil {
		return nil, fmt.Errorf("build llm client: %w", err)
	}
	go client.Catalog().Warm()

	auth := server.ContextAuthenticator{}
	messages := server.NewMessagesProxy(captureStore, auth, anthropicKey,
		"https://api.anthropic.com", paths.Get(paths.DefaultModel), client.Catalog(), logger)
	if openrouterKey != "" {
		messages.SetFallback(openrouterKey, "https://openrouter.ai/api", client.Breaker(llm.UpstreamAnthropic))
	}

	var chat *server.ChatCompletionsProxy
	if openrouterKey != "" {
		chat = server.NewChatCompletionsProxy(captureStore, auth,
			[]server.OpenAIUpstream{{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: openrouterKey}},
			nil, "openrouter", logger)
	}

	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	// A token even on loopback: any process on this machine can reach an
	// ephemeral local port, and this one forwards to a paid API on the
	// daemon's credentials. Generated per boot, never written anywhere, and
	// handed only to the children that need it.
	tokenAuth := server.NewStaticTokenAuth(map[string]string{"fundi-child": token})

	mux := http.NewServeMux()
	h := &server.Handler{Messages: messages, Chat: chat}
	h.Mount(mux, tokenAuth.Middleware)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("proxy face stopped", "error", err)
		}
	}()

	f := &proxyFace{
		URL:   "http://" + ln.Addr().String(),
		Token: token,
		srv:   srv,
	}
	logger.Info("proxy face listening", "url", f.URL, "captured", pool != nil)
	return f, nil
}

// Close stops the face. Children are shut down before this runs, so there is
// nothing left to serve by then.
func (f *proxyFace) Close(ctx context.Context) {
	if f == nil || f.srv == nil {
		return
	}
	if err := f.srv.Shutdown(ctx); err != nil {
		slog.Warn("proxy face shutdown", "error", err)
	}
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate proxy token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
