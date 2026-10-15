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

// One channel per login flow, reused across the awaits below and closed when the
// flow settles: creating a fresh BroadcastChannel on every sign-in click (and
// never closing one whose begin failed) leaks one channel per click.
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

export async function startLogin(): Promise<void> {
  // Open the popup SYNCHRONOUSLY, inside the click's user gesture: Safari and
  // strict blockers drop the gesture across the awaits below, and a popup opened
  // then is refused. It is navigated to authorize_url once begin resolves.
  const popup = window.open('about:blank', POPUP_NAME, POPUP_FEATURES)
  if (!popup) {
    throw new LoginError(
      'popup-blocked',
      'rafiki web: the sign-in popup was blocked; allow popups for this site and try again',
    )
  }
  const channel = acquireChannel()
  try {
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
    return await awaitLoginComplete(channel)
  } catch (error) {
    popup.close()
    releaseChannel()
    throw error
  }
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
