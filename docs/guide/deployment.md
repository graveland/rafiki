# Deploying rafiki for real

[Installing rafiki](install.md) covers one machine: `rafiki setup` puts the
database, daemon and executor on your laptop, and everything talks over a unix
socket. This page is for the other shape: a daemon on a server (a VM or
Kubernetes) that several people, executors and integrations reach over the
network.

There is no `rafiki setup` for this and no published Helm chart (one is
planned). You run the pieces yourself; this page lists what they need.

## The pieces

```
  laptops ── rafiki / web UI / SDK ──┐   Connect + proxy faces, TLS, bearer tokens
                                     ▼
  senders ── rafiki-hooks ──────▶  rafikid ──────▶ PostgreSQL
  (GitHub, Gitea, devices)           ▲  ▲          (vector, pg_textsearch)
                                     │  │
        executors dial IN over TLS ──┘  └── LLM providers (Anthropic, OpenRouter, …)
        (VMs, containers, laptops)
```

| Piece | Where it runs | Needs |
|---|---|---|
| PostgreSQL | managed or self-run | version 18 or newer; two extensions for recall search (below) |
| `rafikid` | one process per daemon identity | the database, TLS cert, provider keys |
| executors (`rafiki executor serve`) | wherever agents should work | outbound TLS to the daemon, an enrollment token |
| `rafiki-hooks` | optional, internet-facing | the daemon's URL and a narrow token |
| clients | anywhere | a profile naming the daemon and a token |

Executors and clients always dial the daemon; the daemon never dials them.
Executors can therefore sit behind NAT or inside a VM with no inbound ports.

## PostgreSQL

