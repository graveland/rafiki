# Installing rafiki

**macOS.** `brew install graveland/tap/rafiki`, then `rafiki setup`.

`brew upgrade rafiki` also applies the upgrade to a machine that already runs the services: it
runs `rafiki service refresh`, which migrates the database, then restarts the daemon and executor
services (interrupting live agents; they recover on restart). Run `rafiki service refresh` yourself
after upgrading any other way. If the migration fails nothing is restarted.

**Self-managed installs** (the `install.sh` route, and source builds) update with
`rafiki upgrade`: it downloads the latest release, verifies its signature against
keys built into the binary, replaces `rafiki` and `rafikid` in place, then runs
`rafiki service refresh`. `rafiki upgrade --check` only reports whether an update
exists; `--version vX.Y.Z` installs a specific tag (the only way to downgrade).
A Homebrew or source install is refused unless you pass `--force`, which installs
a self-managed copy in `~/.local/bin` (and tells you to `brew uninstall rafiki`).
Releases before the first signed one cannot be upgraded this way: re-run
`install.sh`, or `brew upgrade rafiki`, once.

**Linux (Debian/Ubuntu).**

```sh
curl -fsSL https://github.com/graveland/rafiki/releases/latest/download/install.sh | sh
```

That installs `rafiki` and `rafikid` into `~/.local/bin` and runs `rafiki setup`.

**What setup needs:** a Claude login (plus `ripgrep`/`rg`, which the fundi
runtime requires on `PATH`). Docker (or OrbStack) is needed only for the
`managed` database mode. API keys are optional.

**What `rafiki setup` does**, in order:

1. Load any saved answers and detect how rafiki was installed.
2. Prerequisites: `rg`, a logged-in Claude Code, and — only for the `managed` database mode — Docker (or OrbStack).
3. Database: rafikid's own PostgreSQL (the default, `--db-mode embedded`), a
   `rafiki-pg` Docker container (`--db-mode managed`), or `--dsn` for a
   bring-your-own postgres. A saved answer is replayed, so an existing install
   keeps the mode it was set up with, and a `RAFIKI_DB` already in `service.env`
   is adopted rather than replaced by the default. A bring-your-own database
   needs `vector` and `pg_textsearch` created BEFORE this migrate — see
   [Deploying rafiki for real](deployment.md#postgresql).
4. Migrate the schema.
5. LLM providers: Anthropic / OpenRouter keys, a local Ollama, or any OpenAI-compatible endpoint, written to `providers.toml` (plus optional recall summaries and embeddings).
6. Daemon service, bound to loopback unless you answer yes to network access.
7. Your admin user and profile.
8. The local executor.

`rafiki doctor` checks the daemon, an executor, and one claude agent end to end;
`--agent fundi:<provider/model>` (repeatable) checks a fundi agent on that model
instead or as well.

### The embedded database

The default database mode. `RAFIKI_DB=embedded` makes `rafikid` run its own
PostgreSQL 18.3, on a unix socket only — no TCP listener, no Docker, nothing to
install:

| | |
|---|---|
| binaries | `~/.cache/rafiki/embedpg/` (`$XDG_CACHE_HOME`), downloaded once from Maven Central (~15 MB per platform) and verified against a sha256 pinned in `rafikid` |
| data | `~/.local/share/rafiki/pg` (`$XDG_DATA_HOME`), mode 0700 — also the socket directory |
| log | `~/.local/state/rafiki/pg.log` (`$XDG_STATE_HOME`) |

Supported: macOS arm64/amd64, Linux amd64/arm64 on glibc (not Alpine/musl). It
starts on demand, from the first `rafikid` command that needs the database, and
`rafikid` never stops it. Under systemd, stopping the rafikid service stops it
too (the unit's cgroup is signalled); with launchd it can keep running after
`rafikid` stops, which is harmless. To stop it yourself:

```sh
~/.cache/rafiki/embedpg/18.3.0-<os>-<arch>/bin/pg_ctl -D ~/.local/share/rafiki/pg stop
```

`rafikid psql` opens `psql` on whatever `RAFIKI_DB` names, the embedded database
included, printing the target first (because `service.env` may name a remote
database); the connection reaches the client in `PG*` environment variables,
never on its command line. The embedded bundle ships no client tools, so `psql`
comes from `PATH`: `brew install libpq && brew link --force libpq` on macOS,
`apt install postgresql-client` on Debian/Ubuntu.

**Backing it up.** Stop the server and copy `~/.local/share/rafiki/pg`, or run
`pg_dump` (version 18 or newer) over the same connection `rafikid psql` uses.

**Re-running.** Setup is safe to re-run: answers are saved in
`~/.config/rafiki/install.toml`, with secrets (the database DSN and your API
keys) in `service.env`/`executor.env` (0600). `rafiki setup --non-interactive`
replays it with no prompts; without a terminal, setup refuses to run unless that
flag is given.

**Secrets and history.** Passing `--dsn`, `--openrouter-key`, or
`--anthropic-key` on the command line writes those secrets into your shell
history; omit them to answer masked prompts instead, or pre-write `service.env`
yourself.

**The executor alone.** `rafiki executor setup`.
