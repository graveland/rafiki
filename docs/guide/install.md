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
PostgreSQL 18.6 with the `vector` and `pg_textsearch` extensions (so recall
search works), on a unix socket only — no TCP listener, no Docker, nothing to
install:

| | |
|---|---|
| binaries | `~/.cache/rafiki/embedpg/` (`$XDG_CACHE_HOME`), downloaded once from this repository's `embedpg-v<version>` release (~17 MB on Linux, ~73 MB on macOS) and verified against a sha256 pinned in `rafikid`; they include `psql`, `pg_dump` and `pg_restore` |
| data | `~/.local/share/rafiki/pg` (`$XDG_DATA_HOME`), mode 0700 — also the socket directory |
| log | `~/.local/state/rafiki/pg.log` (`$XDG_STATE_HOME`) |

Supported: macOS arm64/amd64, and Linux amd64/arm64 on glibc 2.38 or newer
(Ubuntu 24.04, Debian 13; not Alpine/musl). It
starts on demand, from the first `rafikid` command that needs the database, and
`rafikid` never stops it as a side effect. The deliberate exceptions are both
physical ones — `rafikid db backup --format txz|tgz` and `rafikid db restore` —
which stop it for their work and start it again afterwards, under the lock every
other rafikid process takes (below). Which process starts it decides whether
stopping the service stops it: when the daemon starts it, the server lands in
the service's cgroup and stops with it (systemd signals the whole group;
launchd does not, so it can keep running, harmlessly). When the first command
was run from your shell — `rafiki setup` runs `rafikid migrate` that way — the
server lives outside the unit and keeps running after the service stops, until
you stop it yourself:

```sh
~/.cache/rafiki/embedpg/18.6-1-<os>-<arch>/bin/pg_ctl -D ~/.local/share/rafiki/pg stop
```

