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
The database needs three extensions created by a superuser — `ltree`,
`vector`, `pg_textsearch` — with `pg_textsearch` also listed in
`shared_preload_libraries`.
