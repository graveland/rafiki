#!/bin/bash
# Smoke-tests an extracted embedded PostgreSQL tree the way pkg/embedpg runs one:
# initdb with the same flags, a unix-socket-only server with pg_textsearch
# preloaded, every shipped extension created and queried, and a pg_dump /
# pg_restore round trip with the tree's own client tools.
#
# usage: smoke.sh <tree> [scratch-dir]
# Must not run as root (postgres refuses).
set -euo pipefail

tree=$(cd "${1:?usage: smoke.sh <tree> [scratch-dir]}" && pwd)
scratch=${2:-$(mktemp -d)}
data="$scratch/data"
mkdir -p "$scratch"
rm -rf "$data"

fail() { echo "smoke: $*" >&2; exit 1; }
[ "$(id -u)" != 0 ] || fail "refusing to run as root"

# A stray file in the tree is not harmless: PostgreSQL parses every *.control in
# share/postgresql/extension, so a "._x.control" sidecar breaks every query of
# pg_available_extensions (which a migration makes).
stray=$(find "$tree" -name '._*' | sed -n '1,5p')
[ -z "$stray" ] || fail "the tree holds AppleDouble files: $stray"

pg_ctl="$tree/bin/pg_ctl"
psql=("$tree/bin/psql" -X -qAt -v ON_ERROR_STOP=1 -h "$data" -U rafiki)

"$tree/bin/initdb" -D "$data" -U rafiki --auth-local=trust --auth-host=reject -E UTF8 --locale=C >"$scratch/initdb.log" 2>&1 ||
  { cat "$scratch/initdb.log" >&2; fail "initdb failed"; }

cat >>"$data/postgresql.conf" <<CONF
listen_addresses = ''
port = 5432
unix_socket_directories = '$data'
unix_socket_permissions = 0700
shared_preload_libraries = 'pg_textsearch'
CONF
chmod 700 "$data"

"$pg_ctl" -w -D "$data" -l "$scratch/pg.log" start >/dev/null ||
  { cat "$scratch/pg.log" >&2; fail "server did not start"; }
trap '"$pg_ctl" -D "$data" stop -m immediate >/dev/null 2>&1 || true' EXIT

version=$("${psql[@]}" -d postgres -c 'SHOW server_version')
echo "server $version"

# What migration 0036 asks: every extension we ship must be listed as available,
# which forces the server to parse every control file in the tree.
for ext in vector pg_textsearch ltree pg_trgm pgcrypto; do
  have=$("${psql[@]}" -d postgres -c "SELECT count(*) FROM pg_available_extensions WHERE name = '$ext'")
  [ "$have" = 1 ] || fail "$ext is not in pg_available_extensions"
done

"${psql[@]}" -d postgres <<'SQL' >"$scratch/queries.out" 2>&1 || { cat "$scratch/queries.out" >&2; fail "extension queries failed"; }
CREATE EXTENSION vector;
CREATE EXTENSION pg_textsearch;
CREATE EXTENSION ltree;
CREATE EXTENSION pg_trgm;
CREATE EXTENSION pgcrypto;
CREATE TABLE v (id int PRIMARY KEY, e vector(3));
INSERT INTO v VALUES (1,'[1,0,0]'),(2,'[0,1,0]'),(3,'[0.9,0.1,0]');
CREATE INDEX v_hnsw ON v USING hnsw (e vector_cosine_ops);
CREATE TABLE d (id int PRIMARY KEY, content text);
INSERT INTO d VALUES (1,'postgres is a database system'),(2,'cooking pasta at home'),(3,'a distributed database for time series');
CREATE INDEX d_idx ON d USING bm25(content) WITH (text_config='english');
SQL

nearest=$("${psql[@]}" -d postgres -c "SET enable_seqscan = off; SELECT string_agg(id::text, ',' ORDER BY e <=> '[1,0,0]') FROM v" | tail -1)
[ "$nearest" = "1,3,2" ] || fail "vector order is $nearest, want 1,3,2"
top=$("${psql[@]}" -d postgres -c "SELECT id FROM d ORDER BY content <@> 'database system' LIMIT 1")
[ "$top" = 1 ] || fail "bm25 top hit is $top, want 1"
echo "vector and bm25 ok"

"$tree/bin/pg_dump" -h "$data" -U rafiki -Fc -f "$scratch/rt.dump" postgres
"${psql[@]}" -d postgres -c 'CREATE DATABASE rt'
"$tree/bin/pg_restore" -h "$data" -U rafiki -d rt --no-owner --single-transaction "$scratch/rt.dump"
rows=$("${psql[@]}" -d rt -c 'SELECT (SELECT count(*) FROM v) + (SELECT count(*) FROM d)')
[ "$rows" = 6 ] || fail "restored $rows rows, want 6"
top=$("${psql[@]}" -d rt -c "SELECT id FROM d ORDER BY content <@> 'database system' LIMIT 1")
[ "$top" = 1 ] || fail "bm25 after restore is $top, want 1"
echo "dump/restore ok"
echo "smoke ok"
