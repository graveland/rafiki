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
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/trace"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/ejection"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/rawtrace"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/server"
)

// providerGuardEnabled reports whether the provider cache guard runs. Anything
// but an explicit off-switch leaves it on: a guard that disables itself on a
// typo'd value is worse than no guard, because you would believe you had one.
func providerGuardEnabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "false", "0", "no":
		return false
	default:
		return true
	}
}

// buildProviderGuard constructs the guard and seeds it from the durable
// ejection log. Returns nil when disabled — a nil *ProviderGuard is inert at
// every call site, so callers need no branch.
//
// With no pool the guard still runs, memory-only: it protects the budget for
// this process's lifetime and simply forgets across restarts, which is strictly
// better than not guarding at all.
func buildProviderGuard(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) *routing.ProviderGuard {
	if !providerGuardEnabled(paths.Get(paths.ProviderGuard)) {
		logger.Warn("provider cache guard disabled; a provider that stops serving cache hits will not be ejected",
			"env", paths.ProviderGuard)
		return nil
	}
	guard := routing.NewProviderGuard(routing.DefaultEjectTTL, logger)
	if pool == nil {
		return guard
	}
	guard.SetSink(ejection.NewEjectionStore(pool))
	// Bounded: a slow database delays startup by at most this, and Rehydrate
	// logs and swallows its own failure — ejection state is a cost
	// optimisation, never a reason to refuse to boot.
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	guard.Rehydrate(rctx, time.Now())
	return guard
}

// proxyFace is the daemon's own rafiki HTTP face: the same /v1/messages and
// /v1/chat/completions handlers `rafiki serve` mounts, served in-process.
//
// The point is that rafikid IS rafiki. The agent kind already reaches the
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

// defaultProxyListen is where the face binds unless RAFIKI_PROXY_LISTEN says
// otherwise.
//
// Port 8035 is the one `rafiki serve` has always used, so anything already
// pointed at a local rafiki — `make claude`, an editor plugin, a shell alias —
// keeps working with the daemon serving instead.
//
// All interfaces, not loopback: the point of one face is that everything on the
// network can have the capture store and the failover, including other hosts
// and sentinel. A loopback default would make that a knob to discover rather
// than the behaviour. Auth is mandatory either way — see the token handling in
// startProxyFace, and the warning when the face is reachable off-box with no
// token anyone else could know.
const defaultProxyListen = ":8035"

// childTokenName and configuredTokenName are the two token-map keys the daemon
// assigns itself; a config file naming a client either of these would either
// silently overwrite the per-boot child secret (every real spawned child then
// gets rejected as "unknown token") or the RAFIKI_SERVE_TOKEN slot. Both are
// therefore reserved — see the collision check in startProxyFace.
//
// childTokenName was "fundi-child" before the rename to rafiki; it is a
// captured owner string written to the database, so historical rows keep the
// old value — only newly-generated per-boot tokens use the new name.
const (
	childTokenName      = "rafiki-child"
	configuredTokenName = "configured"
)

// faceOptions carries everything the proxy face needs from the daemon. A struct
// rather than parameters because the fold (config, dev mode, listen override)
// adds fields, and a seven-argument constructor is a bug waiting to happen.
type faceOptions struct {
	Pool        *pgxpool.Pool           // nil = route but do not capture
	Logger      *slog.Logger            // required
	Tracer      trace.TracerProvider    // nil = no-op
	Registry    *prometheus.Registry    // nil = metrics not mounted
	Config      Config                  // named client tokens, openai routes, default model
	Listen      string                  // overrides RAFIKI_PROXY_LISTEN when non-empty
	Catalog     *routing.ModelCatalog   // shared with the Controller (ctrl_get/list's ContextWindow) via llm.WithCatalog; nil = the client builds its own
	RawTrace    *rawtrace.RawTraceStore // nil when no pool configured
	RawTraceAll bool                    // RAFIKI_RECORD_REQUESTS=1: record all sessions unconditionally
}

