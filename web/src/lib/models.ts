import type { ModelRow } from '../gen/rafiki/v1/control_pb'

export type SortKey = 'name' | 'cost' | 'context'

/**
 * Total per-token cost as a sort key. A model with neither price known is
 * unpriced, not free, so it sinks to the bottom of a cheapest-first list
 * rather than sorting to the top.
 */
export function costKey(m: ModelRow): number {
  const prompt = m.promptUsd
  const completion = m.completionUsd
  if (prompt == null && completion == null) {
    return Infinity
  }
  return (prompt ?? 0) + (completion ?? 0)
}

/**
 * Sorts a copy of the rows. `context` is descending (largest first) and an
 * unknown context window sorts last; `cost` is ascending and unpriced models
 * sort last; `name` is alphabetical.
 */
export function processed(models: readonly ModelRow[], sortBy: SortKey = 'name'): ModelRow[] {
  const rows = [...models]
  if (sortBy === 'cost') {
    rows.sort((a, b) => costKey(a) - costKey(b))
  } else if (sortBy === 'context') {
    rows.sort((a, b) => (b.contextWindow ?? 0) - (a.contextWindow ?? 0))
  } else {
    rows.sort((a, b) => a.name.localeCompare(b.name))
  }
  return rows
}

/** Case-insensitive substring match on the model name. Empty query keeps all. */
export function filtered(models: readonly ModelRow[], query: string): ModelRow[] {
  const needle = query.trim().toLowerCase()
  if (needle === '') {
    return [...models]
  }
  return models.filter((m) => m.name.toLowerCase().includes(needle))
}

/** OpenRouter reports USD per token; render as USD per million tokens. */
export function perMtok(perToken: number | undefined): number | null {
  if (perToken == null || !Number.isFinite(perToken)) {
    return null
  }
  return Math.round(perToken * 1_000_000 * 100) / 100
}

/** A compact prompt/completion/cache price hint, or null when nothing is priced. */
export function costHint(m: ModelRow): string | null {
  const segments: string[] = []
  const prompt = perMtok(m.promptUsd)
  const completion = perMtok(m.completionUsd)
  if (prompt != null && completion != null) {
    segments.push(prompt === 0 && completion === 0 ? 'free' : `$${prompt}/$${completion} /Mtok`)
  } else if ((prompt ?? completion) != null) {
    segments.push(`$${prompt ?? completion} /Mtok`)
  }
  const cache: string[] = []
  const cacheRead = perMtok(m.cacheReadUsd)
  const cacheWrite = perMtok(m.cacheWriteUsd)
  if (cacheRead != null) {
    cache.push(`r$${cacheRead}`)
  }
  if (cacheWrite != null) {
    cache.push(`w$${cacheWrite}`)
  }
  if (cache.length > 0) {
    segments.push(`cache ${cache.join('/')}`)
  }
  return segments.length > 0 ? segments.join(' · ') : null
}
