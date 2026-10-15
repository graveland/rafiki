import { get } from 'svelte/store'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { authState } from './authState'
import { LoginError, setChannelFactory, startLogin } from './login'
import { SESSION_CHANNEL } from './session'

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

function beginOk(authorizeUrl: string): Response {
  return {
    status: 200,
    ok: true,
    json: () => Promise.resolve({ authorize_url: authorizeUrl }),
  } as unknown as Response
}

function beginError(code: string, message: string): Response {
  return {
    status: 400,
    ok: false,
    json: () => Promise.resolve({ code, message }),
  } as unknown as Response
}

async function signalLoginComplete(): Promise<void> {
  await vi.waitFor(() => expect(FakeChannel.registry.length).toBeGreaterThan(0))
  const popup = new FakeChannel(SESSION_CHANNEL)
  popup.postMessage({ type: 'login-complete' })
}

beforeEach(() => {
  FakeChannel.registry = []
  setChannelFactory((name) => new FakeChannel(name))
  sessionStorage.clear()
  window.history.replaceState(null, '', '/')
  authState.set('needs-login')
})

afterEach(() => {
  vi.unstubAllGlobals()
})

it('begin posts with secret header', async () => {
  sessionStorage.setItem(SECRET_STORAGE_KEY, 'sec')
  const fetchMock = vi.fn().mockResolvedValue(beginOk('http://idp/auth'))
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('open', vi.fn())

  const promise = startLogin()
  await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1))

  const [url, init] = fetchMock.mock.calls[0]
  expect(url).toBe('/_rafiki/login/begin')
  expect(init.method).toBe('POST')
  expect(init.headers['Content-Type']).toBe('application/json')
  expect(init.headers['X-Rafiki-Web']).toBe('sec')
  expect(JSON.parse(init.body)).toEqual({})

  await signalLoginComplete()
  await expect(promise).resolves.toBeUndefined()
})

it('opens popup with authorize_url', async () => {
  sessionStorage.setItem(SECRET_STORAGE_KEY, 'sec')
  const openMock = vi.fn()
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(beginOk('http://idp/auth?a=1')))
  vi.stubGlobal('open', openMock)

  const promise = startLogin()
  await vi.waitFor(() => expect(openMock).toHaveBeenCalledTimes(1))
  expect(openMock).toHaveBeenCalledWith(
    'http://idp/auth?a=1',
    'rafiki-login',
    'popup,width=520,height=720',
  )

  await signalLoginComplete()
  await expect(promise).resolves.toBeUndefined()
})

it('resolves on login-complete broadcast', async () => {
  sessionStorage.setItem(SECRET_STORAGE_KEY, 'sec')
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(beginOk('http://idp/auth')))
  vi.stubGlobal('open', vi.fn())

  const promise = startLogin()
  await signalLoginComplete()

  await expect(promise).resolves.toBeUndefined()
  expect(get(authState)).toBe('ok')
})

it('begin error surfaces server message', async () => {
  sessionStorage.setItem(SECRET_STORAGE_KEY, 'sec')
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue(beginError('failed_precondition', 'OIDC is not configured')),
  )

  const error = await startLogin().catch((reason: unknown) => reason)
  expect(error).toBeInstanceOf(LoginError)
  expect((error as LoginError).code).toBe('failed_precondition')
  expect((error as LoginError).message).toBe('OIDC is not configured')
})
