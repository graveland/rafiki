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

export async function startLogin(): Promise<void> {
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
  return await awaitLoginComplete(body.authorize_url)
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

function awaitLoginComplete(authorizeUrl: string): Promise<void> {
  const channel = channelFactory(SESSION_CHANNEL)
  const complete = new Promise<void>((resolve) => {
    channel.onmessage = (event: MessageEvent) => {
      const message = event.data as ChannelMessage
      if (!message || message.type !== 'login-complete') {
        return
      }
      channel.close()
      setAuthState('ok')
      resolve()
    }
  })
  window.open(authorizeUrl, POPUP_NAME, POPUP_FEATURES)
  return complete
}
