# rafiki — project instructions

**Update this file when you learn something that would save significant time or tokens next time** — a gotcha, an architectural invariant, a footgun, a convention that isn't obvious from the code. Don't log history or one-off facts; only actionable current-state knowledge. Remove entries once the underlying issue is fixed.

**Keep documentation in sync with code, always.** Any change to the wire protocol, CLI commands, config/env vars, or public behavior must update the relevant doc in the same change: `docs/reference/control-protocol.md` (wire protocol), `docs/agent-cli.md` (`rafikid agent` verbs), `README.md` (architecture/setup), `.env.example` (env vars). Documentation that drifts from code is worse than no documentation — don't defer doc updates to "later."

- **Two binaries, split by dependency: `rafiki` is a socket client that must
  never open a postgres connection; `rafikid` owns every DSN.** That is the
  whole point of the split and the easiest invariant for a future change to
  violate silently. `cmd/rafiki` does currently *link* pgx transitively
  (residual from pre-split days; see `tasks/todo.md`). Three concrete changes
  have already landed: the client asks the daemon for model information over
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

- **A `Materializer` may decline by returning `(nil, nil)`; `MaterializeAll` then omits it from the Registry.** This is how `SkillBlueprint` keeps a useless `skill` tool out of `tools[]` when zero skills were discovered (also what `--no-skills` reduces to) — a skill tool over an empty inventory can only ever answer `unknown skill %q; available skills: `, burning a turn. Note `Registry.Register` does **not** guard against nil: it panics inside `BuildDef` on a nil-interface deref, so any new caller that materializes a blueprint by hand must nil-check before registering. Tests that materialize `SkillBlueprint` directly must pass a non-empty `Skills` or they will now get nil back.

- **A TCP/TLS control connection must have exactly one `protocol.FrameReader` for its whole lifetime.** `FrameReader` owns a 64 KiB `bufio.Reader`, so a reader used for the `ctrl_auth` handshake has very likely already buffered bytes past the auth frame's `\n` — `client.DialURL` returns immediately after writing auth, so a caller's first request routinely arrives in the same TCP segment. Building a second reader for the request loop silently discards that pipelined request and the client hangs to its 30s timeout. `authHandshake` therefore returns its reader and `handleConn` reuses it (nil means the UDS path, which has no handshake and allocates its own). Any test that sleeps between dial and first request will pass regardless — exercise it with a single `Write` carrying both frames.

- **This dev environment wraps `go test` (and other dev commands) through `rtk`, which compacts the output to a bare pass count.** A failing run still surfaces, but per-test names, `=== RUN` markers, and — critically — **SKIP lines** are swallowed, so a suite that silently skipped everything looks identical to one that passed. When you need real Go test output (verifying a test actually ran, reading a panic trace, using `-json`), invoke the toolchain directly: `/usr/local/go/bin/go test ...`. This has already misled two separate sessions.

- **ripgrep only honours `.gitignore` inside a tree that contains a `.git` directory** — outside one it ignores the file entirely unless `--no-require-git` is passed. `pkg/fundi/tools/discovery.go` therefore passes `--no-require-git` on every invocation. This is invisible in a normal repo checkout and only bites in tests, whose fixtures are bare `t.TempDir()` trees: without the flag, a fixture asserting "the gitignored file is excluded" fails, and worse, a fixture asserting the opposite would pass for entirely the wrong reason. Never pass `-L` (symlink following lets rg escape the search root into module caches or the nix store and chase cycles) and never pass `--no-ignore` (it defeats the point).

- **A tool's `ToolResult` is flattened to a `string` inside `Registry.Register`'s closure, not inside `Registry.Execute`.** `Execute` looks like the conversion point and is not — it invokes a pre-built `func(context.Context, json.RawMessage) (string, error)`. Anything that changes how results become model-visible content (content blocks, images, per-tool budgets) belongs in the `Register` closure. Note also that a non-text result cannot survive the `string` return in `Registry.Execute`, `agentloop.ToolSet.Execute`, **and** `anthropic.NewToolResultBlock` — all three would need to change together.

