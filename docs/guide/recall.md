# Recall and memory

The daemon indexes what it captures into a searchable recall index: every
captured conversation contributes **windows** — ~3200-char slices of dialogue
with tool-call arguments compacted in — and, when a summaries model is
configured, rolling **summaries**. Tool results are never indexed (they
appear only as size markers in context expansion); neither are the daemon's
own LLM conversations (`recall.ExcludedEntrypoints`). A spawned child may be
flagged `skip_derived_index` (OR-inherited down its subtree), in which case
its conversations still get windows and BM25 search but are never embedded
or summarised. On top of the derived index sits the **memory tree**:
dot-separated paths of small notes each user (or their agents) save
explicitly, always private to the saver.

Agents use six tools: `recall` (hybrid BM25 + vector search, RRF-fused over
memory/summary/window), `recall_context` (expand one hit id), and the memory
CRUD four — `memory_put`, `memory_get`, `memory_tree`, `memory_delete`. The
same surface is on the CLI:

```bash
rafiki recall "payment retry logic" --repo rafiki --limit 20
rafiki recall context w:9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d --before 5
rafiki memory tree projects
rafiki memory put projects.rafiki auth "tokens ride the transport"
rafiki memory get projects.rafiki auth
rafiki memory delete projects.rafiki auth
rafiki memory status
```

Conversation-derived hits follow the credential — an admin's `rafiki recall`
sees every user's conversations, a user's only their own — while memories
are always the caller's own, admin included.

Two optional tables in `providers.toml` configure the LLM-backed halves:

```toml
[embeddings]
url = "https://api.openai.com/v1/embeddings"
api_key_env = "OPENAI_API_KEY"
model = "text-embedding-3-small"
dimensions = 1536

[summaries]
model = "openrouter/<model>"
# max_segment_tokens = 200000
```

`rafiki setup` writes `providers.toml` and owns it while it is unchanged since
setup wrote it (the SHA-256 is kept in `install.toml`); a re-run rewrites an
owned file only when its answers change. Once you edit the file, setup leaves it
alone — it validates it and asks for the keys it references, and offers to
replace it (backing the old file up as `providers.toml.<timestamp>.bak`). Keys
are never written to `providers.toml`; they live in `service.env`.

Without `[embeddings]` search is keyword-only (BM25); without `[summaries]`
there are no conversation summaries. Historical conversations are never
summarized automatically — the summarizer only touches conversations active
after it first ran; use `rafiki memory backfill --since DURATION|RFC3339
--max-cost USD` (admin credential) to arm a budgeted backfill of older ones.

**The database decides what recall can do.** The recall migration creates
`ltree` for memory paths, and `vector`/`pg_textsearch` when the server has them
available (`pg_textsearch` also belongs in `shared_preload_libraries`); both of
those are untrusted extensions, so a superuser may have to `CREATE EXTENSION`
them first. The daemon probes what is actually installed at startup and
degrades instead of failing:

- **No `pg_textsearch`** (always the case on an `embedded` install today):
  recall search is off. `Recall` answers Connect `Unavailable` — "recall search
  needs the pg_textsearch extension, which this database does not have" — and
  `RecallBackfill` refuses the same way (after its authority and budget checks),
  the recall indexer and summarizer do not run, and memory put/get/tree/delete
  work normally.
- **No `vector`**: recall is BM25-only; embeddings are skipped even if
  `[embeddings]` is configured.

**Order matters on a database you provide.** The migration chooses the schema's
shape once, so `vector` and `pg_textsearch` must exist before the first
`rafikid migrate`. Create them afterwards and the tables keep what that first
run chose — `bytea` embedding columns, no bm25 indexes — because migrations
never re-run, while the startup probe now reports the extension present, so the
degradation above no longer applies and search fails at query time instead.
Nothing converts such an install automatically: it needs `ALTER TABLE ... ALTER
COLUMN embedding TYPE vector` on each of `memory`, `conversation_window` and
`conversation_summary`, plus the three bm25 `CREATE INDEX` statements, by hand
(the exact DDL is in `pkg/store/migrations/0036_recall.up.sql`).
