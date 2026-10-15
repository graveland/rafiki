import { writable } from 'svelte/store'

export type RouteName = 'agents' | 'transcript' | 'search' | 'spend' | 'login-done'

export interface Route {
  name: RouteName
  ids: string[]
}

const MAX_TRANSCRIPT_IDS = 3

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
        .slice(0, MAX_TRANSCRIPT_IDS)
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
