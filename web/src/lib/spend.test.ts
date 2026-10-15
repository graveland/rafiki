import { create } from '@bufbuild/protobuf'
import { expect, it } from 'vitest'
import {
  ConversationQueryResponseSchema,
  QueryValueSchema,
  type ConversationQueryResponse,
  type QueryValue,
} from '../gen/rafiki/v1/control_pb'
import {
  byGroup,
  decodeRows,
  legendEntries,
  soloKey,
  toggleKey,
  totals,
  visibleRows,
} from './spend'

type Kind = 'str' | 'int' | 'float'

const KINDS: Record<string, Kind> = {
  day: 'str',
  model: 'str',
  provider: 'str',
  turns: 'int',
  input_tokens: 'int',
  output_tokens: 'int',
  cache_read_tokens: 'int',
  cache_creation_tokens: 'int',
  cost_usd: 'float',
  priced: 'int',
}

function cell(kind: Kind, value: string | number): QueryValue {
  if (kind === 'str') {
    return create(QueryValueSchema, { v: { case: 'strValue', value: String(value) } })
  }
  if (kind === 'int') {
    return create(QueryValueSchema, { v: { case: 'intValue', value: BigInt(value) } })
  }
  return create(QueryValueSchema, { v: { case: 'floatValue', value: Number(value) } })
}

/** Builds a response whose cells follow `cols`' order, keyed by column name. */
function response(
  cols: string[],
  rows: Record<string, string | number>[],
): ConversationQueryResponse {
  return create(ConversationQueryResponseSchema, {
    columns: cols.map((name) => ({ name, kind: KINDS[name] ?? 'str', format: '' })),
    rows: rows.map((row) => ({
      cells: cols.map((name) => cell(KINDS[name] ?? 'str', row[name] ?? 0)),
    })),
  })
}

it('decodes by column name not position', () => {
  const inOrder = ['day', 'model', 'provider', 'turns', 'input_tokens', 'output_tokens', 'cost_usd', 'priced']
  const shuffled = ['priced', 'cost_usd', 'model', 'output_tokens', 'provider', 'turns', 'day', 'input_tokens']
  const row = {
    day: '2026-02-03',
    model: 'anthropic/claude',
    provider: 'anthropic',
    turns: 7,
    input_tokens: 100,
    output_tokens: 40,
    cost_usd: 1.25,
    priced: 1,
  }

  const straight = decodeRows(response(inOrder, [row]))[0]
  const scrambled = decodeRows(response(shuffled, [row]))[0]

  expect(scrambled).toEqual(straight)
  expect(scrambled.day).toBe('2026-02-03')
  expect(scrambled.model).toBe('anthropic/claude')
  expect(scrambled.costUsd).toBe(1.25)
  expect(scrambled.priced).toBe(true)
})

it('unpriced rows flag anyUnpriced and are excluded from cost stacks', () => {
  const rows = decodeRows(
    response(
      ['day', 'model', 'provider', 'cost_usd', 'priced'],
      [
        { day: '2026-02-03', model: 'priced', provider: 'a', cost_usd: 2, priced: 1 },
        { day: '2026-02-03', model: 'free?', provider: 'b', cost_usd: 0, priced: 0 },
      ],
    ),
  )

  expect(rows.map((r) => r.priced)).toEqual([true, false])
  expect(totals(rows)).toEqual({ costUsd: 2, anyUnpriced: true })

  const stacks = byGroup(rows, 'model')
  expect(stacks).toEqual([{ day: '2026-02-03', key: 'priced', costUsd: 2 }])
})

it('groups by provider with empty as unknown', () => {
  const rows = decodeRows(
    response(
      ['day', 'model', 'provider', 'cost_usd', 'priced'],
      [
        { day: '2026-02-03', model: 'x', provider: '', cost_usd: 1, priced: 1 },
        { day: '2026-02-03', model: 'y', provider: 'openrouter', cost_usd: 3, priced: 1 },
      ],
    ),
  )

  expect(byGroup(rows, 'provider')).toEqual([
    { day: '2026-02-03', key: 'unknown', costUsd: 1 },
    { day: '2026-02-03', key: 'openrouter', costUsd: 3 },
  ])

  expect(totals(rows)).toEqual({ costUsd: 4, anyUnpriced: false })
})

const COLS = ['day', 'model', 'provider', 'cost_usd', 'priced']

function sample() {
  return decodeRows(
    response(COLS, [
      { day: '2026-02-03', model: 'cheap', provider: 'a', cost_usd: 1, priced: 1 },
      { day: '2026-02-04', model: 'cheap', provider: '', cost_usd: 1, priced: 1 },
      { day: '2026-02-03', model: 'dear', provider: 'b', cost_usd: 5, priced: 1 },
      { day: '2026-02-03', model: 'mystery', provider: 'b', cost_usd: 0, priced: 0 },
    ]),
  )
}

it('legend entries sort by cost and keep unpriced series as unknown', () => {
  expect(legendEntries(sample(), 'model')).toEqual([
    { key: 'dear', costUsd: 5, priced: true },
    { key: 'cheap', costUsd: 2, priced: true },
    { key: 'mystery', costUsd: 0, priced: false },
  ])
  expect(legendEntries(sample(), 'provider').map((e) => e.key)).toEqual(['b', 'a', 'unknown'])
})

it('hidden series are dropped from stacks and totals', () => {
  const rows = visibleRows(sample(), 'model', new Set(['dear']))
  expect(byGroup(rows, 'model').map((p) => p.key)).toEqual(['cheap', 'cheap'])
  expect(totals(rows)).toEqual({ costUsd: 2, anyUnpriced: true })
})

it('hiding unknown provider drops empty-provider rows', () => {
  const rows = visibleRows(sample(), 'provider', new Set(['unknown']))
  expect(rows.some((r) => r.provider === '')).toBe(false)
  expect(rows).toHaveLength(3)
})

it('a hidden name absent from the rows is ignored', () => {
  expect(visibleRows(sample(), 'model', new Set(['gone']))).toHaveLength(4)
})

it('toggleKey flips one series', () => {
  expect([...toggleKey(new Set(), 'a')]).toEqual(['a'])
  expect([...toggleKey(new Set(['a', 'b']), 'a')]).toEqual(['b'])
})

it('soloKey shows only the series, and again shows all', () => {
  const keys = ['a', 'b', 'c']
  const solo = soloKey(new Set(['a', 'gone']), keys, 'a')
  expect([...solo].sort()).toEqual(['b', 'c', 'gone'])
  expect([...soloKey(solo, keys, 'a')]).toEqual(['gone'])
})
