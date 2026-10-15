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
