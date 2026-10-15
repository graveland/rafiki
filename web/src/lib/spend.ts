import type { ConversationQueryResponse, QueryValue } from '../gen/rafiki/v1/control_pb'

/** One decoded row of the `spend` catalogue query. */
export interface SpendRow {
  day: string
  model: string
  provider: string
  turns: number
  inputTokens: number
  outputTokens: number
  cacheReadTokens: number
  cacheCreationTokens: number
  costUsd: number
  priced: boolean
}

/** A cost stack segment: one day, one group key, one priced cost. */
export interface StackPoint {
  day: string
  key: string
  costUsd: number
}

export type GroupKey = 'model' | 'provider'

export interface SpendTotals {
  costUsd: number
  anyUnpriced: boolean
}

function strCell(cells: Map<string, QueryValue>, name: string): string {
  const cell = cells.get(name)
  if (cell?.v.case === 'strValue') {
    return cell.v.value
  }
  return ''
}

function numCell(cells: Map<string, QueryValue>, name: string): number {
  const cell = cells.get(name)
  if (cell?.v.case === 'intValue') {
    return Number(cell.v.value)
  }
  if (cell?.v.case === 'floatValue') {
    return cell.v.value
  }
  return 0
}

/**
 * Maps every cell to its column's NAME, never its position, so a response whose
 * columns (and cells) are reordered decodes identically. A row missing a
 * column, or carrying a cell of the wrong kind, decodes to that field's zero
 * value rather than throwing.
 */
export function decodeRows(resp: ConversationQueryResponse): SpendRow[] {
  return resp.rows.map((row) => {
    const cells = new Map<string, QueryValue>()
    resp.columns.forEach((column, index) => {
      const cell = row.cells[index]
      if (cell !== undefined) {
        cells.set(column.name, cell)
      }
    })
    return {
      day: strCell(cells, 'day'),
      model: strCell(cells, 'model'),
      provider: strCell(cells, 'provider'),
      turns: numCell(cells, 'turns'),
      inputTokens: numCell(cells, 'input_tokens'),
      outputTokens: numCell(cells, 'output_tokens'),
      cacheReadTokens: numCell(cells, 'cache_read_tokens'),
      cacheCreationTokens: numCell(cells, 'cache_creation_tokens'),
      costUsd: numCell(cells, 'cost_usd'),
      priced: numCell(cells, 'priced') !== 0,
    }
  })
}

/**
 * The stack label for a row. An empty provider is unknown cost attribution, not
 * an empty legend entry, so it is named explicitly.
 */
export function groupLabel(row: SpendRow, key: GroupKey): string {
  const value = key === 'model' ? row.model : row.provider
  return value === '' ? 'unknown' : value
}

/**
 * One stack segment per priced row. An unpriced row's cost is unknown, not
 * zero, so it is excluded from the stacks rather than drawn as a flat bar.
 */
export function byGroup(rows: readonly SpendRow[], key: GroupKey): StackPoint[] {
  const points: StackPoint[] = []
  for (const row of rows) {
    if (!row.priced) {
      continue
    }
    points.push({ day: row.day, key: groupLabel(row, key), costUsd: row.costUsd })
  }
  return points
}

/** Summed cost and whether any row's cost is unknown (so the total is partial). */
export function totals(rows: readonly SpendRow[]): SpendTotals {
  let costUsd = 0
  let anyUnpriced = false
  for (const row of rows) {
    costUsd += row.costUsd
    if (!row.priced) {
      anyUnpriced = true
    }
  }
  return { costUsd, anyUnpriced }
}

/** One legend series: its label, its priced cost over the rows, and whether any of its cost is known. */
export interface LegendEntry {
  key: string
  costUsd: number
  priced: boolean
}

/**
 * Every series in rows, most expensive first (ties by name), so the legend and
 * the color domain share one stable order. A series with only unpriced rows is
 * still listed, with priced=false: its cost is unknown, not zero.
 */
export function legendEntries(rows: readonly SpendRow[], key: GroupKey): LegendEntry[] {
  const byKey = new Map<string, LegendEntry>()
  for (const row of rows) {
    const label = groupLabel(row, key)
    const entry = byKey.get(label) ?? { key: label, costUsd: 0, priced: false }
    if (row.priced) {
      entry.costUsd += row.costUsd
      entry.priced = true
    }
    byKey.set(label, entry)
  }
  return [...byKey.values()].sort((a, b) => b.costUsd - a.costUsd || a.key.localeCompare(b.key))
}

/** The rows whose series is not hidden. */
export function visibleRows(
  rows: readonly SpendRow[],
  key: GroupKey,
  hidden: ReadonlySet<string>,
): SpendRow[] {
  return rows.filter((row) => !hidden.has(groupLabel(row, key)))
}

/** hidden with key flipped. */
export function toggleKey(hidden: ReadonlySet<string>, key: string): Set<string> {
  const next = new Set(hidden)
  if (!next.delete(key)) {
    next.add(key)
  }
  return next
}

/**
 * Shows only key; if key is already the only visible series, shows everything.
 * Names hidden but absent from keys are kept, so a series that drops out of the
 * current range stays hidden when it comes back.
 */
export function soloKey(hidden: ReadonlySet<string>, keys: readonly string[], key: string): Set<string> {
  const others = keys.filter((k) => k !== key)
  const alreadySolo = !hidden.has(key) && others.every((k) => hidden.has(k))
  const next = new Set(hidden)
  if (alreadySolo) {
    for (const k of others) {
      next.delete(k)
    }
    return next
  }
  next.delete(key)
  for (const k of others) {
    next.add(k)
  }
  return next
}
