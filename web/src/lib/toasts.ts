import { writable } from 'svelte/store'

export interface Toast {
  id: number
  kind: 'info' | 'error'
  text: string
}

export const AUTO_DISMISS_MS = 6000

export const toasts = writable<Toast[]>([])

let nextId = 1

export function pushToast(toast: { kind: 'info' | 'error'; text: string }): number {
  const id = nextId++
  toasts.update((list) => [...list, { id, kind: toast.kind, text: toast.text }])
  setTimeout(() => dismissToast(id), AUTO_DISMISS_MS)
  return id
}

export function dismissToast(id: number): void {
  toasts.update((list) => list.filter((toast) => toast.id !== id))
}
