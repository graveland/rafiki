import { writable } from 'svelte/store'

export const SESSION_CHANNEL = 'rafiki-web-session'

const SECRET_STORAGE_KEY = 'rafiki-web-secret'
const OTHER_TABS_TIMEOUT_MS = 1500

export class LinkUsedError extends Error {
  constructor() {
    super('the launch link has already been used')
    this.name = 'LinkUsedError'
  }
}

export class NoSessionError extends Error {
  constructor() {
    super('no rafiki web session available')
    this.name = 'NoSessionError'
  }
}

type ChannelMessage =
  | { type: 'want-secret' }
  | { type: 'secret'; secret: string }
  | { type: 'hello' }
  | { type: 'here' }

export interface SessionChannel {
  postMessage(data: unknown): void
  close(): void
  onmessage: ((event: MessageEvent) => void) | null
}

export type ChannelFactory = (name: string) => SessionChannel

let channelFactory: ChannelFactory = (name) => new BroadcastChannel(name)

export function setChannelFactory(factory: ChannelFactory): void {
  channelFactory = factory
}

export const otherTabs = writable(0)

let inflight: Promise<string> | null = null

export function getSecret(): Promise<string> {
  if (inflight) {
    return inflight
  }
  const code = fragmentCode()
  if (code !== null) {
    inflight = exchangeCode(code).finally(() => {
      inflight = null
    })
    return inflight
  }
  const stored = sessionStorage.getItem(SECRET_STORAGE_KEY)
  if (stored) {
    return Promise.resolve(stored)
  }
  return askOtherTabs()
}

function fragmentCode(): string | null {
  const hash = window.location.hash
  if (hash.startsWith('#k=')) {
    return hash.slice(3)
  }
  return null
}

async function exchangeCode(code: string): Promise<string> {
  const response = await fetch('/_rafiki/session', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ code }),
  })
  if (response.status === 410) {
    throw new LinkUsedError()
  }
  if (!response.ok) {
    throw new Error(`session exchange failed with status ${response.status}`)
  }
  const body = (await response.json()) as { secret: string }
  sessionStorage.setItem(SECRET_STORAGE_KEY, body.secret)
  stripFragment()
  return body.secret
}

function stripFragment(): void {
  window.history.replaceState(null, '', '#/agents')
}

function askOtherTabs(): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    const channel = channelFactory(SESSION_CHANNEL)
    let settled = false
    const timeout = setTimeout(() => {
      if (settled) {
        return
      }
      settled = true
      channel.close()
      reject(new NoSessionError())
    }, OTHER_TABS_TIMEOUT_MS)
    channel.onmessage = (event: MessageEvent) => {
      if (settled) {
        return
      }
      const message = event.data as ChannelMessage
      if (message && message.type === 'secret') {
        settled = true
        clearTimeout(timeout)
        channel.close()
        sessionStorage.setItem(SECRET_STORAGE_KEY, message.secret)
        resolve(message.secret)
      }
    }
    channel.postMessage({ type: 'want-secret' } satisfies ChannelMessage)
  })
}

export function startSessionResponder(): void {
  const channel = channelFactory(SESSION_CHANNEL)
  channel.onmessage = (event: MessageEvent) => {
    const message = event.data as ChannelMessage
    if (!message || typeof message.type !== 'string') {
      return
    }
    switch (message.type) {
      case 'want-secret': {
        const secret = sessionStorage.getItem(SECRET_STORAGE_KEY)
        if (secret) {
          channel.postMessage({ type: 'secret', secret } satisfies ChannelMessage)
        }
        break
      }
      case 'hello':
        channel.postMessage({ type: 'here' } satisfies ChannelMessage)
        break
      case 'here':
        otherTabs.update((count) => count + 1)
        break
      default:
        break
    }
  }
  channel.postMessage({ type: 'hello' } satisfies ChannelMessage)
}