`rafikid db psql` opens `psql` on whatever `RAFIKI_DB` names, the embedded
database included, printing the target first (because `service.env` may name a
remote database); the connection reaches the client in `PG*` environment
variables, never on its command line. `rafiki db …` forwards to it for a local
profile and refuses one whose daemon is on another machine — the database lives
on the daemon's host, so run it there. That check is about where the DAEMON is,
not the database: `rafikid` resolves `RAFIKI_DB` on its own host (its
environment, and `service.env` when that is where the daemon's settings live), so
a local daemon pointed at a remote database means these verbs act on that remote
one. Read the target line it prints before giving a destructive verb `--force`. Inside the private cluster rafiki's
database is named `rafiki`, created on the first use of the embedded mode, so
`\l` lists it next to the `postgres` maintenance database — which is initdb's
and never holds rafiki data. Embedded provisioning also sets the `rafiki` role's
`search_path` to `public, conversations` for this database only, repairing that
default on existing installs as well. `db psql` independently supplies the same
session default (including for external databases); explicit DSN `options` can
override it. `public` stays first so extension creation still uses that schema.
A server keeps running the build it was started from, so a new build is picked up
when `rafikid` next resolves the database with no daemon running — which is what a
service restart after `rafiki upgrade` does: the new daemon finds the old server
running from an older tree and restarts it onto the new one (a few seconds of
downtime, and the point of shipping fixes in the server). A `rafikid` command run
while the daemon is up never restarts the server under it; it prints that the old
build is still running and to restart the service.

Its extensions are managed for you. Each time `rafikid` resolves the embedded
database it installs `vector` and `pg_textsearch` if they are missing and brings an
installed one to the version the downloaded tree ships, so a new tree updates them
on the next start. That is the embedded server only: any other `RAFIKI_DB`'s
extensions are its operator's. The reconcile installs the extension and nothing
else; the columns and indexes that use it come from the migrations.

For the embedded server, `psql`, `pg_dump` and `pg_restore` are the ones its own
tree ships, built with the server and so exactly its major version: nothing to
install, and nothing on `PATH` is consulted. For any other `RAFIKI_DB` they come
from `PATH`; `pg_dump` is refused when it is older than the server (it cannot
dump a newer one), and `pg_restore` must be new enough to read the archive it is
handed, so install the server's own major:
`brew install libpq && brew link --force libpq` on macOS,
`apt install postgresql-client` on Debian/Ubuntu (the distro package may lag the
server's major; add the PGDG repository if it does).

**Backing it up.** `rafikid db backup <path>` writes one file. By default it is a
`pg_dump -Fc` logical dump (the portable one): for the embedded server taken with
the `pg_dump` in its own tree, for any other `RAFIKI_DB` with the one on `PATH`
(refused if there is none, or if it is older than the server). A physical copy of
the data directory is chosen only explicitly — `--format txz`, or `--format tgz`.

A dump copies over the wire and never stops anything. A physical copy has to
stop the embedded server for the whole copy plus its compression, taking the
database away from whatever uses it — a live daemon and every turn in flight —
for that time, so a server that is RUNNING refuses without `--force` (a server
that is already stopped is copied as it stands). It starts the server again
afterwards, on a failure or an interrupt too, and a daemon picks the database
back up with its next request rather than waiting for a restart. If you stop the
server yourself and leave it down, nothing brings it back while rafikid runs — a
daemon never starts a server that went away underneath it — and it reconnects by
itself once something does. The copy is embedded-only, and a backup path inside
the data directory is refused (the archive would contain itself).

`rafikid db restore <path> --force` puts a backup back. It sniffs the file's
content — a `PGDMP` dump, or a compressed tar of the data directory — and never
trusts its extension. A restore destroys what it covers, so it does nothing
without `--force`: it names what it would replace and exits non-zero. A dump is
restored with `pg_restore --clean --if-exists --no-owner --single-transaction`,
so a failure rolls back rather than leaving a half-rebuilt database.

**Ownership.** `pg_restore --no-owner` makes the CONNECTING role the owner of
everything it creates, so whether the restore passes it is decided by that role
rather than fixed: a non-superuser restore keeps it (it can only own what it
creates), and a superuser restore omits it, so the archive's own `OWNER TO`
statements put the original owners back — for an in-place restore of this
install, the daemon's own unprivileged role. Restoring as a superuser *with*
`--no-owner` would hand the whole schema to that superuser and leave the daemon
with permission denied on its first query, which is why the flag follows the
role. A role the archive names that does not exist on the target fails the
whole restore, loudly, rather than having its objects reassigned.

A post-`0068` dump restore needs no extension hook: the archive holds ordinary
tables, so `pg_restore --clean` never meets the statement TimescaleDB refuses.
Two conditions come with that.

**The target must be empty or already past `0068`.** Restoring over a database
migrated before `0068` makes `--clean` drop and recreate that database's own
TimescaleDB tables, which fails on `ALTER TABLE ONLY` however ordinary the archive
is. Restore into a fresh database, or migrate the target to `0068` first.

**The archive can still carry `CREATE EXTENSION timescaledb`.** A dump taken while
the source database still had the extension installed contains that statement, so
any target database without it needs the extension files on that server and a
role allowed to create it — an existing database is no different from a fresh one
here. A dump taken after the extension was dropped carries no Timescale object at
all. (A dump taken before `0068` still needs the extension's hooks run by hand,
with an exact procedure and a shell `trap` — see [Deploying rafiki →
Backups](deployment.md#backups).)

A restore that creates extensions is a superuser operation, and that is not only a
fresh database: rafiki's own dump creates `vector` and `pg_textsearch` as well,
and both are UNTRUSTED extensions only a superuser may create — so a
non-superuser attempt fails at that `CREATE EXTENSION` and rolls back.

Stop the rafikid service before a dump restore: it replaces tables a running
daemon keeps writing to, so those writes are lost. A dump restore also REFUSES to
start while a local `rafikid` answers on its socket, for exactly that reason —
`--force` accepts it if you have decided you can.

A dump restore runs over the wire and stops nothing. A physical one stops the
embedded server (and starts it again), so the backup's downtime rules apply to
it: it unpacks the archive into a staging directory and checks it BEFORE moving
anything, moves the live data directory to `<data dir>.replaced-<unix>` — never
deleted — and unpacks the archive in its place. It does not stop a running
rafikid: stopping the server drops its connections, so stop the service first.

Interrupting a backup or a restore (Ctrl-C, SIGHUP, SIGTERM) cancels it rather
than killing it, so the safety work still happens — a physical restore never
starts the swap (it checks again after a stop that can take 90 seconds), restarts
the server it stopped and leaves the data directory untouched — and the command
exits non-zero. Once the two renames have begun the command finishes the swap —
the renames themselves are not interruptible — and an interrupt that arrives then
rolls the completed swap back: the previous data directory is put back, the
restored tree is discarded, and the command exits non-zero. A second interrupt
kills the process outright, which can leave a physical restore's marker in
place.

An interrupted physical restore leaves `<data dir>.restore-in-progress` and a
`.replaced-<unix>` sibling, and `rafikid` then refuses to initialise an empty
cluster there: a missing data directory next to a preserved one is an
interrupted restore, not a fresh install. Deal with both by hand before
starting again.

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