- **Setting `Accept-Encoding: gzip` by hand on an outbound request DISABLES Go's transparent decompression.** `http.Transport` only adds the header and gunzips the response when the request's Accept-Encoding is empty; set it yourself and you receive raw gzip bytes with no error anywhere. This shipped in `searchBrave` and made every Brave-keyed `websearch` fail with `invalid character '\x1f' looking for beginning of value`. A fixture server that answers plain JSON cannot catch it — any test guarding a client against this must actually gzip its body and set `Content-Encoding: gzip`.

- **Custom auth headers survive cross-host redirects; only `Authorization`/`WWW-Authenticate`/`Cookie` are stripped.** `X-Subscription-Token` (Brave) was forwarded intact to a redirect target. Outbound clients in `pkg/fundi/tools` therefore go through `newGuardedClient` (dialer `Control` hook, the only place an address check survives DNS rebinding *and* redirects) and, for credentialed calls, `newBraveClient`, which wraps rather than replaces the existing `CheckRedirect` and deletes the credential on a host change. Any NEW outbound client belongs on that path — `websearch` originally built bare `&http.Client{}` literals and had neither guard.

- **`rtk` has no distinct exit code for "I refuse to run this"** (verified against 0.43.0): refusals and genuine tool failures both exit nonzero (1/128/254/255 on both sides). The only reliable discriminator is that rtk prefixes its OWN diagnostics with `rtk: ` on stderr while passing the underlying tool's stderr through untouched — that is what `rtkRefused` in `pkg/fundi/tools/bash.go` keys on to re-run the original command under plain bash. Bias it toward NOT falling back: a false positive re-runs a failing command twice.

- **Teeing a subprocess's stderr into a buffer that stdout also writes to is a data race.** `exec.Cmd.CombinedOutput` gets away with one buffer only because it makes `Stdout` and `Stderr` the *identical* writer, which `os/exec` identity-checks to share a single pipe. Point them at a buffer plus an `io.MultiWriter` over that same buffer and you get two goroutines writing concurrently — which silently drops output as well as tripping `-race`. `pkg/fundi/tools/bash.go` uses a mutex-guarded `syncWriter` for this.

