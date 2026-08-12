# Provider guard replay fixtures

Real turn sequences exported from the capture store on 2026-08-12, used by the
`ProviderGuard` replay tests. They are the empirical basis for the 5-miss
ejection threshold — see `docs/plans/2026-08-12-provider-cache-guard-design.md`.

| file | window (UTC) | turns | zero-cache turns | expected result |
|---|---|---|---|---|
| `novita_healthy.json` | 08-11 18:00 → 08-12 07:00 | 1203 | 12 | **zero ejections** |
| `coreweave_broken.json` | 08-12 15:06:17 → 17:00 | 214 | 201 | **ejects CoreWeave** on the 5th qualifying miss |

Both are `deepseek/deepseek-v4-pro`, `upstream=openrouter`, `status=complete`,
ordered by `created_at`.

Two fields are derived rather than stored:

- **`provider`** — `conversation_turn` has no provider column. OpenRouter served
  this model line exclusively from Novita until 2026-08-12 15:06:17 UTC and from
  CoreWeave after, confirmed against `openrouter.broadcast`
  (`trace.metadata.openrouter.provider_name`), so the window determines the
  value. The three Novita generations that appear after the cutover are outside
  the exported conversation and do not affect the sequences here.
- **`prefix_hash`** — the real hashes are replaced by `h<N>` dense-rank labels.
  Only equality between consecutive turns matters to the guard, and short labels
  keep the fixtures readable.

To regenerate, see the export query in
`docs/plans/2026-08-12-provider-cache-guard-plan.md`.
