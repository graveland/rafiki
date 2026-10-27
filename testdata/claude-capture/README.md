# A captured Claude Code session

One real conversation, recorded through `rafikid proxy record` — the raw HTTP
exchanges between Claude Code and the Anthropic API, as `NNNNNN-request.http`
and `NNNNNN-response.http` pairs in arrival order.

It is driven by `testdata/claude-capture.toml`, two `claude` processes sharing
one session, chosen so the transcript covers the awkward shapes of request:

- a plain turn, a tool call, a large tool result, two tool calls in one
  response (both results come back in one user message), a failing tool call,
  non-ASCII output and a multi-line prompt;
- a session killed the instant a `tool_use` response arrives (the client never
  answers it) and resumed by a second process. Claude Code drops the unanswered
  `tool_use` and replays a synthetic assistant `No response requested.` at the
  index the stored reply occupies: a client rewriting a message the proxy
  already stored;
- two dependent tool calls in sequence;
- `/compact`, which goes through in headless stream-json mode as a
  summarization request over the same history (its last user message gains a
  "CRITICAL: Respond with TEXT ONLY" block, a second rewrite), then a request
  that opens with the summary;
- `/clear`, then one more turn on the fresh history.

`TestCapturedClaudeSessionReplays` (`pkg/capture`) replays every request
through capture and logs the stored ordering (`go test -v`).

Regenerate it with:

```sh
make capture-claude
```

That replaces the capture files and leaves this README, so committing the
result makes `git diff` answer "what changed in Claude Code's requests?" — the
client ships constantly, and this is a snapshot of one day's protocol rather
than an archive.

## What is safe to commit, and what is not

- **Credentials** (`Authorization`, `X-Api-Key`) are recorded as their scheme
  plus a sha256 of the token, never the token.
- **`metadata.user_id`** (device id and account uuid) is recorded as a sha256
  of the whole field.
- **Prompts and completions are verbatim.** `testdata/claude-capture.toml` is
  deliberately dull for that reason: anything you add to it lands in the
  repository in clear text.
- Machine-specific headers (`X-Stainless-Arch`, `Host`, session uuids, client
  version strings) are recorded as sent. Nothing asserts on them; a test reads
  the status line, `Content-Type` and the body.

## Reading one

Each file is an HTTP message: start line, headers, a blank line, then the exact
body. Responses are SSE, so the body is the event stream the client saw —
`content_block_start` with `"name":"Bash"` and the `input_json_delta` that
follows is the tool call; the next request's `tool_result` is the client
answering it.
