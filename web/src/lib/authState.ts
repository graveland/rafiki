import { writable } from 'svelte/store'

export type AuthState = 'ok' | 'needs-login' | 'gate-refused' | 'link-used' | 'no-session'

export const authState = writable<AuthState>('ok')

export function setAuthState(value: AuthState): void {
  authState.set(value)
}
