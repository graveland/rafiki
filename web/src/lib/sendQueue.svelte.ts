import { create } from '@bufbuild/protobuf'
import type { SendMode } from '../gen/rafiki/v1/control_pb'
import { SendMode as SendModeEnum } from '../gen/rafiki/v1/control_pb'
import type { ControlClient } from './transcript.svelte'
import { ContentBlockSchema, TextBlockSchema } from '../gen/rafiki/v1/event_pb'

export type SendModeName = 'prompt' | 'steer' | 'abort'

export type SendStatus = 'sending' | 'queued' | 'delivered' | 'failed'

export interface SendEntry {
  id: number
  text: string
  mode: SendModeName
  status: SendStatus
  messageId?: string
  error?: string
}

export interface SendQueueDeps {
  control: ControlClient
}

const MODE_ENUM: Record<SendModeName, SendMode> = {
  prompt: SendModeEnum.PROMPT,
  steer: SendModeEnum.STEER,
  abort: SendModeEnum.ABORT,
}

function errorMessage(err: unknown): string {
  if (err instanceof Error) {
    return err.message
  }
  return String(err)
}

/**
 * SendQueue holds the outgoing messages for one child.
 *
 * Send is NOT idempotent: a retry can queue the prompt twice, so a failed send
 * is never retried automatically. resend(id) is the only retry path and it is
 * user-triggered. Failed entries stay in memory for this tab only.
 */
export class SendQueue {
  readonly childId: string
  private readonly deps: SendQueueDeps
  private nextId = 1

  entries = $state<SendEntry[]>([])

  constructor(childId: string, deps: SendQueueDeps) {
    this.childId = childId
    this.deps = deps
  }

  async send(text: string, mode: SendModeName): Promise<void> {
    const entry: SendEntry = {
      id: this.nextId,
      text,
      mode,
      status: 'sending',
    }
    this.nextId += 1
    this.entries = [...this.entries, entry]
    await this.dispatch(entry)
  }

  /** The only retry path; user-triggered, never automatic. */
  async resend(id: number): Promise<void> {
    const entry = this.entries.find((candidate) => candidate.id === id)
    if (!entry || entry.status !== 'failed') {
      return
    }
    await this.dispatch(entry)
  }

  /** Mark the oldest queued entry delivered when its text matches, FIFO. */
  markDelivered(text: string): void {
    const oldest = this.entries.find((entry) => entry.status === 'queued')
    if (oldest && oldest.text === text) {
      oldest.status = 'delivered'
    }
  }

  private async dispatch(entry: SendEntry): Promise<void> {
    entry.status = 'sending'
    entry.error = undefined
    try {
      const resp = await this.deps.control.send({
        childId: this.childId,
        mode: MODE_ENUM[entry.mode],
        blocks: [
          create(ContentBlockSchema, {
            block: { case: 'text', value: create(TextBlockSchema, { text: entry.text }) },
          }),
        ],
      })
      entry.status = 'queued'
      entry.messageId = resp.messageId
    } catch (err) {
      entry.status = 'failed'
      entry.error = errorMessage(err)
    }
  }
}
