# Repos, skills and the Python SDK

## Repos

A repo is an owner-scoped `(name, url, ref)` registration pointing at
external git content. Children address it as `repo=<name>` on the pymodule
tools, and the reserved name `local` stays the built-in blob store. The name
is a safe path segment — 1-64 letters, digits, `_` and `-`, not starting with
`-` — because it also names the executor's checkout directory.

```bash
rafiki repo add <name> <url> [--ref <ref>] [--refresh <duration>|off]
rafiki repo set <name> [--ref <ref>] [--refresh <duration>|off]
rafiki repo refresh [name...]
rafiki repo list
rafiki repo remove <name>
```

- **`add`** registers the source and runs its FIRST refresh synchronously,
  so a bad clone (unreachable host, bad ref, failed auth) fails the add and
  rolls the registration back to what it was before. A skills namespace
  collision with another source rolls it back too; any other skill error is
  reported (`skill error: …` on stderr) and the command exits non-zero while
  the registration and its scripts stay. The summary line names the resolved
  commit, the script/package/skill counts and the venv state.
- **`set`** changes `--ref` and/or `--refresh` in place and does NOT refresh:
  the new ref takes effect on the next refresh (`repo refresh` or the
  schedule's next fire), and the stored commit keeps describing the previous
  ref until then. Naming neither flag is an error.
- **`refresh`** re-pulls each named source on every live executor of yours
  whose `Describe` reports `repo_sync`, in parallel; with no name it
  refreshes every registered source, in list order. Each source is its own
  Connect call, so one failure is reported on stderr and does not stop the
  others. A failed venv build is printed but does not fail the command; a
  skill error does exit non-zero.
- **`list`** shows `NAME`, `URL`, `REF`, `COMMIT` (the last resolved commit,
  shortened to 12 characters), `REFRESH` (`-` when on demand), `SKILLS` (how
  many rows the source owns for you) and `SCRIPTS` (how many its cached
  discovery reported — zero until this daemon has refreshed the source).
- **`remove`** deletes the registration and the `repo:<name>` skills it
  owned. Executors are never told: there is no prune model for repos, so
  cached checkouts simply age out.

**Refresh is opt-in, never on by default, and the floor is one minute.**
`--refresh 15m` arms a per-repo timer; `--refresh off` clears it. A timer
fires at `resolved_at + interval` (immediately when the source has never
been resolved), and re-arms from when a fire finished, so a slow refresh
cannot stack up behind itself. The scheduler logs a failure once per outage
and once on recovery rather than on every fire.

**Ref resolution happens once.** The daemon asks one eligible executor to
resolve the ref to a commit SHA (`ResolveRepoRef`) and fans that SHA out to
every one of your eligible executors, so git runs only on executors, with
your own credentials, and no two executors can disagree about which commit a
refresh means.

What a checkout yields:

- **Scripts and packages.** Each `scripts/*.py` is a callable script, and
  each top-level directory with an `__init__.py` is an importable package;
  both are described by their first `#` comment line.
- **One shared venv per commit.** `uv sync` runs against the checkout's own
  `pyproject.toml` and builds `<commits>/<sha>/.venv`. A checkout with no
  dependency manifest needs no build; one carrying only `requirements.txt`
  (which `uv sync` cannot consume) reports the venv as not ready rather than
  pretending otherwise. A failed build is reported and only fails the runs
  that need it.
- **Skills.** The checkout's `skills/`, `.claude/skills/` and
  `.rafiki/skills/` directories, plus every distribution declared in
  `[tool.rafiki] skill-sources` of its `pyproject.toml` and installed into
  that commit's venv. A wheel is read through its own `RECORD`; a local
  (`file://`) install is read from its source directory. The namespace is
  the checkout's `.claude-plugin/plugin.json` name when it declares one,
  else the repo's name; a skill-source is namespaced by its entry name (a
  wheel) or by the source directory's plugin name (a local install).

Each commit gets its own detached worktree, and `current` is a symlink
flipped to it only after the fetch, checkout, venv and discovery all
succeeded. A job resolves `current` to its real path when it starts and
holds a `flock` on it for life, so a refresh can move `current` on without
pulling the commit or its venv out from under a running script. Unheld
checkouts older than 7 days are swept after a successful refresh.

## Skills

The daemon's skill corpus lives in `conversations.skills`; it is what a
fundi child's `skill` tool serves and what is pushed to executors for claude
children. Skills come from the daemon's embedded core corpus, from repos,
and from rows written by earlier rafiki versions — there is deliberately no
`add` or `import` subcommand: a skill's source owns it.

Three layers decide precedence, strongest last: **core** (the embedded
corpus, source `rafiki-core`, synced at every daemon startup), **global**
(shared rows with no owner) and **user** (rows owned by you; repo skills
land here under `repo:<name>`). `skills.Resolve` is the only place that
precedence is decided. A namespace present in a layer — any row, enabled or
not — replaces that namespace from every lower layer WHOLE; the default
namespace `rafiki` is the exception and merges per skill name. Disabled rows
are dropped last, and because presence is about the row's existence rather
than its content, a disabled higher-layer row still shadows the layer below
it: disabling an override hides the skill rather than revealing the core
one. Disabling a core skill is how you switch it off.

