import { writable } from 'svelte/store'

export type RouteName = 'agents' | 'transcript' | 'search' | 'spend' | 'login-done'

export interface Route {
  name: RouteName
  ids: string[]
}

/** The at-most-3-live-panes budget the transcript renders; see transcriptPanes. */
export const MAX_TRANSCRIPT_PANES = 3

/**
 * The id charset allowed in a route id. Ids come off the URL fragment — typed at
 * the pane "+" prompt or navigated to from a search hit — and become each-block
 * keys in SplitPanes, so a stray slash/space/control char must not reach it.
 * Child ids are `c_<ULID>` (Crockford base32, uppercase alnum) and search may
 * hand through a dashed uuid.
 */
const ID_PATTERN = /^[A-Za-z0-9_-]+$/

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
      // Drop repeats (a duplicate each-key throws each_key_duplicate in dev and
      // is undefined behaviour in prod — the "+" prompt accepts the id already
      // in the route) and any garbage the typed segment carries.
      const seen = new Set<string>()
      const ids: string[] = []
      for (const candidate of (segments[1] ?? '').split(',')) {
        const id = candidate.trim()
        if (id === '' || !ID_PATTERN.test(id) || seen.has(id)) {
          continue
        }
        seen.add(id)
        ids.push(id)
      }
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
