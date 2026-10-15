import { create } from '@bufbuild/protobuf'
import { expect, it } from 'vitest'
import {
  ConversationQueryResponseSchema,
  QueryValueSchema,
  type ConversationQueryResponse,
  type QueryValue,
} from '../gen/rafiki/v1/control_pb'
import { byGroup, decodeRows, totals } from './spend'

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
