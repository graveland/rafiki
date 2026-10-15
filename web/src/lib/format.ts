export function usd(v: number | undefined, priced = true): string {
  if (v === undefined || !priced) {
    return '-'
  }
  if (v === 0) {
    return '$0.00'
  }
  if (Math.abs(v) < 0.01) {
    return `$${v.toFixed(4)}`
  }
  return `$${v.toFixed(2)}`
}

export function tokens(n?: bigint | number): string {
  if (n === undefined || n === null) {
    return '-'
  }
  const v = typeof n === 'bigint' ? Number(n) : n
  if (v >= 1_000_000) {
    return `${(v / 1_000_000).toFixed(1)}M`
  }
  if (v >= 1000) {
    return `${(v / 1000).toFixed(1)}k`
  }
  return `${v}`
}

export function relTime(d?: Date, now: Date = new Date()): string {
  if (d === undefined) {
    return '-'
  }
  const seconds = Math.floor((now.getTime() - d.getTime()) / 1000)
  if (seconds < 60) {
    return 'just now'
  }
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) {
    return `${minutes}m ago`
  }
  const hours = Math.floor(minutes / 60)
  if (hours < 24) {
    return `${hours}h ago`
  }
  return `${Math.floor(hours / 24)}d ago`
}

export function ctxLabel(n?: number): string {
  if (n === undefined || n === null || n === 0) {
    return '-'
  }
  if (n >= 1_000_000) {
    return `${Math.round(n / 1_000_000)}M ctx`
  }
  if (n >= 1000) {
    return `${Math.round(n / 1000)}K ctx`
  }
  return `${n} ctx`
}
