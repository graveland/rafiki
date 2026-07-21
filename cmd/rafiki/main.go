// rafiki is the standalone LLM capturing proxy: Anthropic /v1/messages and
// OpenAI /v1/chat/completions faces with static bearer-token auth, a
// TimescaleDB conversation store, Prometheus metrics and optional OTLP
// tracing.
//
//	rafiki serve --db postgres://localhost/rafiki --config rafiki.yaml
//	rafiki serve --db postgres://localhost/rafiki --dev   # token "dev", :8035
//	rafiki migrate --db postgres://localhost/rafiki
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"

	"github.com/timescale/rafiki/llm"
	"github.com/timescale/rafiki/routing"
	"github.com/timescale/rafiki/server"
	"github.com/timescale/rafiki/store"

	"github.com/timescale/savannah-common/go/tslogs"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rafiki <serve|migrate> [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serveCmd(os.Args[2:])
	case "migrate":
		err = migrateCmd(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q (want serve or migrate)", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rafiki:", err)
		os.Exit(1)
	}
}

// Config is the deployment config file (env/flags cover local use).
type Config struct {
	// Tokens maps client name -> static bearer token; the name becomes the
	// captured owner identity.
	Tokens map[string]string `yaml:"tokens"`
	// OpenAIRoutes maps model-id prefixes to upstream names on the
	// /v1/chat/completions face ("openrouter" is the built-in default).
	OpenAIRoutes []struct {
		Prefix   string `yaml:"prefix"`
		Upstream string `yaml:"upstream"`
	} `yaml:"openai_routes"`
	DefaultModel string `yaml:"default_model"`
}

func migrateCmd(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	db := fs.String("db", os.Getenv("RAFIKI_DB"), "postgres DSN (or RAFIKI_DB)")
	_ = fs.Parse(args)
	if *db == "" {
		return errors.New("--db (or RAFIKI_DB) is required")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *db)
	if err != nil {
		return err
	}
	defer pool.Close()
	return store.Migrate(ctx, pool)
}

func serveCmd(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	db := fs.String("db", os.Getenv("RAFIKI_DB"), "postgres DSN (or RAFIKI_DB); empty = capture-less")
	listen := fs.String("listen", ":8035", "listen address")
	configPath := fs.String("config", "", "config file (tokens, openai routes)")
	defaultModel := fs.String("default-model", "haiku-latest", "model used when a request has none")
	dev := fs.Bool("dev", false, "dev mode: auto-migrate, accept token \"dev\"")
	_ = fs.Parse(args)

	logger, err := tslogs.NewLogger(tslogs.LevelInfo, false, "rafiki", 0)
	if err != nil {
		return err
	}

	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	if anthropicKey == "" {
		return errors.New("ANTHROPIC_API_KEY is required")
	}
	openrouterKey := os.Getenv("OPENROUTER_API_KEY")

	cfg := Config{DefaultModel: *defaultModel}
	if *configPath != "" {
		raw, err := os.ReadFile(*configPath)
		if err != nil {
			return fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
		if cfg.DefaultModel == "" {
			cfg.DefaultModel = *defaultModel
		}
	}
	if *dev {
		if cfg.Tokens == nil {
			cfg.Tokens = map[string]string{}
		}
		cfg.Tokens["dev"] = "dev"
	}
	if len(cfg.Tokens) == 0 {
		return errors.New("no client tokens configured (config `tokens:` or --dev)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Tracing: standard OTLP env vars; no-op (zero overhead) when unset.
	tp, shutdownTracing, err := setupTracing(ctx, logger)
	if err != nil {
		return err
	}
	defer shutdownTracing()

	// Store (optional): capture-less without --db, auto-migrate with --dev.
	var pool *pgxpool.Pool
	var captureStore *routing.CaptureStore
	if *db != "" {
		pool, err = pgxpool.New(ctx, *db)
		if err != nil {
			return fmt.Errorf("connect store: %w", err)
		}
		defer pool.Close()
		if *dev {
			if err := store.Migrate(ctx, pool); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
			logger.Info("dev mode: store migrated")
		}
		captureStore = routing.NewCaptureStore(pool)
	} else {
		logger.Warn("no --db configured; running capture-less")
	}

	// The llm.Client owns the catalog and per-upstream breakers; the proxy
	// faces consult the SAME breaker (one health signal per process).
	llmOpts := []llm.ClientOption{
		llm.WithUpstream(llm.UpstreamAnthropic, llm.Anthropic(anthropicKey)),
		llm.WithLogger(logger),
		llm.WithDefaultModel(cfg.DefaultModel),
		llm.WithTracerProvider(tp),
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
	llmClient, err := llm.NewClient(llmOpts...)
	if err != nil {
		return err
	}
	go llmClient.Catalog().Warm()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	metrics := server.NewMetrics(reg)
	metrics.WatchBreaker(string(llm.UpstreamAnthropic), llmClient.Breaker(llm.UpstreamAnthropic))

	auth := server.ContextAuthenticator{}
	messages := server.NewMessagesProxy(captureStore, auth, anthropicKey,
		"https://api.anthropic.com", cfg.DefaultModel, llmClient.Catalog(), logger)
	messages.SetMetrics(metrics)
	if openrouterKey != "" {
		messages.SetFallback(openrouterKey, "https://openrouter.ai/api", llmClient.Breaker(llm.UpstreamAnthropic))
	}

	var chat *server.ChatCompletionsProxy
	if openrouterKey != "" {
		routes := make([]server.OpenAIRoute, 0, len(cfg.OpenAIRoutes))
		for _, route := range cfg.OpenAIRoutes {
			routes = append(routes, server.OpenAIRoute{Prefix: route.Prefix, Upstream: route.Upstream})
		}
		chat = server.NewChatCompletionsProxy(captureStore, auth,
			[]server.OpenAIUpstream{{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", APIKey: openrouterKey}},
			routes, "openrouter", logger)
		chat.SetMetrics(metrics)
	} else {
		logger.Warn("OPENROUTER_API_KEY not set; /v1/chat/completions face and failover disabled")
		// A default model only OpenRouter can serve would 502 every defaulted request
		if strings.Contains(cfg.DefaultModel, "/") || slices.Contains(routing.ModelAliases(), cfg.DefaultModel) {
			logger.Warn("default model requires OpenRouter; requests without an explicit model will fail",
				"default_model", cfg.DefaultModel)
		}
	}

	tokenAuth := server.NewStaticTokenAuth(cfg.Tokens)
	mux := http.NewServeMux()
	h := &server.Handler{Messages: messages, Chat: chat}
	h.Mount(mux, func(next http.Handler) http.Handler {
		return tokenAuth.Middleware(traceMiddleware(next))
	})
	// /metrics and /healthz are deliberately OUTSIDE the token middleware:
	// scrapers and probes don't carry client tokens, and standalone deploys
	// sit behind a private network / service mesh (the home deployment is
	// tailnet-only). Only the LLM faces require authentication.
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	logger.Info("rafiki serving", "addr", *listen, "capture", pool != nil,
		"failover", openrouterKey != "", "openai_face", chat != nil, "clients", len(cfg.Tokens), "dev", *dev)

	select {
	case err := <-serveErr:
		return err // listener died on its own
	case <-ctx.Done():
	}
	// Graceful drain: in-flight SSE streams get the full grace period to
	// finish (and their detached-context capture writes to land) before the
	// process exits — a SIGTERM must not strand turns 'pending'.
	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Warn("shutdown did not drain cleanly", "error", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
