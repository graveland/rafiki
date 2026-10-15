import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import {
  getSecret,
  LinkUsedError,
  NoSessionError,
  SESSION_CHANNEL,
  setChannelFactory,
} from './session'

const SECRET_STORAGE_KEY = 'rafiki-web-secret'

class FakeChannel {
  static registry: FakeChannel[] = []
  name: string
  closed = false
  onmessage: ((event: MessageEvent) => void) | null = null

  constructor(name: string) {
    this.name = name
    FakeChannel.registry.push(this)
  }

  postMessage(data: unknown): void {
    if (this.closed) {
      return
    }
    for (const target of FakeChannel.registry) {
      if (target !== this && target.name === this.name && !target.closed && target.onmessage) {
        target.onmessage({ data } as MessageEvent)
      }
    }
  }

  close(): void {
    this.closed = true
  }
}

function okResponse(secret: string): Response {
  return {
    status: 200,
    ok: true,
    json: () => Promise.resolve({ secret }),
  } as unknown as Response
}

function usedResponse(): Response {
  return { status: 410, ok: false } as unknown as Response
}

beforeEach(() => {
  FakeChannel.registry = []
  setChannelFactory((name) => new FakeChannel(name))
  sessionStorage.clear()
  window.history.replaceState(null, '', '/')
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

it('uses stored secret', async () => {
  sessionStorage.setItem(SECRET_STORAGE_KEY, 'stored')
  const fetchMock = vi.fn()
  vi.stubGlobal('fetch', fetchMock)
  await expect(getSecret()).resolves.toBe('stored')
  expect(fetchMock).not.toHaveBeenCalled()
})

it('exchanges fragment code once and strips it', async () => {
  window.location.hash = '#k=abc'
  const fetchMock = vi.fn().mockResolvedValue(okResponse('s1'))
  vi.stubGlobal('fetch', fetchMock)

  await expect(getSecret()).resolves.toBe('s1')

  expect(fetchMock).toHaveBeenCalledTimes(1)
  const [url, init] = fetchMock.mock.calls[0]
  expect(url).toBe('/_rafiki/session')
  expect(init.method).toBe('POST')
  expect(init.headers['Content-Type']).toBe('application/json')
  expect(JSON.parse(init.body)).toEqual({ code: 'abc' })
  expect(window.location.hash).toBe('#/agents')
  expect(sessionStorage.getItem(SECRET_STORAGE_KEY)).toBe('s1')
})

it('fresh link overrides stored secret', async () => {
  sessionStorage.setItem(SECRET_STORAGE_KEY, 'stale')
  window.location.hash = '#k=fresh'
  const fetchMock = vi.fn().mockResolvedValue(okResponse('new'))
  vi.stubGlobal('fetch', fetchMock)

  await expect(getSecret()).resolves.toBe('new')
  expect(fetchMock).toHaveBeenCalledTimes(1)
  expect(sessionStorage.getItem(SECRET_STORAGE_KEY)).toBe('new')
})

it('concurrent first calls exchange once', async () => {
  window.location.hash = '#k=abc'
  const fetchMock = vi.fn(
    () => new Promise<Response>((resolve) => setTimeout(() => resolve(okResponse('s1')), 10)),
  )
  vi.stubGlobal('fetch', fetchMock)

  const [first, second] = await Promise.all([getSecret(), getSecret()])
  expect(first).toBe('s1')
  expect(second).toBe('s1')
  expect(fetchMock).toHaveBeenCalledTimes(1)
})

it('410 throws LinkUsedError', async () => {
  window.location.hash = '#k=used'
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(usedResponse()))

  await expect(getSecret()).rejects.toBeInstanceOf(LinkUsedError)
})

it('asks other tabs when no code', async () => {
  const responder = new FakeChannel(SESSION_CHANNEL)
  responder.onmessage = (event) => {
    const message = event.data as { type: string }
    if (message.type === 'want-secret') {
      responder.postMessage({ type: 'secret', secret: 'from-tab' })
    }
  }

  await expect(getSecret()).resolves.toBe('from-tab')
  expect(sessionStorage.getItem(SECRET_STORAGE_KEY)).toBe('from-tab')
})

it('times out to NoSessionError', async () => {
  vi.useFakeTimers()
  const promise = getSecret()
  const assertion = expect(promise).rejects.toBeInstanceOf(NoSessionError)
  await vi.advanceTimersByTimeAsync(1500)
  await assertion
})
