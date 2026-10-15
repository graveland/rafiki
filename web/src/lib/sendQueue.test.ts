import { expect, it, vi } from 'vitest'
import { SendQueue } from './sendQueue.svelte'
import type { ControlClient } from './transcript.svelte'

function fakeControl(send: (input: unknown) => Promise<{ messageId: string }>) {
  return { send } as unknown as ControlClient
}

it('success goes queued with messageId', async () => {
  const send = vi.fn().mockResolvedValue({ messageId: 'm1' })
  const queue = new SendQueue('c', { control: fakeControl(send) })
  await queue.send('hi', 'prompt')
  expect(send).toHaveBeenCalledTimes(1)
  expect(queue.entries).toHaveLength(1)
  expect(queue.entries[0].status).toBe('queued')
  expect(queue.entries[0].messageId).toBe('m1')
})

it('failure goes failed and is not retried', async () => {
  const send = vi.fn().mockRejectedValue(new Error('nope'))
  const queue = new SendQueue('c', { control: fakeControl(send) })
  await queue.send('hi', 'prompt')
  expect(send).toHaveBeenCalledTimes(1)
  expect(queue.entries[0].status).toBe('failed')
  expect(queue.entries[0].error).toBe('nope')
})

it('resend calls send again', async () => {
  const send = vi.fn().mockRejectedValueOnce(new Error('nope')).mockResolvedValue({ messageId: 'm2' })
  const queue = new SendQueue('c', { control: fakeControl(send) })
  await queue.send('hi', 'prompt')
  expect(send).toHaveBeenCalledTimes(1)
  await queue.resend(queue.entries[0].id)
  expect(send).toHaveBeenCalledTimes(2)
  expect(queue.entries[0].status).toBe('queued')
  expect(queue.entries[0].messageId).toBe('m2')
})

it('delivered matches oldest queued FIFO', async () => {
  const send = vi.fn().mockResolvedValue({ messageId: 'm1' })
  const queue = new SendQueue('c', { control: fakeControl(send) })
  await queue.send('first', 'prompt')
  await queue.send('second', 'prompt')

  queue.markDelivered('second')
  expect(queue.entries[0].status).toBe('queued')
  expect(queue.entries[1].status).toBe('queued')

  queue.markDelivered('first')
  expect(queue.entries[0].status).toBe('delivered')
  expect(queue.entries[1].status).toBe('queued')
})

it('an empty abort sends no blocks and reaches queued', async () => {
  const send = vi.fn().mockResolvedValue({ messageId: 'm1' })
  const queue = new SendQueue('c', { control: fakeControl(send) })
  await queue.send('', 'abort')
  expect(send).toHaveBeenCalledTimes(1)
  const call = send.mock.calls[0][0] as { mode: number; blocks: unknown[] }
  expect(call.blocks).toEqual([])
  expect(queue.entries[0].status).toBe('queued')
})