```bash
rafiki skills list [--all]              # NAME, LAYER, SOURCE, ENABLED, SHADOWED, DESCRIPTION
rafiki skills show <namespace:name>     # body, plus a files: inventory when it has any
rafiki skills rm <namespace:name>       # delete the highest-layer row
rafiki skills enable <namespace:name>
rafiki skills disable <namespace:name>
```

- `list` omits bodies and carries no files; `--all` adds disabled rows.
  `SHADOWED` marks an enabled row a higher layer overrides — listed so you
  can see what is being overridden, and why disabling the override changes
  nothing.
- `show` prints the body and, for a skill with supporting files, one
  `<mode> <size> <path>` line per file. The content stays on the daemon.
- `rm` deletes the highest-layer row a bare name resolves to. A core skill
  cannot be deleted (disable it instead) and a repo skill cannot be deleted
  (remove the repo — a refresh would resurrect it). Hand-authored and
  legacy-imported rows are yours to delete.
- `enable`/`disable` flips that same highest-layer row. A shared row (core
  or global) may be changed only by an admin or over the local unix socket;
  your own user rows are yours.

**Supporting files.** A skill may carry files beside its `SKILL.md`: every
regular file under its directory, recursively. Hidden entries (a name starting
with `.`, and `__pycache__` directories) are skipped; a symlink anywhere is an
error naming its path, never followed. Modes are reduced to `0644` or `0755`,
and the caps are 1 MiB per file and 8 MiB per skill — over either is an error,
never a truncation. Files are stored content-addressed (`skill_blob`/`skill_file`),
so an unchanged set writes nothing. A claude child gets them written beside
`SKILL.md` by the corpus push; a fundi child bound to an executor has them
materialized on demand into a content-addressed cache there when the model
invokes the skill, and with no executor the body says the files are
unavailable instead of naming a directory that does not exist.

The corpus pushed to an executor is resolved for THAT executor's owner, not
sent daemon-wide: an unowned executor receives the shared core and global
layers only, and one owner's overrides never land on another owner's
machine.

## The Python SDK

`sdk/python` is rafiki's first SDK: a hand-written Connect JSON client over
`httpx` and the standard library — no grpc, no protobuf runtime. Its message
types are dataclasses generated from `control.proto` by `make proto` (the
generator is `cmd/protoc-gen-rafikipy`), so the SDK cannot lag the wire: one
run regenerates the Go code and the Python types together.

Two ways to connect, per the script-children design. Inside a script child,
`Client.inside()` dials the per-child unix socket in `RAFIKI_CHILD_CONNECT`
— the socket is the credential, and no token is ever sent. Standalone (CI,
drivers, notebooks), `Client.from_profile()` shells out to
`rafiki profile show -o json` and lets `pkg/profile` do the resolving — the
SDK parses no profile files. `Client(url, token)` is the explicit form: a
unix socket path, `unix://`, `http+unix://`, or an `http(s)://` URL.

```python
from rafiki import Client

c = Client.from_profile()                       # or Client.inside() in a script child
child = c.spawn("one turn, please", model="anthropic/claude-sonnet-4-5")
for settle in c.settled([child]):
    print(settle)                               # Settle(child_id=..., state="idle")
print(c.export(child).turns)                    # the child's transcript
```

The full method list and the error/retry contract live in
[sdk/python/README.md](../../sdk/python/README.md). One rule is worth stating here:
every failure raises `ConnectError` with a Connect code, and exactly ONE code
is retried — `unavailable`, the daemon-restarting signal the per-child
socket's proxy answers with (HTTP 503 and a Connect body naming the code) —
with bounded exponential backoff. `permission_denied` and `invalid_argument`
raise through immediately. Streaming calls resume rather than hang:
`settled()` replays from the highest event ordinal it has seen, `receive()`
re-opens under its documented at-most-once contract.

A script child spawned with the SDK reports, receives and settles through
the same `Client.inside()` face the wave-2 verbs define — see the script
children section of [agents](agents.md#script-children) for the socket's own contract.

## The Rust SDK

`sdk/rust` is the same control plane from Rust: a protobuf Connect client over
hyper + rustls (ring only — no aws-lc-rs, no native-tls) on a unix socket, a
plain `http://` loopback, or an `https://` listener. Its message types are
generated from `control.proto` by `make proto` too (the `rafiki-gen` binary in
that workspace), so the Rust SDK cannot lag the wire either. `Client::new`
takes one endpoint and an optional bearer token; the token is attached in the
transport, once, for unary and streaming calls alike. The two transports are
`unary` and `server_stream`, with typed verbs (`get_history`, `stream_events`,
`send`, `spawn`, `list_models`, …) over the generated `pb::` types; nothing is
retried, and a raw stream's idle window ends it with an `is_idle()` error.
`follow_events` is the event feed that re-opens itself: it keeps the cursor
from what it yields, re-opens silently on idle and after a backoff on
`unavailable`, yields `Interrupted` so a renderer can drop partial text, and
ends on any error a retry cannot fix. See
[sdk/rust/README.md](../../sdk/rust/README.md); `make check-sdk-rust` is its
gate.
