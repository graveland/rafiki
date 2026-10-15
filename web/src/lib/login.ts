import { SESSION_CHANNEL, getSecret, type SessionChannel } from './session'
import { setAuthState } from './authState'

export class LoginError extends Error {
  readonly code: string

  constructor(code: string, message: string) {
    super(message)
    this.name = 'LoginError'
    this.code = code
  }
}

export type ChannelFactory = (name: string) => SessionChannel

let channelFactory: ChannelFactory = (name) => new BroadcastChannel(name)

export function setChannelFactory(factory: ChannelFactory): void {
  channelFactory = factory
}

const POPUP_NAME = 'rafiki-login'
const POPUP_FEATURES = 'popup,width=520,height=720'

interface ChannelMessage {
  type: 'login-complete'
}

// The one login flow that may be pending, so a second sign-in click can
// supersede it (rejecting the first promise) instead of orphaning it, and each
// attempt can be told whether it is still the live one.
let loginAttempt = 0
let pendingLogin: { reject: (reason: unknown) => void } | null = null

// One channel per login flow: creating a fresh BroadcastChannel on every click
// (and never closing one whose begin failed) leaks one channel per click.
let loginChannel: SessionChannel | null = null

function acquireChannel(): SessionChannel {
  if (loginChannel === null) {
    loginChannel = channelFactory(SESSION_CHANNEL)
  }
  return loginChannel
}

function releaseChannel(): void {
  loginChannel?.close()
  loginChannel = null
}

export function startLogin(): Promise<void> {
  // Open the popup SYNCHRONOUSLY, inside the click's user gesture: Safari and
  // strict blockers drop the gesture across the awaits below, and a popup opened
  // then is refused. It is navigated to authorize_url once begin resolves.
  const popup = window.open('about:blank', POPUP_NAME, POPUP_FEATURES)
  if (!popup) {
    return Promise.reject(
      new LoginError(
        'popup-blocked',
        'rafiki web: the sign-in popup was blocked; allow popups for this site and try again',
      ),
    )
  }
  // A newer click supersedes the pending flow: reject its promise so it never
  // hangs, and let this attempt own a fresh channel.
  if (pendingLogin) {
    const previous = pendingLogin
    pendingLogin = null
    previous.reject(
      new LoginError('superseded', 'rafiki web: superseded by a newer sign-in attempt'),
    )
  }
  const attempt = (loginAttempt += 1)
  releaseChannel()
  const channel = acquireChannel()

  let rejectLogin: (reason: unknown) => void = () => {}
  const promise = new Promise<void>((resolve, reject) => {
    rejectLogin = reject
    void runLogin(popup, channel).then(
      () => {
        if (loginAttempt === attempt) {
          pendingLogin = null
        }
        resolve()
      },
      (error: unknown) => {
        // Only the live attempt may tear down shared state (the popup window is
        // named, so a superseding attempt shares it).
        if (loginAttempt === attempt) {
          pendingLogin = null
          popup.close()
          releaseChannel()
        }
        reject(error)
      },
    )
  })
  pendingLogin = { reject: rejectLogin }
  return promise
}

async function runLogin(popup: Window, channel: SessionChannel): Promise<void> {
  const secret = await getSecret()
  const response = await fetch('/_rafiki/login/begin', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', 'X-Rafiki-Web': secret },
    body: JSON.stringify({}),
  })
  if (!response.ok) {
    throw await beginError(response)
  }
  const body = (await response.json()) as { authorize_url?: string }
  if (!body.authorize_url) {
    throw new LoginError('internal', 'rafiki web: login begin returned no authorize_url')
  }
  popup.location.href = body.authorize_url
  await awaitLoginComplete(channel)
}

async function beginError(response: Response): Promise<LoginError> {
  let code = 'internal'
  let message = `login begin failed with status ${response.status}`
  try {
    const body = (await response.json()) as { code?: string; message?: string }
    if (typeof body.code === 'string' && body.code !== '') {
      code = body.code
    }
    if (typeof body.message === 'string' && body.message !== '') {
      message = body.message
    }
  } catch {
    // A non-JSON error body leaves the status-derived message in place.
  }
  return new LoginError(code, message)
}

function awaitLoginComplete(channel: SessionChannel): Promise<void> {
  return new Promise<void>((resolve) => {
    channel.onmessage = (event: MessageEvent) => {
      const message = event.data as ChannelMessage
      if (!message || message.type !== 'login-complete') {
        return
      }
      channel.close()
      loginChannel = null
      setAuthState('ok')
      resolve()
    }
  })
}
