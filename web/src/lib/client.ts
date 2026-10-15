import { Code, ConnectError, createClient } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'
import type { Interceptor } from '@connectrpc/connect'
import { Control } from '../gen/rafiki/v1/control_pb'
import { setAuthState } from './authState'
import { getSecret, GateRefusedError, LinkUsedError, NoSessionError } from './session'

export const authInterceptor: Interceptor = (next) => async (req) => {
  let secret: string
  try {
    secret = await getSecret()
  } catch (error) {
    if (error instanceof LinkUsedError) {
      setAuthState('link-used')
    } else if (error instanceof GateRefusedError) {
      setAuthState('gate-refused')
    } else if (error instanceof NoSessionError) {
      setAuthState('no-session')
    }
    throw error
  }
  req.header.set('X-Rafiki-Web', secret)
  try {
    return await next(req)
  } catch (error) {
    if (error instanceof ConnectError) {
      if (error.metadata.get('rafiki-web-gate') === 'refused') {
        setAuthState('gate-refused')
      } else if (error.code === Code.Unauthenticated) {
        setAuthState('needs-login')
      }
    }
    throw error
  }
}

export const transport = createConnectTransport({
  baseUrl: window.location.origin,
  useBinaryFormat: !window.location.search.includes('wire=json'),
  interceptors: [authInterceptor],
})

export const control = createClient(Control, transport)
