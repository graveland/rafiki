# A captured Claude Code session

One real session, recorded through `rafikid proxy record` — the raw HTTP
exchanges between Claude Code and the Anthropic API, as `NNNNNN-request.http`
and `NNNNNN-response.http` pairs in arrival order.

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
- **Prompts and completions are verbatim.** `testdata/claude-capture-turns.txt` is
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
