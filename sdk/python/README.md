# rafiki-py — rafiki's Python SDK

A Connect client for the [rafiki](../../README.md) control plane: enough to
drive rafiki from Python — spawn children, wait for them to settle, read
their transcripts — and, inside a script child, everything a script needs to
talk back: `report`, `receive`, `result`.

Two dependencies: `httpx` and the standard library. No grpc, no protobuf
runtime. The message types in `rafiki/_gen/` are dataclasses generated from
`proto/rafiki/v1/*.proto` by `make proto` (the generator lives in
`cmd/protoc-gen-rafikipy`), so the SDK cannot lag the wire: the same `make
proto` run that regenerates the Go code regenerates these.

```bash
pip install ./sdk/python      # from a repo checkout; or: PYTHONPATH=<repo>/sdk/python
```

## Building a client

```python
from rafiki import Client, ConnectError

# Inside a script child: the per-child unix socket in RAFIKI_CHILD_CONNECT.
# The socket is the credential — no token is ever sent.
c = Client.inside()

# Standalone: resolve a profile exactly the way the CLI does (pkg/profile is
# the only resolver; the SDK shells out to `rafiki profile show -o json`
# rather than parsing profile files).
c = Client.from_profile()            # the current profile
c = Client.from_profile("work")      # a named one

# Explicit: a unix socket path, unix://, http+unix://, or an http(s):// URL.
c = Client("/run/user/1000/rafiki/controller.sock", token=...)   # local
c = Client("https://rafiki.example.net", token="rfk_...")     # remote
```

`from_profile` needs a `rafiki` binary on PATH built recently enough to
honor `-o json` on `profile show`; the error says so when it isn't.

## The verbs

Every method maps to one Connect RPC of `rafiki.v1.Control` — see
`docs/reference/control-protocol.md` for the wire contract each one rides.

```python
child = c.spawn("one turn, please", kind="fundi", labels={"team": "infra"})
#                 └ prompt: spawn alone starts no work; this is sent
#                   as the child's first message (an inbox row)

c.send(child, "and a second turn")        # prompt (queues), "steer", "abort"
kids = c.list(labels={"team": "infra"})   # status-filtered server-side,
                                          # label-filtered client-side
one  = c.get(child)                       # status, cost, latest_ordinal, result

for settle in c.settled([child]):         # yields Settle(child_id, state)
    print(settle)                         #   as each child reaches idle/exited
states = c.wait([child], timeout=120)     # {"c_...": "idle"} once all settle

transcript = c.export(child)              # the child's decomposed transcript
c.stop(child)                             # graceful, then the kill ladder;
                                          # carries a per-call read timeout of
                                          # shutdown+kill windows + 30s slack

# Presets (owner-scoped; a child credential may read, only a user may put).
# put_preset takes a PresetRow or `rafiki preset get -j`'s dict shape:
# tools/skills/mcp_servers absent = the kind's default, [] = none.
seats = c.list_presets("review:")         # latest live row per name
seat  = c.get_preset("review:reviewer")   # not_found raises ConnectError
c.put_preset({"name": "review:reviewer", "kind": "fundi", "model": "...", "skills": []})

# Recall and memories (owner-scoped: reads and writes act as the caller's
# owner, and a child credential is bound to its owner's rows, never an admin's).
hits = c.recall("what did we decide", sources=["memory", "summary"])  # owner's memories + conversations
text = c.recall_context(hits[0].id)       # expand a "m:"/"s:"/"w:" hit id
mem  = c.memory_get("infra", "notes")     # not_found raises ConnectError
rows = c.memory_tree("infra", depth=2)    # memories under a path, 0 = unlimited
row  = c.memory_put("infra", "notes", "body", meta={"tag": "x"})  # save/replace
c.memory_delete("infra", "notes")         # tombstone, never a hard delete

# Routing-policy rows (any caller may read; a user or the local socket may
# write). A row's line is a "-" prefix family: it matches model ids equal to
# it or extending it with "-", so "z-ai/glm-5.3" also governs
# "z-ai/glm-5.3-flash" and stamped releases.
c.set_route("z-ai/glm-5.3", "sort=price,quant=fp8+")   # daemon-validates the spec
rows = c.list_routes()                                 # one RouteRow per live line
c.delete_route("z-ai/glm-5.3")                         # not_found when there is none

# Script-child verbs (identity = the child; from a user credential they
# are refused — a user has no position in the tree to report from):
c.report("progress", {"stage": "halfway"})     # → the parent's event buffer
c.result({"answer": 42})                       # the settle fragment carries it
for msg in c.receive():                        # the inbox, streamed
    if msg.stop:                               # work first, stop last: a Stop
        break                                  # ends the stream (and the run)
    print(msg.text.text, msg.text.message_ids)
```

## Errors and the retry rule

Every failure raises `ConnectError(code, message, status)` — the code is
one of Connect's (`"unavailable"`, `"permission_denied"`, ...), never a
subclass hierarchy. Exactly one code is retried, with bounded exponential
backoff (6 attempts, 0.1 s base, 2 s cap — constructor-overridable via
`max_attempts`): `unavailable`. That is the daemon-restarting signal the
per-child socket's proxy answers with (HTTP 503 and a Connect body naming
the code), and a failed dial means the same thing and joins the same
budget. `permission_denied` and `invalid_argument` are authority and
caller mistakes — they raise through immediately.

Streaming calls retry too, with their delivery contracts spelled out:

- After this client sends a child a prompt (`send()`, or `spawn()` with a
  prompt), `settled()`/`wait()` settle it on the turn that prompt started — a
  busy status after the send, then idle — never on the idle it reports before
  picking the prompt up. Exited always settles.
- `settled()`/`wait()` resume from the highest event ordinal actually seen
  (the daemon's replay cursor), so a settle that lands during a daemon
  restart is seen rather than waited past.
- `receive()` re-opens from the start of the inbox. Delivery is
  at-most-once — rows are consumed at the pull — so a stream that dies
  between pull and wire loses what it had not yet yielded. That is the
  daemon's contract, not a choice the SDK makes.

## Tests

The Go test `TestPythonSDK*` in `test/integration` drives this SDK against a
real scratch daemon (isolated profile, fake-LLM seat). Its interpreter is a
venv uv builds from this directory's `pyproject.toml` in the user cache
(`RAFIKI_TEST_SDK_PYTHON` overrides it); with no uv on PATH the tests skip,
loudly. The
wire-level details (envelopes, error mapping, retry budget) are additionally
covered without a daemon by the same test's self-check script.
