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
laptop with no API key they run on the operator's own Claude login.

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
session fails rather than hanging.

One client-side gotcha when pointing Claude Code at any proxy by hand: its
SSE-ping stream-idle watchdog only activates when the base URL host is
exactly `api.anthropic.com`, so a thinking phase with >300s between content
events on a custom host dies with "Response stalled mid-stream" even though
bytes flowed the whole time. Set `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1`
to restore direct-connection behavior (`rafiki claude` and fundi-spawned
claude children get it automatically) — but that variable also re-enables
deferred tool search, which 400s on non-Anthropic models, so the launcher
also sets `ENABLE_TOOL_SEARCH=false` whenever `--model` isn't an Anthropic id.
