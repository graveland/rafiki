import { expect, it } from 'vitest'
import { ctxLabel, relTime, tokens, usd } from './format'

it('usd renders unknown and unpriced as dash', () => {
  expect(usd(undefined)).toBe('-')
  expect(usd(0, false)).toBe('-')
  expect(usd(1.5, false)).toBe('-')
})

it('usd renders zero as $0.00', () => {
  expect(usd(0)).toBe('$0.00')
})

it('usd renders two decimals at or above a cent', () => {
  expect(usd(1.5)).toBe('$1.50')
  expect(usd(0.01)).toBe('$0.01')
})

it('usd renders four decimals below a cent', () => {
  expect(usd(0.005)).toBe('$0.0050')
  expect(usd(0.0001)).toBe('$0.0001')
})

it('tokens renders unknown as dash', () => {
  expect(tokens(undefined)).toBe('-')
})

it('tokens renders sub-thousand exactly', () => {
  expect(tokens(500)).toBe('500')
})

it('tokens renders thousands as k', () => {
  expect(tokens(1200)).toBe('1.2k')
})

it('tokens renders millions as M', () => {
  expect(tokens(3_400_000)).toBe('3.4M')
})

it('relTime renders unknown as dash', () => {
  expect(relTime(undefined)).toBe('-')
})

it('relTime renders recent as just now', () => {
  const now = new Date('2026-01-01T00:00:00Z')
  expect(relTime(new Date('2026-01-01T00:00:30Z'), now)).toBe('just now')
})

it('relTime renders minutes', () => {
  const now = new Date('2026-01-01T00:05:00Z')
  expect(relTime(new Date('2026-01-01T00:00:00Z'), now)).toBe('5m ago')
})

it('relTime renders hours', () => {
  const now = new Date('2026-01-01T03:00:00Z')
  expect(relTime(new Date('2026-01-01T00:00:00Z'), now)).toBe('3h ago')
})

it('relTime renders days', () => {
  const now = new Date('2026-01-03T00:00:00Z')
  expect(relTime(new Date('2026-01-01T00:00:00Z'), now)).toBe('2d ago')
})

it('ctxLabel treats unknown and zero as dash', () => {
  expect(ctxLabel(undefined)).toBe('-')
  expect(ctxLabel(0)).toBe('-')
})

it('ctxLabel renders thousands as K', () => {
  expect(ctxLabel(128_000)).toBe('128K ctx')
})

it('ctxLabel renders millions as M', () => {
  expect(ctxLabel(1_000_000)).toBe('1M ctx')
})
