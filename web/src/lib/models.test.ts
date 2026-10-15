import { create, type MessageInitShape } from '@bufbuild/protobuf'
import { expect, it } from 'vitest'
import { ModelRowSchema, type ModelRow } from '../gen/rafiki/v1/control_pb'
import { costHint, costKey, filtered, perMtok, processed } from './models'

function row(init: MessageInitShape<typeof ModelRowSchema> & { name: string }): ModelRow {
  return create(ModelRowSchema, init)
}

it('sorts by cost with unpriced models last', () => {
  const cheap = row({ name: 'cheap', promptUsd: 0.000001, completionUsd: 0.000002 })
  const pricey = row({ name: 'pricey', promptUsd: 0.00001, completionUsd: 0.00002 })
  const unpriced = row({ name: 'unpriced' })

  expect(processed([unpriced, pricey, cheap], 'cost').map((m) => m.name)).toEqual([
    'cheap',
    'pricey',
    'unpriced',
  ])
})

it('sorts by context descending, unknown last', () => {
  const small = row({ name: 'small', contextWindow: 8000 })
  const large = row({ name: 'large', contextWindow: 1_000_000 })
  const unknown = row({ name: 'unknown' })
  const zero = row({ name: 'zero', contextWindow: 0 })

  expect(processed([unknown, small, large, zero], 'context').map((m) => m.name)).toEqual([
    'large',
    'small',
    'unknown',
    'zero',
  ])
})

it('sorts by name alphabetically', () => {
  const b = row({ name: 'anthropic/claude' })
  const a = row({ name: 'openai/gpt' })
  expect(processed([b, a]).map((m) => m.name)).toEqual(['anthropic/claude', 'openai/gpt'])
})

it('filters by case-insensitive name substring', () => {
  const claude = row({ name: 'anthropic/claude-3' })
  const gpt = row({ name: 'openai/gpt-4o' })
  expect(filtered([claude, gpt], 'GPT').map((m) => m.name)).toEqual(['openai/gpt-4o'])
  expect(filtered([claude, gpt], '  ').map((m) => m.name)).toEqual([
    'anthropic/claude-3',
    'openai/gpt-4o',
  ])
})

it('costKey treats a fully unpriced row as infinite', () => {
  expect(costKey(row({ name: 'free', promptUsd: 0, completionUsd: 0 }))).toBe(0)
  expect(costKey(row({ name: 'unpriced' }))).toBe(Infinity)
})

it('perMtok scales per-token price to per-million', () => {
  expect(perMtok(0.000001)).toBe(1)
  expect(perMtok(0.00000015)).toBe(0.15)
  expect(perMtok(undefined)).toBeNull()
})

it('costHint is null when nothing is priced and summarizes otherwise', () => {
  expect(costHint(row({ name: 'unpriced' }))).toBeNull()
  expect(costHint(row({ name: 'free', promptUsd: 0, completionUsd: 0 }))).toBe('free')
  expect(costHint(row({ name: 'paid', promptUsd: 0.000001, completionUsd: 0.000002 }))).toBe(
    '$1/$2 /Mtok',
  )
})
