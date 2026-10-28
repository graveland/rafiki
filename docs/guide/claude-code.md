# Claude Code and subscription billing

`--passthrough-auth` (or `RAFIKI_CLAUDE_PASSTHROUGH`) is a three-way switch —
`auto` (default), `on`, `off` — for who gets billed. `on` bills **your**
Claude subscription instead of the daemon's `ANTHROPIC_API_KEY` (by omitting
`ANTHROPIC_AUTH_TOKEN`, the only thing that makes Claude Code prefer
API-key auth over OAuth); `off` always bills the daemon's key; `auto` picks
`on` whenever `--model` resolves to an Anthropic id. It's Anthropic-only
(rejected outright against a non-Anthropic model, with OpenRouter failover
disabled for these requests too) and fails closed on every ambiguous case —
no rafiki token, no forwardable credential, a typoed flag value — never
silently falling back to the daemon's key. Daemon-spawned `--kind claude`
children take the same switch (`passthrough_auth` on Spawn, resolved by
`proxyenv.PassthroughAuthFor` in `cmd/rafikid/agent_runtime.go`), so on a
laptop with no API key they run on the operator's own Claude login. A
passthrough request never fails over to OpenRouter, but a transport error
before any response byte (a pooled HTTP/2 connection that died) is replayed
against Anthropic on `routing.RetryBackoffs`; a status Anthropic returned is
passed back verbatim, not retried.

Every passthrough response carries Anthropic's `anthropic-ratelimit-unified-*`
headers (your subscription's 5h/7d utilization), captured best-effort into a
latest-only per-user snapshot. `rafiki claude --limits` prints it; the
cockpit's status line shows a live-polled summary; a `quota_status` tool lets
a coordinator check its own headroom before deciding to fail over.

`rafiki claude --turn "..."` (repeatable) or `--turn-file turns.txt` runs a
scripted headless session instead of an interactive one: the same binary, in
the stream-json mode a daemon-managed claude child uses, with each prompt
awaited on its own result frame. It is how a capture run gets several turns
(text, tool call, follow-up) in one conversation, and it works against any
Anthropic-protocol endpoint — including `rafikid proxy record`, which sets
`ANTHROPIC_BASE_URL` for it. `--turn-timeout` bounds a single turn so a wedged
session fails rather than hanging. A scripted run resolves no profile at all —
endpoint from `--url` or `ANTHROPIC_BASE_URL`, credential from `--token`,
`ANTHROPIC_AUTH_TOKEN` or `ANTHROPIC_API_KEY` — so it works on a host with no
rafiki config.

`--script script.toml` runs several claude processes in one invocation, for
behaviour that only exists across processes. It is exclusive with `--turn` and
`--turn-file`, which stay a one-run script. A script is a list of `[[run]]`
tables, each with `turns`, an optional `name`, and:

- `resume = "<earlier run's name>"` launches the run with `--resume` on that
  run's session; both runs carry the same `X-Rafiki-Session`, so they land on
  one conversation. A run takes part in a resume only if it is named by one or
  names one, and only those runs drop `--no-session-persistence`.
- `interrupt = "tool_use"` kills the run (SIGKILL) the moment the last turn's
  response carries a `tool_use` block, before the tool result goes back. The
  kill is the run's success; a last turn that ends without a tool call is an
  error. Claude Code then drops that unanswered `tool_use` from the history a
  resumed run replays.
- `model`, `system_prompt`, `tools` and `turn_timeout` override the same keys at
  the top of the file, which override the flags. `args` at the top applies to
  every run and a run's own `args` are appended after; flags the driver owns
  (`-p`, the format flags, `--resume`, `--session-id`, `--model`, ...) are
  refused. Unknown keys are an error. Each run ends by closing the process's
  stdin once its last turn's result frame arrives.

`testdata/claude-capture.toml` is the worked example and the default for
`make capture-claude`.

One client-side gotcha when pointing Claude Code at any proxy by hand: its
SSE-ping stream-idle watchdog only activates when the base URL host is
exactly `api.anthropic.com`, so a thinking phase with >300s between content
events on a custom host dies with "Response stalled mid-stream" even though
bytes flowed the whole time. Set `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1`
to restore direct-connection behavior (`rafiki claude` and fundi-spawned
claude children get it automatically) — but that variable also re-enables
deferred tool search, which 400s on non-Anthropic models, so the launcher
also sets `ENABLE_TOOL_SEARCH=false` whenever `--model` isn't an Anthropic id.