// startProxyFace binds the proxy face and serves it.
//
// A busy address is a hard error, not a reason to pick another port. The
// address is a contract: clients are configured to find it, and silently
// landing somewhere else would leave them talking to whatever *did* claim
// 8035 — most likely a stale `rafiki serve` with different credentials and a
// different database.
//
// pool may be nil, in which case turns are proxied but not captured — the
// routing, failover and model resolution are still worth having, and refusing
// to start over a missing database would take pi and claude children down for a
// feature they may not use.
func startProxyFace(ctx context.Context, opts faceOptions) (*proxyFace, error) {
	logger := opts.Logger
	pool := opts.Pool
	defaultModel := resolveDefaultModel(opts.Config)
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

	var captureStore *capture.CaptureStore
	if pool != nil {
		captureStore = capture.NewCaptureStore(pool)
	} else {
		logger.Warn("no agent database; proxied turns will be routed but NOT captured", "env", paths.DB)
	}

	llmOpts := []llm.ClientOption{
		llm.WithUpstream(llm.UpstreamAnthropic, llm.Anthropic(anthropicKey)),
		llm.WithLogger(logger),
	}
	if defaultModel != "" {
		llmOpts = append(llmOpts, llm.WithDefaultModel(defaultModel))
	}
	if opts.Catalog != nil {
		llmOpts = append(llmOpts, llm.WithCatalog(opts.Catalog))
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
	if opts.Tracer != nil {
		llmOpts = append(llmOpts, llm.WithTracerProvider(opts.Tracer))
	}
	client, err := llm.NewClient(llmOpts...)
	if err != nil {
		return nil, fmt.Errorf("build llm client: %w", err)
	}
	go client.Catalog().Warm()

	// The provider cache guard is shared by both request paths — the proxy face
	// below and the llm client above — because OpenRouter routes them both and
	// either can be the one that notices a provider has stopped caching. A nil
	// guard is inert at every call site, so the disabled case needs no branching
	// past this point.
	guard := buildProviderGuard(ctx, pool, logger)
	client.SetProviderGuard(guard)

	auth := server.ContextAuthenticator{}
	messages := server.NewMessagesProxy(captureStore, auth, anthropicKey,
		"https://api.anthropic.com", defaultModel, client.Catalog(), logger)
	if openrouterKey != "" {
		messages.SetFallback(openrouterKey, "https://openrouter.ai/api", client.Breaker(llm.UpstreamAnthropic))
	}
	if opts.RawTrace != nil {
		messages.SetRawTrace(opts.RawTrace, opts.RawTraceAll)
	}
	messages.SetProviderGuard(guard)

	var metrics *server.Metrics
	if opts.Registry != nil {
		opts.Registry.MustRegister(
			collectors.NewGoCollector(),
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		)
		metrics = server.NewMetrics(opts.Registry)
		metrics.WatchBreaker(string(llm.UpstreamAnthropic), client.Breaker(llm.UpstreamAnthropic))
		metrics.WatchProviderGuard(guard)
		messages.SetMetrics(metrics)
	}

	var chat *server.ChatCompletionsProxy
	if openrouterKey != "" {
		routes := make([]server.OpenAIRoute, 0, len(opts.Config.OpenAIRoutes))
		for _, r := range opts.Config.OpenAIRoutes {
			routes = append(routes, server.OpenAIRoute{Prefix: r.Prefix, Upstream: r.Upstream})
		}
		chat = server.NewChatCompletionsProxy(captureStore, auth,
			[]server.OpenAIUpstream{{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: openrouterKey}},
			routes, "openrouter", logger)
		if metrics != nil {
			chat.SetMetrics(metrics)
		}
	}

	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	// A token even on loopback: any process on this machine can reach a local
	// port, and this one forwards to a paid API on the daemon's credentials.
	// The per-boot token is generated fresh, written nowhere, and handed only
	// to the children the daemon spawns.
	tokens := map[string]string{childTokenName: token}

	// Named tokens from the config file: each NAME becomes the captured owner
	// identity, which is the one thing an environment variable cannot express.
	// childTokenName and configuredTokenName are reserved — a config entry
	// using either name would silently overwrite the per-boot child secret (or
	// the RAFIKI_SERVE_TOKEN slot below) and every real spawned child would
	// then present the true per-boot token only to be rejected as "unknown
	// token". That is a startup-time configuration error, not something to
	// resolve silently in either direction.
	for name, tok := range opts.Config.Tokens {
		if name == childTokenName || name == configuredTokenName {
			return nil, fmt.Errorf("config token name %q is reserved for the daemon's own use; rename it", name)
		}
		tokens[name] = tok
	}

	// RAFIKI_SERVE_TOKEN names an ADDITIONAL token this face accepts — for
	// humans and tools that are not children (`rafiki claude`, an editor
	// plugin, curl). They cannot know a per-boot secret.
	//
	// Unconditional, unlike the variable it replaces: the old
	// FUNDI_PROXY_TOKEN meant "accept this" when serving and "send this"
	// when pointing away, so it needed a guard to tell which. Two names, no
	// guard.
	if extra := paths.Get(paths.ServeToken); extra != "" {
		tokens[configuredTokenName] = extra
	}
	tokenAuth := server.NewStaticTokenAuth(tokens)

	mux := http.NewServeMux()
	h := &server.Handler{Messages: messages, Chat: chat}
	h.Mount(mux, func(next http.Handler) http.Handler {
		return tokenAuth.Middleware(traceMiddleware(next))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	// /metrics and /healthz sit deliberately OUTSIDE the token middleware:
	// scrapers and probes do not carry client tokens. Only the LLM faces
	// require authentication.
	if opts.Registry != nil {
		mux.Handle("/metrics", promhttp.HandlerFor(opts.Registry, promhttp.HandlerOpts{}))
	}

	addr := opts.Listen
	if addr == "" {
		addr = paths.Get(paths.ProxyListen)
	}
	if addr == "" {
		addr = defaultProxyListen
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s (is a `rafiki serve` or another rafikid already there?): %w", addr, err)
	}
	if offBox(addr) && paths.Get(paths.ServeToken) == "" {
		// Reachable from the network but the only valid token is the per-boot
		// one, which only this daemon's own children ever see. Nothing else can
		// authenticate, so the face is off-box in name only — and the operator
		// who opened it up plainly meant otherwise.
		logger.Warn("proxy face is reachable off-box but no shared token is set, "+
			"so only this daemon's own children can use it",
			"addr", addr, "set", paths.ServeToken, "in", paths.ServiceEnvFile())
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("proxy face stopped", "error", err)
		}
	}()

	// Children get a loopback URL with the real port, NOT ln.Addr(): binding
	// every interface reports "[::]:8035", which is a valid bind address and a
	// useless destination. Children are local by construction, so loopback is
	// both correct and the shortest path.
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return nil, fmt.Errorf("resolve proxy port from %s: %w", ln.Addr(), err)
	}
	f := &proxyFace{
		URL:   "http://127.0.0.1:" + port,
		Token: token,
		srv:   srv,
	}
	logger.Info("proxy face listening",
		"addr", ln.Addr().String(), "children_use", f.URL, "captured", pool != nil)
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

// startBroadcastListener binds a dedicated HTTP listener for OpenRouter's OTLP
// broadcast webhook, gated by RAFIKI_BROADCAST_LISTEN. Returns (nil, nil) when
// the var is unset (the feature is off). The listener runs on its own port with
// no auth — it receives only from OpenRouter, not rafiki clients.
func startBroadcastListener(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (*http.Server, error) {
	addr := paths.Get(paths.BroadcastListen)
	if addr == "" {
		return nil, nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", server.HandleOTLP(pool, logger))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("broadcast listener on %s: %w", addr, err)
	}

	go func() {
		logger.Info("broadcast listener started", "addr", addr)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("broadcast listener stopped", "error", err)
		}
	}()

	return srv, nil
}

// resolveDefaultModel picks the model used when a request names none: the
// config file's default_model wins when set, otherwise the environment
// variable (RAFIKI_DEFAULT_MODEL) is the fallback, matching `rafiki serve`'s
// own config-over-env precedence.
func resolveDefaultModel(cfg Config) string {
	if cfg.DefaultModel != "" {
		return cfg.DefaultModel
	}
	return paths.Get(paths.DefaultModel)
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate proxy token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// offBox reports whether a listen address is reachable from outside this
// machine. An empty or wildcard host binds every interface; anything else that
// is not a loopback literal is a specific external address.
func offBox(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true // unparseable: assume the riskier reading
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip == nil || !ip.IsLoopback()
}
