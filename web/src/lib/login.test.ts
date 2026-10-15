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

interface FakePopup {
  location: { href: string }
  close: () => void
}

function fakePopup(): FakePopup {
  return { location: { href: '' }, close: vi.fn() }
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
  // Wait until the LIVE login flow has registered its handler: the popup's
  // broadcast is only delivered to a channel that is open and listening.
  await vi.waitFor(() => {
    const target = FakeChannel.registry.find(
      (c) => c.name === SESSION_CHANNEL && !c.closed && c.onmessage,
    )
    expect(target).toBeDefined()
  })
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
  vi.stubGlobal('open', vi.fn(() => fakePopup()))

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

it('opens the popup synchronously and then navigates it', async () => {
  sessionStorage.setItem(SECRET_STORAGE_KEY, 'sec')
  const popup = fakePopup()
  const openMock = vi.fn(() => popup)
  vi.stubGlobal('open', openMock)
  const fetchMock = vi.fn().mockResolvedValue(beginOk('http://idp/auth?a=1'))
  vi.stubGlobal('fetch', fetchMock)

  const promise = startLogin()
  // Synchronous: open runs before the first await, preserving the click's user
  // gesture. The popup is navigated only once begin resolves.
  expect(openMock).toHaveBeenCalledWith('about:blank', 'rafiki-login', 'popup,width=520,height=720')

  await vi.waitFor(() => expect(popup.location.href).toBe('http://idp/auth?a=1'))
  await signalLoginComplete()
  await expect(promise).resolves.toBeUndefined()
})

it('a blocked popup rejects and settles the promise', async () => {
  sessionStorage.setItem(SECRET_STORAGE_KEY, 'sec')
  vi.stubGlobal('open', vi.fn(() => null))
  const fetchMock = vi.fn()
  vi.stubGlobal('fetch', fetchMock)

  const error = await startLogin().catch((reason: unknown) => reason)
  expect(error).toBeInstanceOf(LoginError)
  expect((error as LoginError).code).toBe('popup-blocked')
  expect(fetchMock).not.toHaveBeenCalled()
})

it('resolves on login-complete broadcast', async () => {
  sessionStorage.setItem(SECRET_STORAGE_KEY, 'sec')
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(beginOk('http://idp/auth')))
  vi.stubGlobal('open', vi.fn(() => fakePopup()))

  const promise = startLogin()
  await signalLoginComplete()

  await expect(promise).resolves.toBeUndefined()
  expect(get(authState)).toBe('ok')
})

it('begin error surfaces server message', async () => {
  sessionStorage.setItem(SECRET_STORAGE_KEY, 'sec')
  vi.stubGlobal('open', vi.fn(() => fakePopup()))
  vi.stubGlobal(
    'fetch',
    vi.fn().mockResolvedValue(beginError('failed_precondition', 'OIDC is not configured')),
  )

  const error = await startLogin().catch((reason: unknown) => reason)
  expect(error).toBeInstanceOf(LoginError)
  expect((error as LoginError).code).toBe('failed_precondition')
  expect((error as LoginError).message).toBe('OIDC is not configured')
})

it('closes its channel when begin fails', async () => {
  sessionStorage.setItem(SECRET_STORAGE_KEY, 'sec')
  vi.stubGlobal('open', vi.fn(() => fakePopup()))
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(beginError('failed_precondition', 'nope')))

  await expect(startLogin()).rejects.toBeInstanceOf(LoginError)
  expect(FakeChannel.registry).toHaveLength(1)
  expect(FakeChannel.registry[0].closed).toBe(true)
})

it('a second sign-in supersedes the first pending promise', async () => {
  sessionStorage.setItem(SECRET_STORAGE_KEY, 'sec')
  vi.stubGlobal('open', vi.fn(() => fakePopup()))
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(beginOk('http://idp/auth')))

  const first = startLogin()
  const second = startLogin()

  const firstError = await first.catch((reason: unknown) => reason)
  expect(firstError).toBeInstanceOf(LoginError)
  expect((firstError as LoginError).code).toBe('superseded')

  await signalLoginComplete()
  await expect(second).resolves.toBeUndefined()
})
