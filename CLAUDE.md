# rafiki — project instructions

**Update this file when you learn something that would save significant time or tokens next time** — a gotcha, an architectural invariant, a footgun, a convention that isn't obvious from the code. Don't log history or one-off facts; only actionable current-state knowledge. Remove entries once the underlying issue is fixed.

**Keep documentation in sync with code, always.** Any change to the wire protocol, CLI commands, config/env vars, or public behavior must update the relevant doc in the same change: `docs/reference/control-protocol.md` (wire protocol), `docs/agent-cli.md` (`rafikid agent` verbs), `README.md` (architecture/setup), `.env.example` (env vars). Documentation that drifts from code is worse than no documentation — don't defer doc updates to "later."

- **Two binaries, split by dependency: `rafiki` is a socket client that must
  never open a postgres connection; `rafikid` owns every DSN.** That is the
  whole point of the split and the easiest invariant for a future change to
  violate silently. `cmd/rafiki` links **zero** pgx packages, enforced by
  `TestClientDoesNotLinkPostgres`. `rafiki` is also the EXECUTOR
  (`rafiki executor serve`) — `cmd/rafiki-executor` is retired, and its absence
  is deliberate: two artifacts, one client and one server. Folding it in required `pkg/executor` to become pgx-free too, so that
  test now covers the executor as well; `pkg/executor/no_postgres_test.go` keeps
  the invariant attached to the packages so it survives further rearranging.
  Several concrete changes got the client there: it asks the daemon for model
  information over
  `ctrl_model_info` instead of reading the OpenRouter catalog itself (the `claude`
  subcommand no longer imports `pkg/routing`); `CaptureStore`,
  `RawTraceStore`, and `EjectionStore` have moved out of `pkg/routing` so the
  catalog helpers (`ModelCatalog`, `AutoCompactWindow`, `PrefixHash`, etc.) are
  DB-free — `pkg/routing` itself now links zero pgx packages; and a new
  `pkg/conversationview` package extracts the shared rendering from `pkg/agentcli`
  using the pgx-free `pkg/insightstypes` types, so `cmd/rafiki` (the client)
  also links zero pgx. `pkg/executors` is also DB-free. The linker-enforcement
  test is `TestClientDoesNotLinkPostgres` in `cmd/rafiki/no_postgres_test.go`.
- **`fundi` is not a retired name — it names the native agent runtime**, one of three child kinds. `pkg/fundi/`, `protocol.KindFundi`, `--kind fundi`, and `rafikid fundi` are all correct and must survive any sweep.
- **`go test` caching cannot see through to the daemon.** `test/integration` builds the daemon binary via a subprocess inside `TestMain`, so its import graph (`go list -json ./test/integration/` → `XTestImports`) contains only `pkg/protocol`. A change to `pkg/paths`, `pkg/control`, or anything else the daemon reads at startup will **not** invalidate a cached PASS. Always use `-count=1` when changing daemon startup behaviour, or a stale `(cached)` result will hide a real break.
- **`make check` does not run `make build-attach`.** The TypeScript TUI can be fully broken while the Go gate is green — this has already happened once, when a directory move under `cmd/` broke a relative import in `attach/src/`. Anything that moves files under `cmd/` must check `attach/src/` for imports reaching into them.
- **Constants are duplicated across the Go/TypeScript boundary with nothing enforcing agreement.** `APP_NAME` exists in `pkg/paths/paths.go`, `attach/src/client.ts`, and `cmd/rafiki/helpersembed/rafiki-helpers/index.ts`; the `RAFIKI_*` variable names are documented in a comment in `pkg/paths/envvar.go` for the same reason. Neither the Go compiler nor `tsc` will catch a drift. Change one, grep for the others.
- **A `reason="client canceled"` abort with a small `last_byte_ago` is Claude Code's stream idle watchdog, not a proxy stall.** Verified against the v2.1.220 binary: Claude Code re-arms the 300s idle watchdog (`CLAUDE_STREAM_IDLE_TIMEOUT_MS`, a `max(env, 300000)` floor — raise-only) on every event its consume loop yields, and SSE pings *do* feed it — but only via the byte monitor (`_chunkTimes` + the `b1y` heartbeat that synthesizes ping events from raw socket activity), which it attaches **only when `new URL(ANTHROPIC_BASE_URL).host === "api.anthropic.com"`** (the `Yd→d6r→T1e` check; `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1` overrides). The SDK itself never yields ping events, so through any proxy on a custom host the watchdog is fed only by content events, and a thinking phase with >300s between content events dies with "Response stalled mid-stream" even though the proxy flushed bytes seconds earlier. This is why it "never happens without rafiki": direct, the byte monitor makes content-silent streams unkillable. `pkg/proxyenv` sets the override for rafiki-spawned claude children (`Defaults`); hand-configured clients need it in their own env (README). Before blaming the byte path, check the log for `ResponseWriter is not an http.Flusher` / `h2 transport configure failed` and `llm turn truncated` — if all absent and `last_byte_ago` is small, this is the diagnosis.

- **`_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1` also silently re-enables deferred tools (tool search), which 400s every non-Anthropic model.** Claude Code gates the byte watchdog *and* tool search on the same `Yd()` predicate (v2.1.220 `s3()`: `!ENABLE_TOOL_SEARCH && provider==="firstParty" && !Yd()`), so the watchdog fix in `pkg/proxyenv.Defaults` costs you the guard that would otherwise turn tool search off behind a custom base URL. With it on, Claude Code omits tools from `tools[]` and sends `tool_reference` blocks, and the first turn on an OpenRouter-routed model returns `400 Deferred custom tools are only supported on Anthropic models`. `proxyenv.Claude` sets `ENABLE_TOOL_SEARCH=false` when `anthropicModel(o.Model)` is false. That predicate is a string-shape test, not a catalog lookup — a new *Anthropic* id spelling that neither starts with `claude` nor ends in `-latest` would wrongly lose tool search (harmless, costs tokens); a non-Anthropic id that sneaks past it breaks the session outright.

- **Claude Code's OAuth gate does NOT look at `ANTHROPIC_BASE_URL` — only `ANTHROPIC_AUTH_TOKEN`/`ANTHROPIC_API_KEY` suppress the subscription credential.** Verified empirically against 2.1.226 and in the bundle: the auth decision is `pT()`, which gates on `Ru()` → `Kn()==="firstParty"`, and `Kn()` reads only the Bedrock/Vertex/Foundry/gateway variables. The base-URL host check is a *different* predicate, `hf()`/`urn()`, which gates fast mode, error reporting and the byte watchdog — not auth. So a plain-HTTP `ANTHROPIC_BASE_URL=http://127.0.0.1:8035` with no `ANTHROPIC_AUTH_TOKEN` receives `Authorization: Bearer sk-ant-oat01-…` plus `anthropic-beta: …,oauth-2025-04-20,…`, and Anthropic accepts that credential forwarded from an ordinary Go client (no TLS-fingerprint or User-Agent check — `buildAndDo`'s four-header reconstruction is sufficient). This is what `rafiki claude --passthrough-auth` rests on. Two corollaries: the OAuth credential arrives in `Authorization`, **never** `x-api-key`; and dropping `oauth-2025-04-20` from `anthropic-beta` on the way upstream turns a working request into a 401, so the header forward in `buildAndDo` is load-bearing.

