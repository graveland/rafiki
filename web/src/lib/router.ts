import { writable } from 'svelte/store'

export type RouteName = 'agents' | 'transcript' | 'search' | 'spend' | 'login-done'

export interface Route {
  name: RouteName
  ids: string[]
}

/** The at-most-3-live-panes budget the transcript renders; see transcriptPanes. */
export const MAX_TRANSCRIPT_PANES = 3

/**
 * The router keeps the FULL id list; the transcript slices it for rendering.
 * Separating the two is what makes the "too many panes" banner reachable —
 * dropping the extras in the router would leave `ids.length > 3` never true.
 */
export function transcriptPanes(ids: string[]): { shown: string[]; overflow: boolean } {
  return {
    shown: ids.slice(0, MAX_TRANSCRIPT_PANES),
    overflow: ids.length > MAX_TRANSCRIPT_PANES,
  }
}

export function parseRoute(hash: string): Route {
  const raw = hash.replace(/^#/, '')
  if (raw.startsWith('k=')) {
    return { name: 'agents', ids: [] }
  }
  const segments = raw.split('/').filter((segment) => segment.length > 0)
  if (segments.length === 0) {
    return { name: 'agents', ids: [] }
  }
  switch (segments[0]) {
    case 'c': {
      const ids = (segments[1] ?? '')
        .split(',')
        .map((id) => id.trim())
        .filter((id) => id.length > 0)
      if (ids.length === 0) {
        return { name: 'agents', ids: [] }
      }
      return { name: 'transcript', ids }
    }
    case 'search':
      return { name: 'search', ids: [] }
    case 'spend':
      return { name: 'spend', ids: [] }
    case 'login-done':
      return { name: 'login-done', ids: [] }
    case 'agents':
    default:
      return { name: 'agents', ids: [] }
  }
}

export const route = writable<Route>(parseRoute(window.location.hash))

window.addEventListener('hashchange', () => {
  route.set(parseRoute(window.location.hash))
})

export function navigate(hash: string): void {
  window.location.hash = hash
}