- **Quoting rules differ between `'` and `"` and the rtk rewrite guard must honour that.** Inside single quotes every shell metacharacter is inert; inside double quotes bash still expands `$`, `` ` ``, `$( )` and honours `\`. `hasShellChaining` refuses `$`/backtick/backslash inside `"…"` while leaving `;`/`|`/`&`/`<`/`>` inert there. Treating both quote kinds alike (the original bug) let `git commit -m "release $VERSION"` reach rtk's argv and commit the literal string.

- **Deleting a git worktree leaves golangci-lint's cache keyed to the dead path, and the next `make check` in a DIFFERENT worktree reports phantom failures.** The errors cite files by their old absolute path and are accompanied by `failed to parse file: ... no such file or directory` warnings — the finding itself ("Error return value of `c.sf.Do` is not checked" in `pkg/routing/orcatalog.go`) looked entirely real while the file was clean. `golangci-lint cache clean` fixes it. Suspect this whenever `make check` fails on code you did not touch, especially right after a `git worktree remove`.

- **The provider cache guard is observational because the catalog cannot answer the question.** OpenRouter's endpoints API exposes `supports_implicit_caching`, which looks exactly like the flag you'd filter providers on. It is useless: it is `false` for Novita, which served 98-99% of input tokens from cache for days, and `true` only for the first-party DeepSeek endpoint. Every third-party endpoint also publishes an `input_cache_read` price whether or not it ever delivers a hit. There is likewise no `provider` routing field for requiring caching (`require_parameters` covers request parameters, and DeepSeek caching is implicit). So `routing.ProviderGuard` learns from observed `cache_read_tokens` instead — anyone tempted to replace it with a catalog lookup will find the catalog does not know.

- **`ProviderGuard` is inert without capture, by design.** It qualifies a cache miss using `prefix_hash` and the conversation id, both of which come from the capture path; with capture disabled it receives empty values, nothing qualifies as evidence, and it ejects nothing. That is the intended fail-safe (no evidence beats false evidence), not a bug to "fix" by loosening the qualification rules. The rules are pinned by replay tests over real traffic in `pkg/routing/testdata/`: loosening them makes `TestReplayHealthyNovitaNeverEjects` blacklist a provider that was working fine.

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
- **The event buffer's two bypasses are orthogonal, and `eventbuf.Delivery` is what keeps them apart.** *When* a batch goes out and *how* it arrives are separate axes. `Push` is debounced + `DeliverPrompt` + busy-gated (the default for everything). `PushNow` skips the debounce but still defers while the child is mid-turn, and still arrives as a prompt. `PushSteer` skips both the debounce and the busy gate and arrives as `{"type":"steer"}`, injected into the turn already running — reserve it for events that *invalidate* that turn, because a worker that has lost its executor must not spend another 40s believing it still has one. A steer deferred for any reason stays a steer (`perKey.pendingDelivery`). `Buffer.emit` must always run with `b.mu` released: flush is `Controller.injectBatch` → `Controller.Send`, a blocking write, and a producer pushing an event from inside that path would deadlock on a re-entrant acquire.
- **Design/plan docs live in `docs/plans/YYYY-MM-DD-<topic>-design.md` and `...-plan.md`** (not the generic `docs/superpowers/` default) — follow this repo's existing convention when brainstorming or planning new work.
- **Generated protobuf code is checked in** (`pkg/executorpb/`). A contributor without `protoc` or `buf` must still be able to build. The `make proto` target regenerates it when the `.proto` file changes; never hand-edit the generated `.pb.go` or `.connect.go` files.
- **`pkg/protocol` must never import `pkg/executorpb`.** The protocol package promises zero dependencies in its own header and the executor protocol is a separate wire format with its own types and transport. Generated code, the client, and the server all live outside `pkg/protocol`.
- **Two tool lists, and they are not the same list.** `tools.ExecutorLocalTools()` is what an executor process RUNS — `read`, `write`, `edit`, `glob`, `grep`, `bash` — and `executor.NewServer` builds its registry with `MaterializeOnly` over exactly that set. `tools.RoutedToExecutor()` is what the PARENT dispatches remotely: those six plus `bash_start`, `bash_output`, `bash_kill`, which are parent-side tools implemented as RPCs and never reach the executor's registry. Everything else — `skill`, `task_*`, `web_*`, `lsp_*`, MCP — stays parent-side, which is what keeps credentials above the boundary. Building the executor's registry with `MaterializeAll` instead is a live panic: `ToolOpts.Tasks` is nil there, and the `task_*` tools do not nil-check.
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

- **Container mounts ARE the grant, and the daemon derives them — nothing
  model-facing contributes a path.** `pkg/workspace.Derive` takes a cwd and a
  mode, and that signature is the enforcement: a coordinator choosing labels
  and a workspace mode cannot make the subtle mistakes a coordinator composing
  path allowlists would. Do not add a caller-supplied mount list. Note the
  matching asymmetry for NATIVE executors: path scoping there is deliberately
  absent, because the file tools could enforce it and `bash` could not, and a
  scope that evaporates on the first shell command is worse than none — native
  access is gated by admission, not by paths.

- **`RepoRoot` must use `--git-common-dir`, never `--show-toplevel`.** Inside a
  linked worktree `--show-toplevel` returns the worktree, which makes every
  worktree its own repo and turns the read-only `/repo` mount into a duplicate
  of `/work` — silently removing the read-only half of the grant. This is what
  migration 0013's "repo_root groups worktrees of one repo; cwd alone does
  not" is about.

- **`Pool.ClientFor` returns a connection-scoped client; `ClientForWorkspace`
  returns a child-scoped one.** A workspaced child handed the shared client
  runs its tools in whatever workspace the previous caller used — a cross-child
  leak no test catches, because both children see plausible files either way.

- **The model-facing executor grant is exactly two fields — a label selector and
  a workspace mode — and must stay that way.** `agent_spawn` has a test
  asserting no path-shaped parameter exists (`TestAgentSpawnHasNoPathShapedParameter`).
  Mounts are derived by the daemon from the child's worktree
  (`pkg/workspace.Derive`); the moment a caller can contribute a path, grants
  stop being safe to author without human review, which is the property the
  whole selector model exists to buy.

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