- **`conversations.raw_http_request` stores bodies in JSONB, but LLM traffic is frequently not JSON — `nilJSON` in `pkg/rawtrace/rawtrace.go` wraps any non-JSON payload as a JSON *string*.** An SSE `text/event-stream` body (the proxy's normal streaming path) and an HTML 502 from a load balancer are both invalid JSON; inserting them raw makes Postgres reject the row with `invalid input syntax for type json`. `RawTraceStore.Insert` is deliberately best-effort — it logs the failure at warn and returns nil — so a regression here is **silent**: the feature records nothing and nothing errors. When querying `resp_body`, expect either a JSON object (upstream JSON responses) or a JSON string containing the raw text (SSE, error pages), and never assume the former.

- **A `Materializer` may decline by returning `(nil, nil)`; `MaterializeAll` then omits it from the Registry.** This is how `SkillBlueprint` keeps a useless `skill` tool out of `tools[]` when zero skills were discovered (also what `--no-skills` reduces to) — a skill tool over an empty inventory can only ever answer `unknown skill %q; available skills: `, burning a turn. Note `Registry.Register` does **not** guard against nil: it panics inside `BuildDef` on a nil-interface deref, so any new caller that materializes a blueprint by hand must nil-check before registering. Tests that materialize `SkillBlueprint` directly must pass a non-empty `Skills` or they will now get nil back. The largest instance of the pattern is not a blueprint at all: the whole workspace tier (`read`, `write`, `edit`, `glob`, `grep`, `ls`, `bash`, `lsp_*`) is omitted from `MaterializeAll` when `opts.Executor == nil` — the rule the executor architecture exists to establish (see `TierOf`/`TierWorkspace`).

- **A TCP/TLS control connection must have exactly one `protocol.FrameReader` for its whole lifetime.** `FrameReader` owns a 64 KiB `bufio.Reader`, so a reader used for the `ctrl_auth` handshake has very likely already buffered bytes past the auth frame's `\n` — `client.DialURL` returns immediately after writing auth, so a caller's first request routinely arrives in the same TCP segment. Building a second reader for the request loop silently discards that pipelined request and the client hangs to its 30s timeout. `authHandshake` therefore returns its reader and `handleConn` reuses it (nil means the UDS path, which has no handshake and allocates its own). Any test that sleeps between dial and first request will pass regardless — exercise it with a single `Write` carrying both frames.

- **This dev environment wraps `go test` (and other dev commands) through `rtk`, which compacts the output to a bare pass count.** A failing run still surfaces, but per-test names, `=== RUN` markers, and — critically — **SKIP lines** are swallowed, so a suite that silently skipped everything looks identical to one that passed. When you need real Go test output (verifying a test actually ran, reading a panic trace, using `-json`), invoke the toolchain directly: `/usr/local/go/bin/go test ...`. This has already misled two separate sessions.

- **ripgrep only honours `.gitignore` inside a tree that contains a `.git` directory** — outside one it ignores the file entirely unless `--no-require-git` is passed. `pkg/fundi/tools/discovery.go` therefore passes `--no-require-git` on every invocation. This is invisible in a normal repo checkout and only bites in tests, whose fixtures are bare `t.TempDir()` trees: without the flag, a fixture asserting "the gitignored file is excluded" fails, and worse, a fixture asserting the opposite would pass for entirely the wrong reason. Never pass `-L` (symlink following lets rg escape the search root into module caches or the nix store and chase cycles) and never pass `--no-ignore` (it defeats the point).

- **A tool's `ToolResult` is flattened to a `string` inside `Registry.Register`'s closure, not inside `Registry.Execute`.** `Execute` looks like the conversion point and is not — it invokes a pre-built `func(context.Context, json.RawMessage) (string, error)`. Anything that changes how results become model-visible content (content blocks, images, per-tool budgets) belongs in the `Register` closure. Note also that a non-text result cannot survive the `string` return in `Registry.Execute`, `agentloop.ToolSet.Execute`, **and** `anthropic.NewToolResultBlock` — all three would need to change together.

- **An in-band SSE `error` event never becomes a typed `*anthropic.Error` — the SDK's ssestream wraps the raw JSON body in a plain `fmt.Errorf("received error while streaming: %s", body)`, and nothing downstream can classify it.** OpenRouter's Anthropic-compatible face emits exactly this when its upstream dies mid-request (gemini's `timeout_error` / "The operation was aborted"), and because it carries no status code, no net error, and no known string fragment, it sailed past every retry seam: `pkg/llm`'s rate-limit loop (429-only), breaker/failover (`FailoverWorthy` saw a status-less error, but fundi conversations configure NO fallback chain, so there was nothing to fail over to), and `agentloop.isRetryable` — so one aborted upstream call ended the whole turn. `llm.ParseStreamError`/`llm.IsTransientStreamError` (`pkg/llm/streamerr.go`) recover the embedded payload (`timeout_error`, `overloaded_error`, `api_error`, or any code ≥ 500 → transient; everything else fails fast); `agentloop.isRetryable` consults them. A `rate_limit_error` in-band is NOT transient-classified: `llm.IsRateLimitStreamError` (same file, also matches OpenRouter's native `rate_limit_exceeded` spelling) routes it to `sendStreaming`'s rate-limit loop — ModelGate backpressure across all conversations — while `agentloop.isRetryable` returns false for it, mirroring its typed-429 exclusion; one rejection takes exactly one retry seam. Any NEW classifier must go through these, not string-match the wrapper — and never stringify an `*anthropic.Error` while doing it (`Error()` dereferences nil Request/Response on a status-only value; see routing/classify.go's RawJSON caveat).

- **Setting `Accept-Encoding: gzip` by hand on an outbound request DISABLES Go's transparent decompression.** `http.Transport` only adds the header and gunzips the response when the request's Accept-Encoding is empty; set it yourself and you receive raw gzip bytes with no error anywhere. This shipped in `searchBrave` and made every Brave-keyed `websearch` fail with `invalid character '\x1f' looking for beginning of value`. A fixture server that answers plain JSON cannot catch it — any test guarding a client against this must actually gzip its body and set `Content-Encoding: gzip`.

- **Custom auth headers survive cross-host redirects; only `Authorization`/`WWW-Authenticate`/`Cookie` are stripped.** `X-Subscription-Token` (Brave) was forwarded intact to a redirect target. Outbound clients in `pkg/fundi/tools` therefore go through `newGuardedClient` (dialer `Control` hook, the only place an address check survives DNS rebinding *and* redirects) and, for credentialed calls, `newBraveClient`, which wraps rather than replaces the existing `CheckRedirect` and deletes the credential on a host change. Any NEW outbound client belongs on that path — `websearch` originally built bare `&http.Client{}` literals and had neither guard.

- **`rtk` has no distinct exit code for "I refuse to run this"** (verified against 0.43.0): refusals and genuine tool failures both exit nonzero (1/128/254/255 on both sides). The only reliable discriminator is that rtk prefixes its OWN diagnostics with `rtk: ` on stderr while passing the underlying tool's stderr through untouched — that is what `rtkRefused` in `pkg/fundi/tools/bash.go` keys on to re-run the original command under plain bash. Bias it toward NOT falling back: a false positive re-runs a failing command twice.

- **Teeing a subprocess's stderr into a buffer that stdout also writes to is a data race.** `exec.Cmd.CombinedOutput` gets away with one buffer only because it makes `Stdout` and `Stderr` the *identical* writer, which `os/exec` identity-checks to share a single pipe. Point them at a buffer plus an `io.MultiWriter` over that same buffer and you get two goroutines writing concurrently — which silently drops output as well as tripping `-race`. `pkg/fundi/tools/bash.go` uses a mutex-guarded `syncWriter` for this.

- **Quoting rules differ between `'` and `"` and the rtk rewrite guard must honour that.** Inside single quotes every shell metacharacter is inert; inside double quotes bash still expands `$`, `` ` ``, `$( )` and honours `\`. `hasShellChaining` refuses `$`/backtick/backslash inside `"…"` while leaving `;`/`|`/`&`/`<`/`>` inert there. Treating both quote kinds alike (the original bug) let `git commit -m "release $VERSION"` reach rtk's argv and commit the literal string.

- **Deleting a git worktree leaves golangci-lint's cache keyed to the dead path, and the next `make check` in a DIFFERENT worktree reports phantom failures.** The errors cite files by their old absolute path and are accompanied by `failed to parse file: ... no such file or directory` warnings — the finding itself ("Error return value of `c.sf.Do` is not checked" in `pkg/routing/orcatalog.go`) looked entirely real while the file was clean. `golangci-lint cache clean` fixes it. Suspect this whenever `make check` fails on code you did not touch, especially right after a `git worktree remove`.

- **The provider cache guard is observational because the catalog cannot answer the question.** OpenRouter's endpoints API exposes `supports_implicit_caching`, which looks exactly like the flag you'd filter providers on. It is useless: it is `false` for Novita, which served 98-99% of input tokens from cache for days, and `true` only for the first-party DeepSeek endpoint. Every third-party endpoint also publishes an `input_cache_read` price whether or not it ever delivers a hit. There is likewise no `provider` routing field for requiring caching (`require_parameters` covers request parameters, and DeepSeek caching is implicit). So `routing.ProviderGuard` learns from observed `cache_read_tokens` instead — anyone tempted to replace it with a catalog lookup will find the catalog does not know.

- **`ProviderGuard` is inert without capture, by design.** It qualifies a cache miss using `prefix_hash` and the conversation id, both of which come from the capture path; with capture disabled it receives empty values, nothing qualifies as evidence, and it ejects nothing. That is the intended fail-safe (no evidence beats false evidence), not a bug to "fix" by loosening the qualification rules. The rules are pinned by replay tests over real traffic in `pkg/routing/testdata/`: loosening them makes `TestReplayHealthyNovitaNeverEjects` blacklist a provider that was working fine.

- **One credential scheme: `base64url(sha256(token))` in an indexed TEXT
  column, for BOTH `users` and `executors`.** Not bcrypt, and the reason is
  not just speed: bcrypt salts per row, so a digest cannot be looked up and
  every authentication becomes a full-table scan of bcrypt comparisons — paid
  per HTTP request on the proxy face, which authenticates per request rather
  than per connection. The tokens are 256 bits of `crypto/rand`, so a work
  factor defends nothing. `users.HashToken` and `executorsdb.hashToken` must
  stay identical; the `executors.credential_hash` column comment still says
  "bcrypt/argon2 digest" and has always been wrong.
- **`users` rows are TOMBSTONED, never deleted, and joins must be by id.**
  `deleted_at` keeps history resolving to a real username and avoids an
  `ON DELETE SET NULL` rewrite across compressed hypertable chunks. The
  consequence: `username` is unique only among ACTIVE users (partial index
  `users_username_active`), so one name can belong to one active row plus any
  number of tombstones. A query that joins `users` by name will eventually
  return multiple rows — `pkg/insights/search.go`'s owner filter carries an
  `ORDER BY created_at DESC LIMIT 1` for exactly this.
- **An auth failure and an auth OUTAGE are different answers on both
  surfaces.** `users.ErrNotFound` → 401 / `ERR_AUTH_INVALID`. Any other store
  error → 503 / `ERR_INTERNAL`, never 401: a 401 tells clients their
  credential is bad and they respond by discarding it, so answering a database
  blip with 401 logs out everyone at once. The store's error text never
  reaches the peer — it has not proved who it is and a pgx error carries the
  DSN. Same rule as `executors.IsTerminalAuthError`.
- **The daemon has no users until someone creates one, and that window is
  open on every listener.** With no active user a TLS control connection is
  admitted WITHOUT a token and may send only `ctrl_user_create` — first writer
  claims the daemon. Deliberate (a k8s pod has no operator shell on its UDS);
  bounded by operator sequencing, a per-minute WARN, and a logged peer
  address. `RAFIKI_CONTROL_LISTEN` without `RAFIKI_DB` is fatal at startup,
  because otherwise the listener sits in permanent bootstrap mode.
- **A hypertable with columnstore rejects `ADD COLUMN ... REFERENCES`, but
  accepts the same FK as a separate `ADD CONSTRAINT`.** The inline form fails
  with "cannot add column with constraints to a hypertable that has columnstore
  enabled"; `ALTER TABLE … ADD COLUMN x UUID` followed by `ALTER TABLE … ADD
  CONSTRAINT … FOREIGN KEY (x) REFERENCES …` is accepted and propagates to the
  chunks (verified: an insert of an unknown id is rejected by the chunk, not the
  parent). `0019_user_attribution` does exactly this for
  `conversation_turn.author_user_id`. Never "solve" this by dropping columnstore
  or by leaving the column app-enforced.

- **`conversations.v_analysis` and `v_finding` do NOT depend on
  `v_conversation`** — they read `conversation_analysis` and `analysis_finding`
  directly, so `DROP VIEW v_conversation CASCADE` reaches only `v_turn`
  (`pg_depend` confirms). Two design docs in this repo claim otherwise. When you
  rebuild the views, drop and recreate only the two that actually select the
  changed columns; retyping the other two is regression surface for nothing. The
  view-body chain is now 0007, 0008, 0011, 0019 — review all four before
  touching `v_turn`, whose ~10 pricing expressions this repo has no other copy
  of. `pg_get_viewdef('conversations.v_turn', true)` before and after is the
  cheap way to prove you changed only what you meant to. `owner_canonical` and
  `owner_kind` (present in the 0007/0008 view bodies) no longer exist as of
  0019 — they guessed an identity out of free text, which a `users` join now
  answers directly.

- **The migration chain must be contiguous 1..N, and `Migrate` skips by version
  NUMBER, not name.** `loadMigrations` hard-fails on a gap or a duplicate, so a
  branch that numbers a migration into a slot another branch already used will
  not load at all — and worse, if it *does* load, a number already applied to a
  live database is silently never applied while looking correct on a fresh one.
  Before adding a migration, check the max version in the **target database**,
  not just the highest file on your branch. This has bitten once already, when
  `main` and `agent-platform` both claimed 13.

- **No `Co-Authored-By` trailers in commit messages.**
- **`make check` (vet + golangci-lint + unit tests, `-race`) is the only gate — there is no CI on this repo.** Run it before considering anything done.
- **DB-backed tests silently skip without `RAFIKI_TEST_DSN`** — a green `go test ./...` does not mean the store/insights/agentcli code was exercised. Source `.env` first (`set -a; . ./.env; set +a`) and check the skip count, not just the exit code.
- **A kill is not complete when `ch.Shutdown` returns — wait for `cm.Remove`.** `Shutdown` returns when the child *process* is reaped, but the status only becomes `exited` when `handleChildExit` calls `MarkExited`, and that runs asynchronously on `monitorChild`'s goroutine. Any path that reports a kill as done, or that touches on-disk state afterwards, must call `waitForChildRemoval` (`cmd/rafikid/controller.go`) first: `cm.Remove` is the final step of `handleChildExit`, so absence from the manager is the observable signal that the store snapshot now reads `exited`. `Kill`, `Forget`, and `ShutdownAllChildren` all do this. Its absence in `Kill` is what made `TestIntegration_FullLifecycle`/`KillResume` fail "want status=exited, got shutting_down" — recorded here for months as a timing flake when they were in fact deterministic. The entire documented flake list went green once `Kill` learned to wait, so treat a kill-path failure as a real bug, not noise.
- **gofmt realigns the WHOLE `const`/`var` block, not just new lines** — adding an identifier longer than existing ones to an aligned block and only hand-aligning your own addition leaves the file gofmt-dirty (and thus golangci-lint-dirty) on the *pre-existing* lines. Always run `gofmt -w` (or `golangci-lint run` with the `gofmt` linter) after touching an aligned block, not just `go vet`.
- **In this dev environment, gopls/editor diagnostics claiming "BrokenImport"/"undefined"/"module not in workspace" on files in this repo are frequently false positives** from a stale workspace resolution issue, not real compile errors. Trust `go vet ./...` / `go build` / `go test` run from a real shell over IDE-style diagnostics before treating them as findings.
- **`pkg/protocol` is a deliberately pure-data, zero-dependency package** (imports nothing but `encoding/json` — see its own file header). New wire **request** types belong there. New wire **response** types generally should NOT duplicate an existing domain type's fields (e.g. `insights.Stats` is ~9 nested structs/~40 fields) — instead, pass the domain type straight to `okResponse` (which takes `any`) from `pkg/control`, which already depends on domain packages elsewhere (`childstore.Snapshot` in the `Controller` interface itself). Mirroring types nobody enforces stay in sync is pure drift risk for zero benefit.
- **`ctrl_*` response payload shapes are not uniform — read `pkg/control/dispatch.go` before writing a client-side decode, never infer from a sibling verb.** `ctrl_conversation_stats` and `ctrl_conversation_export` send the domain value bare; `ctrl_conversation_search` wraps its rows in `{"rows": [...]}`. Assuming the search shape from its neighbours produced a runtime `cannot unmarshal object into Go value of type []insights.ConversationSummary` that a unit test happily missed, because the test's fixture encoded the same wrong assumption. Build response fixtures from the shape `dispatch.go` marshals, and confirm against a live daemon.
- **Every `ctrl_*` response is capped at `protocol.MaxFrameBytes` (16 MiB) per frame** (`pkg/protocol/frame.go`) — a writer that emits a larger frame doesn't error cleanly, it silently breaks the reader (`FrameReader` returns `ErrFrameTooLarge`, the connection gets torn down, and the client sees an unhelpful "connection closed" rather than a real error). Any new handler returning variable/unbounded-size data (history, transcripts, search results) must clamp limits and/or size-check the payload before responding — see `recentResponseBudget` in `cmd/rafikid/controller.go` (`ctrl_get_recent`) and `protocol.ErrPayloadTooLarge` (`ctrl_conversation_export`) for the two established patterns.
- **The event buffer only schedules now; the persisted inbox rows are the batch, not `eventbuf.Delivery`.** `Push`/`PushNow`/`PushSteer` are still three distinct behaviours (debounced+busy-gated / immediate-but-busy-gated / immediate-and-busy-bypassing), but each one's job is now just to persist a row with the right `inbox.Mode` and decide *when* to ask for delivery — coalescing itself happens at flush, over whatever rows are pending, not over in-memory buffer state. This is why `DeliverPrompt`/`DeliverSteer` and `Buffer.emit` are gone: there is no batch sitting in the buffer to hand a delivery function, only a signal to go read the rows. The old "sticky steer" flag (`perKey.pendingDelivery`) is gone with them — stickiness is now a property of the data: `inbox.Coalesce` groups every pending row for a (child, source) and a group holding any `ModeSteer` row delivers as a steer, so it survives a daemon restart the in-memory flag never did. `Buffer.fire` must still run its flush callback with `b.mu` released, for the same reason as before: flush reaches `Controller.deliverInbox` → `sendFrame`, a blocking write, and a producer pushing from inside that path would deadlock on a re-entrant acquire.
- **`inbox.ModePrompt` is `Mode = iota` — the zero value — so a dropped, unset, or defaulted `Mode` silently becomes a prompt, never a steer or an abort.** This is the SECOND zero-value trap in this repo carrying real behavioural weight, after `SpawnRequest.MaxCost == 0` meaning unlimited (above); the pattern is worth watching for in general, not just these two spots. `ParseMode`'s fallback to `ModePrompt` for an unrecognised spelling is deliberate — the column is CHECK-constrained, so only a hand-edited row reaches it, and queueing is a safer failure than injecting a steer into an unrelated turn — but the same zero value is reachable by an ordinary Go mistake: a zero-valued `inbox.Inbound{}` literal, a struct-literal test fixture that forgets the field, or any future constructor that builds one without setting `Mode`. This came within one task of shipping a steer that silently degraded to a prompt during a simulated database blip. Any new enum in this repo whose zero value is a meaningful default rather than an explicit "unset" sentinel needs the same look before it ships.
- **Every `inbox.Store` mutation is scoped to exactly one `childID`, except `Sweep`.** Inbox rows are shared across daemons the same way `conversations.child` rows are, so an unscoped state transition — `MarkSent`, `MarkConsumed`, `ResetSent`, `Drop` taken globally — would reach into another live daemon's in-flight messages. `Sweep` is the one deliberate exception, and it is safe for a reason specific to it: it only deletes rows already in a terminal state (`consumed`/`dropped`), which can never again affect any daemon's delivery decision. Corollary learned the hard way while testing this: asserting an exact row count from `Sweep` against the shared test database is structurally flaky, not unlucky — anything else touching the table concurrently changes the count.
- **A fundi child is IN-PROCESS, so "the daemon died" and "the child died" are the same event — but the crash window that actually matters is narrower than that.** Because there is no separate child process to survive a daemon crash, the interesting risk is not "wrote the frame but the child never got it" (impossible in-process) but the gap between the write returning and the agent actually dequeuing the message into a turn. That is why an inbox row is retired only when the child confirms it entered a turn, not when `sendFrame` returns: `pkg/fundi/frontend.go`'s `IDHandler` (`HandlePromptID`/`HandleSteerID`) and `pkg/fundi/engine.go`'s `OnConsumed` fire from `Engine.worker`'s loop (via `e.consume`) the moment a queued prompt is dequeued, before `runTurnGuarded`/`runTurn` execute it, and `cmd/rafikid/inbox_wiring.go`'s `deliverInbox` records the frame's row ids in `sentFrames` **before** calling `sendFrame` — because for an in-process child the ack can land on the child's own goroutine before `sendFrame` has even returned, and a map written afterward would be missing exactly the entry that ack needs. A `claude` child has no such signal (`awaitAck` is forced false for any non-`KindFundi` child in `deliverInbox`), so its rows are marked consumed on the write and its queue does not survive a restart.
- **An acked prompt can be deterministically un-persisted, and that is intentional, not a bug to chase.** `agentloop.Run` calls `conv.RetractTailUser` when the very first LLM call on a fresh user message fails `llm.IsPromptTooLarge` — an oversized first message is structurally untrimmable by the library's own retry. But `fundi/engine.go`'s `Engine.worker` acks the inbox row (`e.consume`, which calls `OnConsumed`) *before* calling `runTurnGuarded`/`runTurn`, because acking after a turn that can run for minutes would let a crash replay a prompt the model already worked on. The two together mean an oversized prompt ends up acked in the inbox AND erased from the conversation, leaving only an `agent_error` frame as its record. Do not "fix" this by treating `IsPromptTooLarge` as "never entered a turn" and leaving the row unacked: the prompt is deterministically oversized, so an unacked row is a poison pill that replays and fails identically on every restart, forever. One loud failure beats an infinite loop.
- **Design/plan docs live in `docs/plans/YYYY-MM-DD-<topic>-design.md` and `...-plan.md`** (not the generic `docs/superpowers/` default) — follow this repo's existing convention when brainstorming or planning new work.
- **Generated protobuf code is checked in** (`pkg/executorpb/`). A contributor without `protoc` or `buf` must still be able to build. The `make proto` target regenerates it when the `.proto` file changes; never hand-edit the generated `.pb.go` or `.connect.go` files.
- **`pkg/protocol` must never import `pkg/executorpb`.** The protocol package promises zero dependencies in its own header and the executor protocol is a separate wire format with its own types and transport. Generated code, the client, and the server all live outside `pkg/protocol`.
- **`Release` ends only its OWN workspace's jobs.** It used to call a `killAll()`
  that took no workspace argument, so releasing one child's workspace killed
  every other child's background jobs on the executor — finding D4. It is
  `releaseWorkspace(id)` now, and it signals the process GROUP, matching
  `kill()`, so a background `npm run dev` does not strand its tree. It is also
  the only thing that ends output retention, since there is no timer.

- **One classification, three derived lists, and a tool you add to none of them fails a test.** `tierByTool` (`pkg/fundi/tools/executor_routing.go`) is the single source: every registered tool is `TierDaemon` or `TierWorkspace` — the classification is **binary**, and `tierCount` plus `TestEveryDeclaredTierIsCarried` keep it so: a declared tier that no tool carries fails the build. A third, `TierPresence`, was reserved for clipboard/editor/browser/file-picker verbs and deleted unbuilt — `docs/reference/executor-protocol.md` §"Not built: a presence tier" records why, and it must be read before anyone adds one back. `TestEveryBlueprintHasATier` fails when a new blueprint is in neither the map nor the registry. That test exists because `ls` shipped outside the old hand-written list for months and therefore listed directories **in the daemon's process** whenever an executor was configured.
  - `WorkspaceTools()` — the 17 that need an executor: `read`, `write`, `edit`, `glob`, `grep`, `ls`, `bash`, `bash_start`/`bash_output`/`bash_kill`, and all seven `lsp_*`. Language servers run on the executor because that is where the files are; a manager in the daemon indexes the wrong tree and `lsp_rename` **writes** to it.
  - `ExecutorLocalTools()` — the 14 an executor process actually serves: `WorkspaceTools()` minus `notRoutedYet`, which holds the three background-job verbs permanently (they are parent-side RPCs and never reach the executor's registry). `executor.NewServer` builds with `MaterializeOnly` over exactly this.
  - `RoutedToExecutor()` — the 17 the parent proxies, i.e. the job verbs added back.
  - **`TierDaemon` stays in the daemon**, and that is what keeps credentials above the boundary: `task_*`, `web*`, `agent_*`, `executor_annotate`, MCP, and `skill`. Note `skill` is daemon-tier with a **union inventory** — `paths.SkillsDirs()` locally plus the workspace's project skills fetched over `ProjectSkills`, with project shadowing user.
  Two traps. `MaterializeAll` (the daemon's path) omits the whole workspace tier when `opts.Executor == nil` and intersects the routed set with `opts.ExecutorTools` from the executor's `Describe` — so a tool the bound executor cannot serve never enters `tools[]`. And building the executor's registry with `MaterializeAll` instead of `MaterializeOnly` is a live panic: `ToolOpts.Tasks` is nil there and the `task_*` tools do not nil-check.
- **`tools[]` is IMMUTABLE for a child's lifetime.** Tool definitions sit inside the prompt-cache breakpoint (the tools+system prefix), so any mutation invalidates the whole cache. An earlier design permitted the list to GROW ONCE, on first human attach, to admit a presence tier; that tier was never built and has been deleted, so the weaker "monotonic" rule is gone with it. Losing a capability never needed a `tools[]` change anyway — the tool stays declared and its calls return `is_error` — and that asymmetry is forced by the API, since a model can only emit a `tool_use` for a declared tool. If you find yourself wanting to add a tool mid-conversation, you are re-opening a decision recorded in `docs/reference/executor-protocol.md`.
- **`tools.AgentSpawner` is bound to ONE child at construction and takes no caller identity in any method.** That is the enforcement of §1.2's rule, and it is easy to undo by "simplifying" the adapter into a single shared value with a `selfID string` first parameter. Do not: fundi children run in-process, so a self id passed as a parameter is one refactor away from being a tool argument, and a tool argument is produced by an LLM that can be prompt-injected into naming a sibling. `newControllerSpawner` is called per-child from `agentRuntimeOptions`, where the daemon-stamped `childID` is already in hand.
- **`SpawnRequest.MaxDepth/MaxCost/MaxChildren` are pointers because zero is
  meaningful for all three, and they collapse to plain values in
  `childstore.Session` — where `MaxCost == 0` means UNLIMITED, not "spend
  nothing".** Every comparison against a stored budget must be guarded by
  `if snap.MaxCost > 0` first. Getting it backwards makes every unbudgeted
  agent refuse its first spawn, which reads as a broken daemon rather than as
  a limit. The pointer-to-value collapse happens in `grantedCost` /
  `grantedDepth` / `grantedChildren` (`cmd/rafikid/limits.go`) and nowhere
  else — do not re-derive it at a call site.

- **Control plane and executor link share ONE TLS listener, routed by PATH.**
  Both are reached by an HTTP/1.1 `Upgrade` (`/control`, `/executor/connect`)
  that `pkg/upgradeconn` hijacks; `RAFIKI_EXECUTORS_ENABLED=1` turns the
  executor path on and it has no address of its own. Why an upgrade rather than
  a byte-sniffing demultiplexer: a mux answers requests, and on the executor
  connection rafikid is the one ASKING — the request direction reverses once the
  connection is up — so there is nothing to route on until you put an HTTP
  request in FRONT of the stream.

  Two consequences. **ALPN stopped being load-bearing**: the outer connection is
  `http/1.1` because net/http can only hijack HTTP/1.1, and the inverted h2
  starts after the 101 on the raw stream where ALPN is not consulted. And
  **after Upgrade, never read the underlying net.Conn** — read the
  `upgradeconn.Conn`. The hijack buffer holds anything the peer pipelined behind
  the request, which is the control plane's auth+request case exactly; and the
  same wrapper keeps the executor's h2 preface from being swallowed by a
  throwaway reader. Those were documented as OPPOSITE rules in two places; one
  wrapper dissolves both, because over-reading is harmless when nothing is
  discarded.

- **The executor DIALS rafikid and then SERVES HTTP/2 on what it dialled;
  rafikid accepts and is the HTTP client.** Four things fail silently if you
  get them wrong (`pkg/execpool/transport.go` documents each at its site):
  `DialTLSContext` is the hook even with no TLS — there is no `DialContext` on
  `http2.Transport`; a second dial request is the reconnect signal, not an
  error to retry; **both** sides must set `NextProtos: []string{"h2"}` or ALPN
  resolves to `""` and `ServeConn` still works; and the hello frame must be
  read byte-at-a-time, because a buffered reader that consumes past its newline
  leaves `http2.Transport` starting mid-frame. Note this last rule is the
  OPPOSITE of the control listener's (`pkg/control/server.go`), where the
  handshake reader must be reused or a pipelined first request is lost.

- **`executors.Store.Delete` is a real hard delete, unlike `users` — verified
  there is nothing else to tombstone for.** The only FK into
  `conversations.executors` is `executor_enrollment_token.executor_id`, and
  it's `ON DELETE SET NULL`; no conversation or child record resolves an
  executor by id after the fact the way conversation authorship resolves a
  tombstoned user by name. `pool.go`'s `refreshRow` distinguishes
  `errors.Is(err, executors.ErrNotFound)` (terminal — the row is gone, evict
  now, same as disabled) from any other `Get` error (transient — keep the last
  known row, matching the "A3 lesson" a few entries below). Getting this
  backwards either evicts a live connection over an ordinary DB blip, or — the
  bug this shipped with before the distinction existed — lets a genuinely
  deleted-but-still-connected executor keep serving forever, because a
  not-found looked exactly like an unreadable row.

- **`conversations.executors` is authoritative on every connection; the
  credential proves only binding to a row.** Nothing that gates access may be
  cached from enrollment time, and nothing self-reported by the executor may
  reach the `labels` column — `self_reported` is a separate column for exactly
  that reason. This is what makes relabelling and revocation row updates
  needing no reissue, no restart, and no access to the machine.

- **Executor narrowing is runtime intersection, never a subset proof.** Compute
  the parent's effective executor set, evaluate the child's selector
  independently, intersect. Attempting to prove a child's selector implies its
  parent's is decidable for equality matches and a logic puzzle the moment
  `notin` appears — and it fails OPEN, which is the wrong direction for a
  confidentiality boundary.

- **A child's executor set is its PARENT'S set intersected with its own
  selector — never `Live()` filtered by the child's selector alone.** The first
  version of `selectExecutor` did the latter, letting any child reach any
  executor its selector happened to match, including one its parent was confined
  away from. `effectiveExecutorSet` (`cmd/rafikid/executor_select.go`) walks the
  lineage root-first so an ancestor's constraint can never be widened by a
  descendant's, and a malformed selector — on either side — EXCLUDES rather
  than admits.

- **`Executor.Admits` is the executor-side half of selection and is easy to
  store and never evaluate** (it shipped that way). An agent-side selector alone
  is permissive by default: it says what the agent wants, not what the executor
  will take. Both halves are evaluated in `effectiveExecutorSet` — a malformed
  admission selector EXCLUDES, never admits, so an operator typo cannot silently
  open a machine to every child.

- **`Pool.mu` is a `sync.RWMutex` and is not reentrant — never block while
  holding it.** A goroutine that blocks holding `Pool.mu` wedges `Live()`,
  `ClientFor()` and every subsequent accept, so one unwell executor takes the
  whole executor plane down. This shipped once (`healthLoop` calling `Park`
  inside its own lock) and was invisible because `pkg/execpool` had no pool
  test. `onHealthFailure` deletes from the live set under the lock, then calls
  `Park` outside it; `sweepParkedOnce` fires `onLost` after the lock is dropped
  for the same reason.

- **The executor row is the ONLY authority on what an executor is, and the
  standing bug is a consumer that reads the self-report instead.** `Describe`
  and `ProvisionResponse` deliberately leave `isolation` and `workspace_mode`
  EMPTY — an executor does not know whether it is in a container and must not
  guess, because that fact gates where other people's children run. Everything
  downstream must read `executors.Executor` (the row the operator wrote at mint
  time): `workspaceInfoFromRow` for what the child is told, the
  `rafiki/workspace-mode` label for reschedule-vs-fail,
  `narrowByWorkspaceMode` for selection. This has already failed twice in one
  direction — the daemon believed `resp.Isolation`, got "none" for every child,
  and `BuildSystemPrompt` silently dropped the entire "Your machine" block for
  exactly the sandboxed workers it exists to warn; and the workspace-mode label
  was hardcoded `"pinned"`, which made the whole reschedule path dead code. Both
  were invisible: no test failed, because the tests had been updated to match
  the self-report. When you touch this path, ask what row field the value came
  from.

  **Transient executors are the explicit exception.** A client-run executor
  (`rafiki create` / `rafiki attach`) has no database row. Its access-gating
  facts are written by the daemon from the authenticated control connection.
  The invariant generalises to: **every access-gating fact is written by the
  daemon from something it verified — a row an operator wrote, or a connection
  it authenticated.** `refreshRow` must skip transient executors (`liveConn.transient`):
  `store.Get` answers `ErrNotFound` for a row that never existed, and `refreshRow`
  correctly treats that as "this row was deleted, evict now", killing a healthy
  executor on its first health tick.

- **An unknown workspace mode is PINNED, never ephemeral** (`workspaceModeOrPinned`).
  Pinned means an executor loss fails the child where it stood; ephemeral means
  the daemon moves it onto another machine. Defaulting an absent value to
  ephemeral — which `HandleExecutorLost` did — reschedules children onto
  machines no operator ever marked interchangeable.

- **`boundExecutor` must never, under any condition, fall back to in-process
  execution.** The `opts.Executor == nil` check in `MaterializeAll` is a
  **security** guard, not a capability check: it is what stops workspace tools
  running in the daemon process. With `boundExecutor`, `opts.Executor` is always
  non-nil for a child with a selector, bypassing that guard by construction.
  It is safe **only** because an unresolvable `boundExecutor` errors rather than
  falling back — see `TestBoundExecutorNeverRunsInProcess`. Any change that
  makes `boundExecutor` return nil or delegate to in-process execution when it
  cannot bind is a confinement escape.

- **Native executors have NO path scoping, deliberately, and `--root` is a
  working directory rather than a sandbox.** The file tools could enforce a
  scope in userspace and `bash` could not, and a scope that evaporates on the
  first shell command is worse than none — native access is gated by
  admission, not by paths. For containers the mounts are the grant and the
  kernel enforces them, but rafiki does not compose them: the operator does, in
  `docker run`. The row's `roots` DESCRIBES that view for humans and selectors;
  nothing enforces it and nothing may imply it does. This is why the workspace
  block prints roots bare — the daemon no longer knows which are read-only, and
  an invented "read-write" label is a claim it cannot back.

- **The workspace block belongs in the system prompt's ENVIRONMENT section,
  never in `defaultBasePrompt`.** It varies between children, and
  `BuildSystemPrompt`'s ordering comment explains why anything that varies must
  follow the cacheable sections — rafiki's prompt-cache breakpoint sits over the
  tools+system prefix. A native, unsandboxed agent gets no block at all.

- **Two protocols must stay conceptually coherent with nothing enforcing it:**
  `pkg/protocol`'s framed JSON for daemon↔client and Connect/protobuf for
  daemon↔executor. This repo already has a documented history of cross-boundary
  constants drifting (`APP_NAME` across Go and TypeScript; the `RAFIKI_*` names
  kept in sync by a comment). Expect the same class of bug between these two and
  do not rely on a compiler to catch it — when you change a workspace or
  isolation value on one side, grep the other.

- **A confinement guard is only as good as the paths that reach it — look for
  the bypass, not the flaw.** Two criticals shipped this way. The executor
  selector narrowed correctly, but nothing required a child to HAVE one:
  `resolveExecutor` returned `(nil, nil)` for an empty selector and the child's
  tools ran in the daemon process, then it stored `""` so `lineageChain` skipped
  it and the whole subtree inherited the escape — closed by the workspace-tools-
  require-an-executor rule, which makes a selector-less child get NO workspace
  tools rather than the daemon's. And the container grant was
  enforced for `bash` while the other five tools went to a host-scoped registry
  that never entered the container. In both cases the guard was right and
  something routed around it. When you add a guard, enumerate every path to the
  guarded resource and assert the guard sits on all of them.

- **rafiki does not launch containers, and an executor declares nothing about
  itself.** A container running `rafiki executor serve` IS a container executor;
  starting containers is what docker and k8s already are. Every fact that gates
  access — `labels`, `isolation`, `workspace_mode`, `roots`, `admits` — comes
  from `conversations.executors`, set at token-mint time by the operator. There
  is no `--isolation`, `--workspace-mode` or `--image`, and **container
  detection must never be added**: sniffing `/proc/1/cgroup` is the executor
  asserting a fact that gates it, which is what `SelfReported` already forbids.

- **A background job needs `WaitDelay`, and its exit code comes from
  `ProcessState`, not from the error.** `exec.Cmd.Wait` waits for the process
  AND for its output pipes to reach EOF; a backgrounded grandchild inherits
  those pipes and holds them, so `bash_start "npm run dev &"` exits instantly
  and `Wait` NEVER returns — the job reports running forever, `RunningHandles`
  counts it forever, retention never starts, and the goroutine leaks. With the
  delay set, a zero exit alongside a lingering grandchild returns
  `exec.ErrWaitDelay` rather than an `*exec.ExitError`, so unwrapping the error
  records -1 for a job that SUCCEEDED; `cmd.ProcessState.ExitCode()` is right in
  every case. `pkg/fundi/tools/bash.go` has carried `bashWaitDelay` for the
  foreground path all along — the background path simply never got it.

- **Background job output is retained by BYTES until the workspace is released,
  never by a timer.** A wall-clock window is meaningless for an async agent: a
  turn can end and resume hours later, so the old 10-minute sweep expired output
  while the agent that started the job was alive and idle. The workspace's
  lifetime already means "as long as someone could ask". A byte budget bounds it
  (`--job-output-budget-mb`), evicting the oldest FINISHED job first — a count
  would treat 32 `git status` jobs and 32 saturated build logs as equal, and a
  running job is never evictable because its output is a live stream.

- **`tools.RTKMode("")` is NOT `RTKOff`** — `rtkRewrite` short-circuits only on
  the literal `"off"`, so a zero value behaves like `auto`. The executor built
  its registry with no `RTK` field at all, which meant every executor silently
  rewrote commands through rtk with nobody having chosen it. `Options.RTK` is
  explicit now and `toolOptsFor` exists to make the whole Options→ToolOpts
  mapping testable, because an unset field there does not fail — it takes a zero
  value that may not mean what it looks like. Note background bash is never
  rewritten regardless: the refusal fallback needs an exit to inspect and output
  it can take back, and a long-running job has neither.

- **`Pool.live` must be mutated on connection IDENTITY, never on executor ID
  alone.** An executor restart installs a new `liveConn` under the same ID long
  before the old connection's health loop notices — up to a full 30s tick — so
  both are live at once. A delete keyed by ID let the stale loop evict its own
  replacement, park a healthy executor, and fire `onLost` against children that
  were running fine. Compare the pointer before deleting (`removeLive`), close
  any `liveConn` you displace (`installLive`), and keep teardown idempotent:
  it arrives from two directions and closing a channel twice panics the daemon.

- **In `pkg/tasks` the memory store is atomic under its own mutex and Postgres
  is not, so the conformance suite structurally CANNOT catch a Postgres-only
  race.** `Drop`'s assignee check needed `FOR UPDATE` over the whole subtree;
  the shared suite passed throughout. Concurrency defects in the postgres store
  need a test in `postgres_test.go` that opens two transactions explicitly — an
  uncommitted UPDATE makes a deterministic barrier where a sleep does not. Note
  also that `FOR UPDATE` locks only the rows it RETURNS, so a predicate in the
  SQL leaves the currently-unassigned rows — the ones a concurrent Assign is
  actually racing for — unlocked; filter in Go instead. And Go has no NULL, so
  `Assignee == ""` matches every unassigned in-memory row where SQL `= ''`
  matches none: guard empty-string arguments in BOTH implementations.

- **An auth failure has two meanings and they are not interchangeable.**
  "This credential is not valid" is an answer and the executor should stop;
  "I could not check it" is not, and an executor that treats the second as the
  first takes itself out of service permanently over a database blip — across a
  fleet reconnecting together, that is the whole fleet. `ExecutorHelloResponse.Retryable`
  carries the distinction; `executors.IsTerminalAuthError` decides it and fails
  toward RETRY, because quitting on a dead credential costs a log line and
  quitting on a transient one costs the fleet. The sentinels live in
  `pkg/executors`, not `pkg/executorsdb`, because the executor links
  `pkg/execpool` and must not link pgx (`pkg/executor/no_postgres_test.go`). Never forward the store's error text to
  the peer: it has not proved who it is, and a pgx error carries the DSN.

- **`ModelCatalog` (`pkg/routing/orcatalog.go`) only ever knows OpenRouter ids —
  it has no source of truth for a local/custom provider's model at all.** A
  provider can declare `[providers.<name>.models.<alias>]` (`providers.Provider.Models`)
  to name a short alias for a real model id and, optionally, its
  `context_window`/`max_completion_tokens` — the only way `CLAUDE_CODE_AUTO_COMPACT_WINDOW`
  can ever be correct for such a model, since the catalog will never report one.
  `Set.Split` substitutes the alias's real id transparently, so every sender
  (`pkg/llm`'s `prepareSend`, `pkg/fundi/config.go`, `pkg/server/proxy.go`) gets
  the translation for free. But `Controller.ContextWindow`/`ModelInfo`
  (`cmd/rafikid/models_presets.go`) must look the alias up by the RAW,
  pre-substitution local id (via `providers.SplitRaw`, not `Set.Split`) —
  calling `Set.Split` first and then trying `p.Models[modelID]` finds nothing,
  because by then `modelID` is already the resolved real id, not the alias key.

- **`NoteBinding` writes the CHILDSTORE; `c.wsLabels` is only a pre-spawn bridge.** `handleChildExit`
  releases a workspace by `snap.Labels`, and `HandleExecutorLost` finds children
  by them, so a binding that stops at `wsLabels` is invisible to both — the child
  leaks its live workspace and releases a dead one. The map is consumed exactly
  once, by `takeWorkspaceLabels` under `wsLabelsMu`; an unlocked read there is a
  *fatal* concurrent map access now that tool goroutines write it.

- **`ErrStreamBroken` means MAYBE RAN, and is the only liveness error that does.**
  Every other sentinel is pre-dispatch: `ErrParked`/`ErrDraining`/`ErrExecutorLost`
  come from `ClientFor`, `ErrRedialed` from the dial hook before the request is
  sent, and `ErrExecutorGone` is the executor answering that the workspace does
  not exist. Only a broken stream may have executed the command, and
  re-provisioning does not give a fresh filesystem — same `--root`, mounts unset
  — so it retries for idempotent verbs only. A new tool defaults to
  non-idempotent by omission from `idempotentTools`, which is the safe direction.

- **A pinned child never changes machines, on either path.** `HandleExecutorLost`
  fails it after the park timeout and `boundExecutor.recover` refuses to
  re-select for it; re-provisioning on the SAME executor is allowed for both
  modes, because the executor's workspace registry is in memory and a restart
  loses every id while the machine is fine.

- **The executor's name is `labels["machine"]`, written by the operator, and
  there is no derived machine id.** `display_name` was dropped in 0020 — no
  enrollment path ever wrote it. The name is optional (a fleet executor

  selected by `env=prod` needs none) and unique per owner
  (`executors_owner_machine_unique`). The client resolves its own via
  `paths.MachineName()`: `RAFIKI_EXECUTOR_NAME`, then
  `<DataDir>/executor-name`, then an error naming both. Never a hostname
  fallback — on darwin a hostname changes with the active network interface,
  which is what the deleted id existed to escape.

- **A top-level spawn whose selector matches nothing is REFUSED; a parented
  spawn starts UNBOUND.** `agentRuntimeOptions` gates on `req.ParentChildID`,
  returning `explainNoMatch`'s diagnostic for a human-operated spawn and letting
  an agent-spawned child wait. An unbound child carries
  `rafiki/executor-state=unbound` in its labels — visible to the operator via
  `rafiki list`/`get` — and the key is removed by `NoteBinding` on the first
  successful bind.

- **`boundExecutor` is the only client for executor-bound work.** The old
  `resolveExecutor`/`selectExecutor` path is deleted — dead code on a
  confinement-critical path since `boundExecutor` landed. `chooseExecutor`
  is now the single selection entry point.

- **A transient executor must never touch the store — not `TouchSeen`, not
  `reattach`, not `refreshRow`.** Its id is `sess-<ULID>` against a UUID
  column (`conversations.executors.id`), and every such call is a silent
  round trip the store rejects. `Evict` now records a tombstone so an
  executor still in its join window is stopped, and `installTransient`
  checks it before installing.

- **One session executor per control connection.** `ExecutorSession` detects
  a previous session on the same connection and releases it (revokes the
  ticket, evicts the executor) before installing the new one. Without it,
  the incumbent stayed in `Pool.live` for the daemon's lifetime.

- **`status` and `last_status` on `conversations.child` are different
  questions, and the recovery predicate reads the second one.** `status` is
  "exited" for every recovered row by construction; `last_status` — written
  only by the exit path — is what says whether the child was ALIVE when the
  daemon died. A child resumes when `last_status` is neither `exited` nor
  `shutting_down`. There is no `running` status (`pkg/protocol/types.go`), and
  `shutting_down` means a deliberate stop, so a filter written as
  `status IN ('running','shutting_down')` selects the one state that means
  "don't resume" and drops every state that means "do". The upsert COALESCEs an
  empty `last_status` for the same reason: an ordinary status write must never
  blank the only column recovery reads.

- **`last_status` is written ONLY by `handleChildExit`, so NULL is the
  strongest evidence a child was alive.** A child that was alive when the
  daemon died has its exit path invoked by the recovery loop rather than by a
  real exit, and the recovery loop does not call `handleChildExit` — so the
  row stays NULL in `last_status`. That NULL is precisely the signal that says
  "this child needs resuming", distinct from every deliberate shutdown that
  `handleChildExit` records. Any other code path setting `last_status` would
  silently mark a live child as dead and prevent its recovery.

- **One writer per conversation, and the guard is in the INSERT statement.**
  Child state on local disk used to provide daemon isolation for free; a shared
  `conversations.child` removes it, and `conversations.conversation_lease`
  restores it. Reads are never gated. Message appends carry an `EXISTS` clause
  checking the lease **in the same statement as the write**, which is why a
  monotonic fencing token is unnecessary — Postgres can answer "is it still me",
  and a stalled holder that wakes after takeover writes nothing. `conversation_turn`
  writes are deliberately NOT fenced: they are append-only analytics, and a lease
  join on every request buys no correctness.

- **`RAFIKI_DAEMON_ID` must be unique per running daemon, and is required in
  k8s.** `LeaseStore.Acquire`'s `OR holder = EXCLUDED.holder` clause lets a
  restarted daemon reclaim its own leases instantly, which is what makes a
  5-minute TTL cost nothing on crash-restart. The same clause means two daemons
  sharing one id reclaim each other's leases on every acquire and reproduce
  exactly the split-brain the lease exists to prevent. A pod filesystem is
  ephemeral, so `<DataDir>/daemon-id` does not survive there — set the env var
  in the Deployment spec or the pod waits out the full TTL before recovering its
  own children.

- **`RAFIKI_DAEMON_ID` cannot prove two rows are on the same machine —
  `ns_token` does.** Two daemons inside the same shared-PID-namespace
  container (a sidecar, a debug shell) see each other's PIDs, and PID alone
  cannot tell them apart. `ns_token` is a random UUID written at daemon
  startup; two processes that share every PID but disagree on `ns_token` are
  not the same daemon, and `Forget` checks both columns before deciding the
  row belongs to the caller. `RAFIKI_DAEMON_ID` alone would let a sidecar
  `Forget` another process's children as its own.

- **A pinned child never changes machines, and restart recovery is not an
  exception — but it IS a resume.** `recoveryAction` returns `planResumeBound`
  for `workspace_mode` pinned (or absent/unrecognised, matching
  `workspaceModeOrPinned`). The stale workspace id is stripped while
  `rafiki/executor` survives, so `boundExecutor.doRecover` can re-provision on
  the SAME executor when it reconnects. Only an ephemeral child has BOTH
  `rafiki/workspace`/`rafiki/executor` labels stripped and resumes unbound.
  Pinned children that migrate would be a backdoor around the rule
  `HandleExecutorLost` and `boundExecutor.recover` already enforce — but
  re-provisioning on the same machine is correct (the files are still there).

- **Child rows are shared across every daemon, so `Forget` must check
  `ownsChildRow` first.** Before the migration, forgetting a child was
  idempotent: a daemon that did not own it had nothing to forget. Now a
  daemon that skips the ownership check deletes rows belonging to a sibling
  daemon that is still running — the opposite of idempotent. `ownsChildRow`
  compares `daemon_id = c.daemonID AND ns_token = c.nsToken` and the
  `RAFIKI_DAEMON_ID`/`ns_token` distinction above is why BOTH are checked.

- **Generated protobuf code in `pkg/gen/` is committed and regenerated by `make proto`; never hand-edit a `.pb.go`.** The Connect control plane (`/rafiki.v1.Control/`) is additive — the JSON-Lines frame protocol still serves `rafiki attach` and the existing CLI, and both run concurrently until the TUI replaces attach. Two rules the schema encodes and a future change will be tempted to break: `ToolUse.input_json` is an **opaque JSON string** (its shape is whatever the model emitted, so typing it means regenerating the schema per tool change), and `ToolResult.content` is a **repeated ContentBlock, not a string** (flattening it makes multimodal tool output — `read` on a PNG — a breaking change later). Every `Usage` field is `optional` because a zero `cache_write` must stay distinguishable from "not reported"; collapsing them silently poisons cost math and `ProviderGuard`'s evidence.