`rafikid` refuses to start without `RAFIKI_DB`: history, cost accounting, the
task ledger, user identity, the executor plane and conversation leases all live
in the database, so there is no database-less mode. (The `embedded` mode is a
single-machine install — `rafikid` runs its own PostgreSQL there; see [Installing
rafiki](install.md#the-embedded-database). A server deployment uses a real
database, below.)

- **PostgreSQL 18 or newer.** The schema defaults every id to `uuidv7()`, which
  is PostgreSQL 18's own function and ships with no shim here, so an older server
  fails at the first migration. (`NULLS NOT DISTINCT`, which the schema also
  uses, only needs 15.)
- **Extensions `vector` and `pg_textsearch` are needed for recall SEARCH only.**
  Both are untrusted extensions, and `pg_textsearch` must be in
  `shared_preload_libraries`. The recall migration creates them when the server
  has them available, but that needs superuser and the daemon's role does not
  need to be, and in production should not be, one — so have a superuser
  `CREATE EXTENSION` both before `rafikid migrate`. Without `pg_textsearch` the
  daemon runs and memory put/get/tree/delete work, but recall search is off —
  `Recall` and `RecallBackfill` answer Connect `Unavailable` ("recall search
  needs the pg_textsearch extension, which this database does not have") and the
  recall indexer and summarizer do not run. Without `vector` alone, recall
  search is BM25-only and embeddings are skipped.
- **They must exist BEFORE the first `rafikid migrate`.** The recall migration
  picks the schema's shape once — `vector` embedding columns and the three bm25
  indexes where the extensions are present, `bytea` placeholders and no bm25
  indexes where they are not — and nothing re-runs it later. Install `vector` or
  `pg_textsearch` after that first migrate and the startup probe reports them
  present, so the daemon does not take the degraded path, while the tables keep
  the old shape: recall search then fails at query time. Nothing converts such
  an install automatically: it needs `ALTER TABLE ... ALTER COLUMN embedding
  TYPE vector` on each of `memory`, `conversation_window` and
  `conversation_summary`, plus the three bm25 `CREATE INDEX` statements, run by
  hand — the exact DDL is in `pkg/store/migrations/0036_recall.up.sql` — or a
  database migrated from empty.
- **rafiki needs no TimescaleDB; the migrations create ordinary tables.** A
  database that still holds TimescaleDB's three tables —
  `conversations.conversation_turn`, `openrouter.broadcast` and
  `conversations.raw_http_request` — is converted by migration `0068`. That is a
  stop-the-daemon step with its own backup, disk, pre-flight and rollback story:
  see [Removing TimescaleDB](#removing-timescaledb).
- **The daemon owns the schema.** `rafikid migrate` brings the database to the
  head of the embedded migration chain. It is idempotent, and concurrent callers
  are serialized by an advisory lock, so two daemons booting together apply it
  exactly once. The daemon itself migrates **only with `--dev`**
  (`cmd/rafikid/main.go`); a production daemon started without it runs against
  whatever schema is there and fails loudly on the first write a new column
  needs, so run `rafikid migrate` as its own step.

### Backups

`rafikid db backup <path>` writes a `pg_dump -Fc` archive over the wire (the
connection rides `PG*`, never argv), and `rafikid db restore <path> --force`
puts one back. For a configured `RAFIKI_DB` both need a `pg_dump`/`pg_restore` on
the daemon host's `PATH`: `pg_dump` is refused when it is older than the server
(it cannot dump a newer one), and `pg_restore` must be new enough to read the
archive it is given. The embedded server needs neither: its own tree ships them,
at its exact major, and they are what run. A physical copy of the data directory
(`--format txz|tgz`) is a different thing, embedded-only, chosen only explicitly,
and stops that server for the duration.

The restore is `pg_restore --clean --if-exists --single-transaction`, plus
`--no-owner` only when the connecting role is not a superuser (see Ownership
below): every object the ARCHIVE contains is dropped and recreated in one
transaction, so a failure rolls back, and everything the archive does not contain
is left alone. It destroys what it covers, so `--force` is required; a run
without it only names the database it would replace.

**A post-`0068` dump needs no extension hook, given the right target.** Since
`0068` the archive holds ordinary tables, so `pg_restore --clean` never meets the
`ALTER TABLE ONLY` statement TimescaleDB refuses. Two conditions come with that:

- **The target must be empty or already past `0068`.** Restoring over a database
  migrated before `0068` makes `--clean` drop and recreate that database's own
  TimescaleDB tables, which fails on `ALTER TABLE ONLY` however ordinary the
  archive is. Restore into a fresh database, or migrate the target to `0068`
  first.
- **The archive can still carry `CREATE EXTENSION timescaledb`.** A dump taken
  while the SOURCE still had the extension installed contains that statement, so
  a target database without it needs the extension files on that server and a
  role allowed to create it — an existing database is no different from a fresh
  one here. A dump taken after the extension was dropped carries no Timescale
  object at all.

**A dump taken BEFORE `0068` is restored by hand.** It still holds hypertable
definitions and the extension's own catalog, so it restores only into a server
that has `timescaledb` installed, at the same extension version, and only with
the extension's hooks around `pg_restore`. `rafikid db restore` no longer runs
them — the previous release's does, and that is the simplest rollback. By hand it
is `pg_restore`'s own flags with the two calls bracketing them, as a superuser
against the target database (the hooks are superuser-only), and with the post
call in a shell `trap` so that it runs even when `pg_restore` fails or you
interrupt it: `timescaledb_pre_restore()` leaves `timescaledb.restoring` set for
every later session on that database until it does.

```sh
#!/bin/sh
# Restore a pre-0068 dump into the target database. Run as a superuser: set DSN
# to that database's superuser connection string, or RAFIKI_DB to it.
set -u
DSN=${DSN:-${RAFIKI_DB:?set DSN or RAFIKI_DB}}
DUMP=/var/backups/rafiki/rafiki-pre-0068.dump

psql "$DSN" -X -q -v ON_ERROR_STOP=1 -c 'SELECT timescaledb_pre_restore();' || exit 1

posted=0
post() {
    [ "$posted" = 1 ] && return
    posted=1
    psql "$DSN" -X -q -c 'SELECT timescaledb_post_restore();'
}
trap post EXIT
trap 'exit 130' INT TERM HUP

# The flags `rafikid db restore` would have used. --no-owner is passed only when
# the connecting role is NOT a superuser, and this bracket needs one, so it is
# omitted here: the archive's OWNER TO statements put the original owners back.
rc=0
pg_restore -d "$DSN" --clean --if-exists --single-transaction "$DUMP" || rc=$?
exit $rc
```

`--single-transaction` is what makes a failed restore leave the database as it
was; `--clean --if-exists` drops what the archive replaces, and the `trap` is
what keeps the database usable afterwards — it covers a dropped ssh session too,
which is why `HUP` is in it. Run `rafikid migrate` against the restored database
next — that is the step that makes its tables ordinary.

**Ownership is the connecting role's, decided rather than constant.** `pg_restore
--no-owner` makes that role the owner of everything it creates, so a superuser
restore does not pass it: the archive's `OWNER TO` statements put the original
owners back, which for an in-place restore is the daemon's unprivileged role. A
dump restored by a superuser *with* `--no-owner` hands the whole schema to the
superuser, and the daemon then gets `permission denied` on its first query —
during an incident. If the archive names a role this server does not have, the
restore fails whole rather than reassigning.

**A dump restore that creates extensions is a superuser operation**, and that is
not only a fresh database: the archive creates the untrusted `vector` and
`pg_textsearch` extensions (and `--clean` drops the target's first), and only a
superuser may create those, so a non-superuser attempt fails at that
`CREATE EXTENSION` and rolls back. While the source still has `timescaledb`
installed, its archive carries that extension too, with the same consequence on
any target that lacks it.

**Stop every daemon on the database first**, dump restores included: a restore
replaces tables a running daemon keeps writing to, so those writes are lost and
the rows it adds afterwards need not match the restored schema. `rafikid db
restore` refuses to start while a local daemon answers on its socket for that
reason, and `--force` is the acknowledgement.

### Removing TimescaleDB

rafiki no longer uses TimescaleDB. The migrations create ordinary tables, and
`0068` converted the three hypertables an older database still had. None of this
is needed to run rafiki: once the tables are ordinary, the extension has nothing
left to manage, and the daemon logs one WARN per start — and `rafikid migrate`
one after migrating — while `timescaledb` or `timescaledb_toolkit` is installed,
naming the statements in the second half of this section.

#### Converting the tables (`0068`)

**Stop every daemon writing to the database.** `0068` takes `ACCESS EXCLUSIVE` on
each of the three tables in turn and holds every lock until its one transaction
commits, so a daemon left running blocks the migration and loses whatever it
writes afterwards. TimescaleDB's background job scheduler keeps running once the
daemon stops; it can only cause a retryable deadlock. Readers delay it too — a
Grafana or FDW query on `conversations.v_turn` — so run it where that wait is
affordable, or fail fast: `PGOPTIONS='-c lock_timeout=30s' rafikid migrate --db
"$RAFIKI_DB"` aborts and rolls back rather than queueing behind a long query.

**Take a backup, and rehearse restoring it.**

```sh
rafikid db backup --db "$RAFIKI_DB" /var/backups/rafiki/rafiki-pre-0068.dump
```

`db backup` requires the path (it appends `.dump` when you give a name without an
extension) and writes it on the host that runs the command; `--db` names the
database explicitly, because every invocation loads `service.env` and that is
where `RAFIKI_DB` points. **This dump is a PRE-`0068` dump**: this release's
`rafikid db restore` cannot put it back, because it no longer runs TimescaleDB's
hooks. Rolling back means the previous release's `rafikid db restore --force`
(which still brackets the restore) or the hand procedure under
[Backups](#backups) — rehearse exactly that, before you migrate. A backup you
have not restored is not a rollback.

**Check what the conversion does not carry.** `0068` recreates a table's rows,
owner, ACL, comment, constraints, indexes and identity-sequence state, and nothing
else: `DROP TABLE` does not fail on a trigger, and a column ACL, a table
reloption, row security, a non-default replica identity, a publication
membership, a comment on an index or a constraint, and a sequence ACL are not
carried at all. Run this against the three tables and deal with whatever comes
back before migrating — re-apply it by hand afterwards, or move the object out of
the way:

```sql
WITH three AS (
  SELECT c.*
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
   WHERE (n.nspname, c.relname) IN (('conversations', 'conversation_turn'),
                                    ('openrouter', 'broadcast'),
                                    ('conversations', 'raw_http_request'))
)
SELECT 'column ACL' AS what, t.relname, a.attname AS object
  FROM three t JOIN pg_attribute a ON a.attrelid = t.oid
 WHERE a.attacl IS NOT NULL AND a.attnum > 0 AND NOT a.attisdropped
UNION ALL
SELECT 'table ACL', relname, array_to_string(relacl, ', ') FROM three WHERE relacl IS NOT NULL
UNION ALL
SELECT 'reloptions', relname, array_to_string(reloptions, ', ') FROM three WHERE reloptions IS NOT NULL
UNION ALL
SELECT 'row security', relname, format('enabled=%s forced=%s', relrowsecurity, relforcerowsecurity)
  FROM three WHERE relrowsecurity OR relforcerowsecurity
UNION ALL
SELECT 'replica identity', relname, relreplident::text FROM three WHERE relreplident <> 'd'
UNION ALL
SELECT 'publication', t.relname, p.pubname
  FROM three t JOIN pg_publication_rel r ON r.prrelid = t.oid
              JOIN pg_publication p ON p.oid = r.prpubid
UNION ALL
SELECT 'trigger', t.relname, g.tgname
  FROM three t JOIN pg_trigger g ON g.tgrelid = t.oid
 WHERE NOT g.tgisinternal
   AND NOT EXISTS (SELECT 1 FROM pg_proc pr JOIN pg_namespace pn ON pn.oid = pr.pronamespace
                    WHERE pr.oid = g.tgfoid AND pn.nspname LIKE '\_timescaledb%')
UNION ALL
SELECT 'policy', t.relname, p.polname FROM three t JOIN pg_policy p ON p.polrelid = t.oid
UNION ALL
SELECT 'index comment', t.relname, i.relname
  FROM three t JOIN pg_index x ON x.indrelid = t.oid JOIN pg_class i ON i.oid = x.indexrelid
 WHERE obj_description(i.oid, 'pg_class') IS NOT NULL
UNION ALL
SELECT 'constraint comment', t.relname, k.conname
  FROM three t JOIN pg_constraint k ON k.conrelid = t.oid
 WHERE obj_description(k.oid, 'pg_constraint') IS NOT NULL
UNION ALL
SELECT 'sequence ACL', t.relname, s.relname
  FROM three t JOIN pg_depend d ON d.refobjid = t.oid AND d.deptype IN ('i', 'a')
              JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
 WHERE s.relacl IS NOT NULL;
```

`table ACL` is the one row that is information rather than loss: `0068` regrants
the table's ACL verbatim. The rest is destroyed by the conversion. The
`_timescaledb` filter excludes any trigger TimescaleDB installs itself — an older
install's `ts_insert_blocker`, for instance, whose function lives in
`_timescaledb_functions` — so anything the query does return was added by hand.
Keep a `\d+` of each of the three as well: it is the cheapest before-and-after
diff.

**Size the disk for all three at once, from a measurement, not a formula.** Every
table is rewritten inside ONE transaction, so nothing is reclaimed until it
commits: all three old tables and their three new twins coexist for the whole run.
Compressed chunks are copied out decompressed into ordinary heap and TOAST, so the
new twin of a compressed table is larger than the table it replaces; the
indexes are rebuilt after the rows load; and every byte of the copy and of each
index build is WAL-logged too. No arithmetic on the current sizes is a safe bound,
because the expansion is not bounded by them. Measure instead.

Measure each table's uncompressed footprint — what it takes now, plus what its
compressed chunks need when they are expanded back into ordinary heap and TOAST.
`hypertable_compression_stats` reports no rows for a table that has never been
compressed, so that term is zero there:

```sql
SELECT format('%I.%I', h.hypertable_schema, h.hypertable_name) AS relation,
       pg_size_pretty(
         hypertable_size(format('%I.%I', h.hypertable_schema, h.hypertable_name)::regclass)
         + coalesce((SELECT before_compression_total_bytes
                       FROM hypertable_compression_stats(format('%I.%I', h.hypertable_schema, h.hypertable_name)::regclass)), 0)
       ) AS uncompressed
  FROM timescaledb_information.hypertables h
 WHERE (h.hypertable_schema, h.hypertable_name) IN
       (('conversations', 'conversation_turn'),
        ('openrouter', 'broadcast'),
        ('conversations', 'raw_http_request'));
```

That over-counts a compressed table by its compressed footprint, because the two
terms overlap — the direction to be wrong in. Add the three up, then add the new
indexes (built after the rows are loaded) and the WAL those copies and builds
write.

**Then size the volume from the rehearsal's measured peak, not from that sum.**
Sample the destination through the run — `pg_database_size(current_database())`
once a minute, or the volume's own high-water mark — and use that number with
headroom on top. The sum above is the estimate you check the rehearsal against; a
figure from another install is not a measurement of yours.

Check `max_wal_size`, archive and replication-slot headroom, and replica lag with
the rehearsal's peak in hand: it is gigabytes inside a single transaction.
`conversations.raw_http_request` is a debugging aid, so `TRUNCATE
conversations.raw_http_request` before the backup makes the dump, the rehearsal
and the conversion all cheap.

**Migrate.** `rafikid migrate --db "$RAFIKI_DB"`. `0068` is one transaction, so a
failure — a lock timeout, a row count that does not match, a volume that filled —
rolls the whole of it back and leaves the three tables exactly as they were. The
rollback writes no second copy: it logs an abort record and unlinks the
relfilenodes the copy created, so it gives that space back. The hazard is the copy
itself — a WAL volume that fills during it PANICs the server, and crash
recovery then needs the space back before it can finish; a data volume that
fills stops the copy with an ordinary error, which rolls the migration back.
That is what the measured peak and its headroom are for, and what the backup is
for.

**Start the daemon, then take the dump you would actually restore from.**

```sh
rafikid db backup --db "$RAFIKI_DB" /var/backups/rafiki/rafiki-post-0068.dump
```

It holds ordinary tables. While `timescaledb` is still installed it also carries
`CREATE EXTENSION timescaledb` (see [Backups](#backups)); after the extension is
dropped, a dump carries no Timescale object at all.

#### Dropping the extension

Optional. An installed extension is harmless once the tables are ordinary — the
daemon only warns — so leaving it in place is a supported end state.

**Keep the right backup.** The dump from the step above is the one to hold on to.
Every dump taken while `timescaledb` was installed carries
`CREATE EXTENSION timescaledb` and restores only into a server with the extension
files at the same version, so dropping the extension strands the older ones. Take
one more after the drop if you want an archive that needs nothing of TimescaleDB.

**rafiki never drops it itself.** Dropping the extension needs a superuser, and
the daemon's role deliberately is not one; the server may also hold hypertables
that are not rafiki's. `DROP EXTENSION` without `CASCADE` REFUSES while any
dependent object remains and names what it found, so the drop is blocked rather
than destructive — `DROP EXTENSION timescaledb CASCADE` is the destructive form,
and nothing here uses it. The checks below tell you whether the drop will be
blocked, and what it would name.

**The preload list is per CLUSTER; the catalog checks are per DATABASE.**
`shared_preload_libraries` is a server setting: taking `timescaledb` out of it and
restarting changes every database on that PostgreSQL cluster, while `pg_extension`
and `timescaledb_information.hypertables` answer for the one database you are
connected to. Inventory all of them first, as a superuser (both `psql` calls take
their connection from `PG*`, so run it on the server's host):

```sh
psql -X -At -d postgres -c "SELECT datname FROM pg_database WHERE datallowconn" |
while read -r db; do
  printf '%-24s %s\n' "$db" \
    "$(psql -X -At -d "$db" -c "SELECT coalesce(string_agg(extname, ', '), '-') FROM pg_extension WHERE extname IN ('timescaledb','timescaledb_toolkit')")"
done
```

The filter is `datallowconn` and nothing else, so **`template1` is in that list**:
it is a database like any other for this purpose, `pg_extension` answers for it,
and an image-built cluster routinely has `timescaledb` installed there — the
`timescaledb-ha` image's own initdb does exactly that. Anything installed in
`template1` is copied into every database created from it afterwards, so leaving
it out hides the extension from precisely the databases that will inherit it.
`template0` is excluded because it accepts no connections, and it never carries
the extension.

Every database has to answer `-` before you touch `shared_preload_libraries`. A
database that still uses TimescaleDB is worse off without the preload than with
it: the extension's event triggers are C functions from the library, so DDL in
that database FATALs. A PostgreSQL cluster dedicated to rafiki makes this a
one-line answer, which is the other way to close it.

**Drop first, then remove the preload entry and restart.** In that order, in every
database that has it, and the wrong order has a specific cost:

- **Installed and preloaded** (the normal state): drop `timescaledb_toolkit` and
  then `timescaledb` in each database that has them, then edit the preload list
  and restart.
- **Installed but NOT preloaded** (you removed the preload entry first): in every
  such database, `DROP EXTENSION timescaledb_toolkit` and every other DDL
  statement — `rafikid migrate` included — FATALs with `extension "timescaledb"
  must be preloaded`. `DROP EXTENSION timescaledb` itself still works.
- **The way back**, without a restart: in each affected database run
  `DROP EXTENSION timescaledb;` first and drop the toolkit after it. Or put
  `timescaledb` back into `shared_preload_libraries` and restart, which restores
  the state the first bullet assumes.

In each such database, as a superuser, look before you drop:

```sql
SELECT * FROM timescaledb_information.hypertables;
```

On a database `rafikid migrate` has taken to `0068` or later the answer is no rows
for rafiki's three, because they are ordinary tables. Anything else listed belongs
to something else on this server: it is not rafiki's, and the drop below will
refuse while it is there.

Then, while `timescaledb` is still preloaded, drop the toolkit first:

```sql
DROP EXTENSION timescaledb_toolkit;
DROP EXTENSION timescaledb;
```

`timescaledb_toolkit` is absent on many installs; when that first statement
reports it does not exist, skip it and run the second. Neither extension declares
a dependency on the other in `pg_depend`, so this order is caution rather than a
requirement — but it is the safe order, and the order the daemon's WARN prints.

Only when every database on the cluster answers `-` for `timescaledb` do you
remove it from `shared_preload_libraries` in `postgresql.conf` and restart the
server. Nothing rafiki's schema needs comes from it, so this is a step for a
database that already had it.

## The daemon image

The `Dockerfile`'s default target, `release`, builds the daemon image:
`rafikid` plus the `rafiki` CLI, `ripgrep` (which the `fundi` runtime requires
on `PATH`) and `rtk`. It runs as uid 1000 (`rafiki`), has `rafikid` as its
entrypoint, and exposes two ports:

| Port | What |
|---|---|
| `8035` | the proxy face (`RAFIKI_PROXY_LISTEN`): Anthropic `/v1/messages`, OpenAI `/v1/chat/completions`, `/mcp`, `/healthz`, `/metrics`. Children reach their own daemon here over loopback |
| `8036` | the TLS control listener (`RAFIKI_CONTROL_LISTEN=tcp:8036`) |

```sh
docker build -t rafikid .
docker run -d --name rafikid -p 8036:8036 \
  -e RAFIKI_DB=postgres://rafiki:…@db.internal:5432/rafiki?sslmode=verify-full \
  -e RAFIKI_DAEMON_ID=rafikid-0 \
  -e RAFIKI_CONTROL_LISTEN=tcp:8036 \
  -e RAFIKI_CONTROL_TLS_CERT=/tls/tls.crt \
  -e RAFIKI_CONTROL_TLS_KEY=/tls/tls.key \
  -e RAFIKI_EXECUTORS_ENABLED=1 \
  -e ANTHROPIC_API_KEY=… \
  -v /etc/rafiki/tls:/tls:ro \
  rafikid
```

### What the daemon needs

| Setting | Why |
|---|---|
| `RAFIKI_DB` | required; see above |
| `RAFIKI_DAEMON_ID` | a stable name for this daemon, recorded as the holder of its conversation leases so a restarting daemon reclaims its own children at once instead of waiting out the 5-minute lease TTL. **Required where the filesystem is ephemeral** (Kubernetes) because the laptop default generates one into the data dir. **Must be unique per running daemon**: two daemons sharing a value reclaim each other's leases and both drive the same conversation |
| `RAFIKI_CONTROL_LISTEN`, `RAFIKI_CONTROL_TLS_CERT`, `RAFIKI_CONTROL_TLS_KEY` | the remote control plane. All three or the daemon refuses to start; TLS is mandatory on TCP. Certificates are re-read on every handshake, so cert-manager rotation needs no restart |
| `RAFIKI_EXECUTORS_ENABLED=1` | executor enrollment is **off** by default once a TCP control listener exists, so that opening the control plane does not silently open executor enrollment. Turn it on deliberately |
| provider keys | `ANTHROPIC_API_KEY` for `fundi` children (and `OPENROUTER_API_KEY` for non-Anthropic models) in the daemon's environment, or a `providers.toml` in its config dir |
| `RAFIKI_CONFIG` | optional path to `rafikid.toml` (telemetry, and the `[mcp_oauth]` destination policy) |
| `RAFIKI_SECRETS_KEY` | optional; seals provider logins and MCP OAuth credentials at rest. Without it those logins answer `Unavailable` and the rest of the daemon runs normally |

The whole variable list is in [Configuration](configuration.md#environment) and
`.env.example`. Config files (`providers.toml`, `oidc.toml`, `rafikid.toml`,
`instructions.md`, `mcp.json`) are read from the daemon's config dir
(`$XDG_CONFIG_HOME/rafiki`, i.e. `~/.config/rafiki` for the `rafiki` user);
mount them as a ConfigMap/Secret or a volume.

### One hostname, one port, one certificate

The TLS listener serves everything remote parties need: the Connect control
plane, the executor link (`/executor/connect`), the daraja link
(`/daraja/connect`), the proxy faces, `/rafiki.v1.Login/`, `/healthz` and
`/metrics`. The executor and daraja links are HTTP/1.1 connection upgrades, so
the listener advertises only `http/1.1` in ALPN and requires TLS 1.2 or newer.

If you put a load balancer or ingress in front, it must pass HTTP/1.1 upgrades
through to the daemon. Terminating TLS there is possible, but the daemon's
listener is TLS-only, so re-encrypt to it. Every request is authenticated
by the daemon itself; the network layer is not trusted to do it.

## Identity

There is no bootstrap mode. A fresh daemon with a database has zero users, logs
that once, and refuses every unauthenticated remote command. Create the first
admin on a host that can reach the database (a `kubectl exec` into the pod, or
any machine with the DSN):

```sh
rafikid user create alice --admin --token
```

The plaintext token prints once. Then, from a laptop:

```sh
rafiki profile add prod --url https://rafiki.example.net --token "$TOKEN"
rafiki profile use prod
```

For anything beyond a single shared token, configure single sign-on (`oidc.toml`)
so people log in with `rafiki login` and tokens expire on your terms. Both
flows, the `oidc.toml` keys and the provider recipes are in
[Authentication](authentication.md). Automation (CI, cron) uses minted service
tokens from the same page.

Ownership follows the user: presets, conversations and executors belong to the
user who created them, and an executor only serves its owner's agents.

## Executors

Agents have no workspace tools until an executor exists, and on a server the
daemon's own filesystem is deliberately not one. Run executors where you want
agents to work:

```sh
# on the machine the executor will run on
rafiki executor name builder-1
# wherever you are logged in as the owner: mint the enrollment token
rafiki executor enroll --name builder-1
# back on the executor machine
rafiki executor serve --connect rafiki.example.net:443 --enroll-token <token> \
  --root /work --launch claude
```

Run it under a service manager (`rafiki executor service install`) or as a
container:

```sh
docker run -d --name rafiki-executor -v /srv/work:/work \
  ghcr.io/graveland/rafiki-sandbox:latest \
  rafiki executor serve --connect rafiki.example.net:443 --enroll-token <token> --root /work
```

Declare what the machine is on its row, because the executor asserts nothing
about itself: `rafiki executor create --isolation container --workspace-mode
ephemeral --root /work --label env=ci`. `ephemeral` machines are interchangeable
(a lost executor reschedules its children elsewhere); `pinned` ones park their
children until they return. `--launch claude` lets the machine host Claude Code
children through daraja, and `--launch script` lets it host script children.

Isolation is whatever you compose: a container's mounts, a VM, a dedicated
host user. `--root` is a working directory, not a sandbox. Details, the relay
for local inference servers and sandboxes are in
[Executors, daraja and sandboxes](executors.md).

## Remote MCP servers that need a login

A `fundi` child's MCP servers come from `mcp.json` on the daemon, and a direct
`url` entry may require OAuth 2.1. The daemon brokers that login: a user runs
`rafiki mcp login <server>`, the daemon runs the whole flow, and children spend
the sealed grant passively. [Agents and subagents](agents.md#mcp-servers-and-oauth)
is the user's view, [MCP OAuth](../reference/mcp-oauth.md) the runtime contract,
and [Configuration](configuration.md#mcp-oauth) the `[mcp_oauth]` keys. What a
server deployment has to get right:

- **The schema first.** `rafikid migrate` must reach migration 0067
  (`conversations.mcp_credential`, `conversations.mcp_endpoint`) before a daemon
  of this release starts — it is already the upgrade step, and skipping it lands
  the login verbs on missing tables. The migration only ADDS two tables and their
  indexes, so a previous-release daemon keeps running against the migrated
  schema: no stop-the-world, just don't skip it.
- **One secrets key, shared.** `RAFIKI_SECRETS_KEY` seals MCP client secrets and
  access/refresh tokens exactly as it seals provider logins — one AES-256-GCM
  ciphertext per grant, bound to its owner and canonical resource, so it cannot
  be opened for another user or moved to another server. Every daemon serving
  the same database needs the same key; replacing the value makes every stored
  grant unreadable and everyone logs in again. There is no MCP-specific key.
- **Reach: the MCP server AND its OAuth endpoints.** The daemon, not the client
  and not the executor, contacts the configured MCP endpoint plus whatever
  discovery points at — protected-resource metadata, authorization-server
  metadata, dynamic client registration, the token endpoint, and revocation. All
  of them must be reachable from the daemon host.
- **Public HTTPS is the default; the opt-in is the operator's — for servers you
  log into.** Only an entry with a stored grant dials through the guarded
  client: that destination must resolve to a public unicast address, a cleartext
  `http://` URL is refused, and the daemon's own `HTTP_PROXY`/`HTTPS_PROXY` are
  ignored for that dial (a proxy resolves the name itself, so the destination
  check would not see the address reached). An internal MCP server you log into
  therefore needs its exact host in `[mcp_oauth] allow_hosts` (or its range in
  `allow_cidrs`), and an `http://` one additionally needs the host in
  `allow_http_hosts` — see [Configuration](configuration.md#mcp-oauth) for all
  three lists and the bounds. That file is the only thing that can widen the
  policy: a project `.mcp.json`, a flag passed to `rafiki mcp login`, and a
  server's own metadata cannot.
- **Logging in does not re-route your other MCP servers.** An entry with an
  explicit `Authorization` header, an entry with no stored grant (a public or
  unauthenticated server, or one using other custom headers), and every stdio
  and `via_executor` entry keep the transport they had before —
  `DefaultTransport` and the daemon's proxy environment included. Two rules with
  two scopes: the exact-endpoint header containment covers EVERY direct-`url`
  entry that carries configured headers (a static `X-Api-Key` or a hand-set
  `Authorization` never follows a redirect either), while the destination policy
  and the bearer drop on an origin change belong to OAuth-backed dials. A
  protected server with no stored grant still answers 401 with the
  `server <name> needs a login` hint, because the passive handler is attached to
  every direct-`url` entry that names no `Authorization` of its own. One case
  gets no hint: a connection with no authenticated owner (a token-less local
  caller). Its entry dials unauthenticated, and a 401 on it stays a 401 — there
  is nobody for `rafiki mcp login` to store a grant for, so the daemon does not
  pretend a login would help.
- **The config file must exist on the daemon host.** `rafiki mcp login` resolves
  an ABSOLUTE path (`--mcp-config` > `./.mcp.json` > `$RAFIKI_MCP_CONFIG` >
  `<config dir>/mcp.json`) and then the daemon reads that exact file and entry;
  a relative path is refused, and nothing uploads a project file. With a remote
  profile, mount the file where the daemon can read it (or point
  `RAFIKI_MCP_CONFIG` / the config dir's `mcp.json` at it). Running
  `rafiki mcp login` from a project directory names a path that exists only
  there unless the daemon has the same tree.
- **The login callback is on the user's machine, at a fixed port.** The CLI
  serves `127.0.0.1:1456` at `/mcp/oauth/callback` — the redirect URI the login
  registered, so the port cannot move and a user needs it free. The URL is
  always printed, and the redirect URL can always be pasted back; `--no-browser`
  only skips opening a browser, which is the flow for a browser on another
  machine (clipboard the URL across, paste the redirect back into the
  terminal). Nothing is relayed through the daemon.
- **Without the key, this feature is unavailable and nothing else degrades.**
  The capability needs the daemon's credential store — the database, which
  `rafikid` cannot start without — and `RAFIKI_SECRETS_KEY`, so the key is the
  part an operator supplies. Without it the daemon logs the fact once (`MCP
  OAuth is unavailable` with the reason), the three login verbs answer
  `Unavailable`, and children dial exactly as before: a static-header or
  unauthenticated MCP server keeps working, and a protected one is skipped with
  the login hint. Standalone `rafikid fundi` is unaffected either way, since it
  holds no store.
- **A refresh that cannot be completed fails closed.** Before spending a
  single-use refresh token the store commits a durable intent, makes at most one
  upstream attempt, and then re-checks the grant under the owner lock. A
  refusal, a crash or a rotation that cannot be committed leaves that grant
  login-required: there is no automatic replay, even after a restart, and the
  fix is `rafiki mcp login <server>` again. A refresh merely IN FLIGHT in
  another session is the transient case, and it reads like the permanent one: a
  second acquisition is refused as login-required for the duration, so a turn
  can tell the user to log in while a concurrent session is already rotating the
  token. Retrying once that refresh completes succeeds with the rotated token
  and needs no login. A database error refuses use of the credential and is
  reported as a store failure, never as "no credential".
- **Logout is authoritative; upstream revocation is best effort.** `rafiki mcp
  logout` drops every endpoint binding to the grant locally first, advancing a
  fence so an older in-flight login cannot recreate it, and only then offers the
  token back to the authorization server — one attempt per grant, bounded at
  5s, which is how long the logout call itself can block; it is skipped when the
  issuer advertises no revocation endpoint. A failed revocation changes nothing:
  the local invalidation already stands, and the child's next request gets the
  re-login hint.
- **Scope.** This is the direct-`url` `fundi` path only. `via_executor` entries
  keep their static headers (their argv, environment and credentials live on the
  executor machine), stdio servers keep their own credential management,
  standalone `rafikid fundi` is unchanged, and a `claude` child authenticates
  with Claude Code on its own host — a headless child cannot run that login for
  anyone. Rafiki's own `/mcp` face stays a bearer-only agent-control surface and
  advertises no protected-resource metadata: this is outbound OAuth, not a new
  authorization server.

## Webhooks from third parties

Put `rafiki-hooks` (a separate static, nonroot, distroless image) at the
internet edge instead of exposing the daemon. It holds one token that can do
nothing but `Notify`, and no database credential. Setup, the route config, the
response codes and the deploy-order rule for notify tokens are in
[Webhook gateway](webhooks.md).

## Observability

- `GET /healthz` answers `ok` on the daemon and on the gateway (the gateway's
  probe does not touch the daemon, so liveness and delivery are separate
  signals).
- `GET /metrics` serves Prometheus metrics.
- Traces: set `OTEL_EXPORTER_OTLP_ENDPOINT` (or `[telemetry]` in
  `rafikid.toml`). One trace per agent turn; prompt text is off unless you opt
  in. See [Telemetry](../reference/telemetry.md).
- `RAFIKI_LOG_LEVEL=debug` for per-request housekeeping lines.

## Upgrading

1. Upgrade the **daemon** first, and run `rafikid migrate` before starting it
   (a production daemon does not migrate itself — only `--dev` does). Migration
   0067, the MCP OAuth credential tables, is part of this step: a daemon serving
   `rafiki mcp login` needs it. If the database still has TimescaleDB's three
   tables, `0068` makes this a stop-the-daemon migration with its own disk and
   backup requirements — see [Removing TimescaleDB](#removing-timescaledb).
   Setting `RAFIKI_SECRETS_KEY` and logging into a
   server does not change which destinations your other MCP servers may reach —
   only an entry with a stored grant dials through the guarded client.
2. Then upgrade executors and clients. The wire protocol carries an epoch that
   is bumped on every breaking change; a mismatched peer is refused loudly
   rather than misbehaving, so a client or executor left behind shows up as an
   error naming the epoch, not as corrupt data. `rafiki upgrade` upgrades a
   self-managed install (verifying the release signature against keys built
   into the binary); Homebrew users use `brew upgrade`.
3. Restarting a daemon interrupts live agents. They recover on restart: leases
   are reclaimed by `RAFIKI_DAEMON_ID`, undelivered inbox messages replay, and
   executors reconnect with backoff.
4. **Mint notify tokens only after every daemon sharing the database is
   upgraded, and revoke them before rolling any daemon back.** An older daemon
   reads a notify-scoped token as a full-authority one.
5. Before adding a migration while another branch is deployed, check the
   highest version in the target database: the chain must stay contiguous.

### Upgrading across a backfill

A migration that rewrites data (migration 0057, which places every existing
conversation on the content-addressed tree, is the worked example) is a
**stop-the-daemon** upgrade: it holds `ACCESS EXCLUSIVE` on the message table
while it runs, and a daemon of the *old* release still writing afterwards leaves
rows the new one cannot see. The full procedure and its reasoning are in
[Conversation tree → Cutover procedure](../reference/conversation-tree.md); the
order that matters:

1. Truncate the debugging tables you do not need in the backup
   (`conversations.raw_http_request`), then `pg_dump -Fc` with the daemon
   stopped (`rafikid db backup --format dump <path>` is the same dump, when the
   daemon host has a matching `pg_dump`). **The dump is the rollback** — a
   migration that rewrites data has no down path worth trusting.
2. Rehearse the restore (into a fresh database, or in place as `rafikid db
   restore --force <path>`), migrate, `rafikid tree check`, repeat until clean.
3. On the real database: stop every daemon writing to it, `rafikid migrate`
   (capture the log — the backfill's own validity signal is a line only it
   prints), `rafikid tree check` (must exit 0), `VACUUM FULL` the rewritten table
   if the migration changed its `fillfactor`, then start the new daemon.
4. After go-live the same `rafikid tree check` is the health probe and must exit
   0; it reports structure only, so a fork or compaction does not move it.

An installation upgrading from a release before the conversation tree must first
upgrade to the last release carrying migrations 0057-0061 and run the gate there:
see [Conversation tree → Cutover procedure](../reference/conversation-tree.md).

Measured on a 3,583-conversation / 321,165-message database: 1 m 07 s to migrate
and 49 s for the check, about 2 minutes of downtime, 63 MB peak RSS.

## Production checklist

- [ ] Postgres ≥ 15 with `vector` and `pg_textsearch`; the daemon's role is not a superuser
- [ ] `RAFIKI_DB` uses a verified-TLS connection if the database is remote
- [ ] `RAFIKI_DAEMON_ID` set and unique per daemon
- [ ] Control listener has a real certificate; clients connect with `https://`
- [ ] First admin created with `rafikid user create --admin`; the token is stored somewhere safe
- [ ] SSO configured, or tokens minted per person and per automation, never shared
- [ ] `RAFIKI_EXECUTORS_ENABLED=1` only because you intend to enroll executors
- [ ] Executors run inside the isolation you declared on their rows
- [ ] Provider keys are in the daemon's environment or `providers.toml`, not on clients
- [ ] `/healthz` wired to your probes, `/metrics` scraped, an OTLP endpoint configured if you want traces
- [ ] The webhook gateway, if any, runs as its own image with only a notify-scoped token
- [ ] `RAFIKI_SECRETS_KEY` is set, backed up and identical on every daemon sharing the database
- [ ] `[mcp_oauth]` names each internal MCP server you log into, and its OAuth endpoints, if they are not public HTTPS; other MCP servers keep their own transport
