import { create } from '@bufbuild/protobuf'
import { Code, ConnectError } from '@connectrpc/connect'
import { expect, it, vi } from 'vitest'
import { ChildSummarySchema } from '../gen/rafiki/v1/control_pb'
import type { Event } from '../gen/rafiki/v1/event_pb'
import {
  AssistantMessageSchema,
  ContentBlockDeltaSchema,
  ContentBlockSchema,
  EventSchema,
  TextBlockSchema,
  ToolResultBlockSchema,
  UserMessageSchema,
} from '../gen/rafiki/v1/event_pb'
import { parseRoute, transcriptPanes } from './router'
import { Conversation, type ControlClient } from './transcript.svelte'

function textBlock(text: string) {
  return create(ContentBlockSchema, {
    block: { case: 'text', value: create(TextBlockSchema, { text }) },
  })
}

function userEvent(childId: string, text: string, ordinal?: number): Event {
  return create(EventSchema, {
    childId,
    ordinal,
    payload: {
      case: 'userMessage',
      value: create(UserMessageSchema, { content: [textBlock(text)] }),
    },
  })
}

function assistantEvent(childId: string, text: string, ordinal?: number): Event {
  return create(EventSchema, {
    childId,
    ordinal,
    payload: {
      case: 'assistantMessage',
      value: create(AssistantMessageSchema, { content: [textBlock(text)] }),
    },
  })
}

function deltaEvent(childId: string, text: string): Event {
  return create(EventSchema, {
    childId,
    payload: {
      case: 'contentBlockDelta',
      value: create(ContentBlockDeltaSchema, { delta: { case: 'text', value: text } }),
    },
  })
}

function idle(signal?: AbortSignal): AsyncIterable<Event> {
  return {
    [Symbol.asyncIterator]() {
      return {
        next: () =>
          new Promise<IteratorResult<Event>>((resolve) => {
            signal?.addEventListener('abort', () => resolve({ done: true, value: undefined }), {
              once: true,
            })
          }),
      }
    },
  }
}

function replay(events: Event[], signal?: AbortSignal): AsyncIterable<Event> {
  let index = 0
  return {
    [Symbol.asyncIterator]() {
      return {
        next: () => {
          if (index < events.length) {
            const value = events[index]
            index += 1
            return Promise.resolve({ done: false as const, value })
          }
          return idle(signal)[Symbol.asyncIterator]().next()
        },
      }
    },
  }
}

/** A stream that ENDS after its events, so the pump reconnects. */
function finite(events: Event[]): AsyncIterable<Event> {
  let index = 0
  return {
    [Symbol.asyncIterator]() {
      return {
        next: () =>
          index < events.length
            ? Promise.resolve({ done: false as const, value: events[index++] })
            : Promise.resolve({ done: true as const, value: undefined }),
      }
    },
  }
}

interface FakeOptions {
  latest?: number
  history?: Event[]
  historyError?: unknown
  childError?: unknown
  stream: (input: unknown, options?: { signal?: AbortSignal }) => AsyncIterable<Event>
}

function fakeControl(options: FakeOptions) {
  const order: string[] = []
  const streamRequests: unknown[] = []
  const control = {
    getChild: (input: { childId: string }) => {
      order.push('getChild')
      void input
      if (options.childError) {
        return Promise.reject(options.childError)
      }
      return Promise.resolve({
        child: create(ChildSummarySchema, {
          childId: 'c',
          latestOrdinal: options.latest,
        }),
      })
    },
    getHistory: (input: { childId: string }) => {
      order.push('getHistory')
      void input
      if (options.historyError) {
        return Promise.reject(options.historyError)
      }
      return Promise.resolve({ events: options.history ?? [] })
    },
    streamEvents: (input: unknown, streamOptions?: { signal?: AbortSignal }) => {
      order.push('streamEvents')
      streamRequests.push(input)
      return options.stream(input, streamOptions)
    },
  } as unknown as ControlClient
  return { control, order, streamRequests }
}

function cursorOf(request: unknown): number {
  const { cursor } = request as { cursor?: { ordinals?: Record<string, number> } }
  return cursor?.ordinals?.c ?? -2
}

async function flush(): Promise<void> {
  for (let i = 0; i < 25; i += 1) {
    await Promise.resolve()
  }
}

it('latest read before history', async () => {
  const { control, order } = fakeControl({ latest: 4, stream: (_i, o) => idle(o?.signal) })
  const conversation = new Conversation('c', { control })
  await conversation.open()
  expect(order).toEqual(['getChild', 'getHistory', 'streamEvents'])
  conversation.dispose()
})

it('fresh child streams from -1', async () => {
  const { control, streamRequests } = fakeControl({ stream: (_i, o) => idle(o?.signal) })
  const conversation = new Conversation('c', { control })
  await conversation.open()
  expect(cursorOf(streamRequests[0])).toBe(-1)
  expect(conversation.blocks).toHaveLength(0)
  conversation.dispose()
})

it('history error falls back to -1 with no history', async () => {
  const { control, streamRequests } = fakeControl({
    latest: 9,
    historyError: new Error('boom'),
    stream: (_i, o) => idle(o?.signal),
  })
  const conversation = new Conversation('c', { control })
  await conversation.open()
  expect(cursorOf(streamRequests[0])).toBe(-1)
  expect(conversation.blocks).toHaveLength(0)
  expect(conversation.error).toBeUndefined()
  conversation.dispose()
})

it('delta then assistant_message supersedes', async () => {
  const events = [deltaEvent('c', 'hel'), assistantEvent('c', 'hello', 0)]
  const { control } = fakeControl({ stream: (_i, o) => replay(events, o?.signal) })
  const conversation = new Conversation('c', { control })
  await conversation.open()
  await flush()
  expect(conversation.blocks).toHaveLength(1)
  expect(conversation.blocks[0].kind).toBe('assistant')
  expect(conversation.blocks[0].text).toBe('hello')
  expect(conversation.blocks[0].inFlight).toBe(false)
  conversation.dispose()
})

it('an input_json delta opens no in-flight block', async () => {
  const inputDelta = create(EventSchema, {
    childId: 'c',
    payload: {
      case: 'contentBlockDelta',
      value: create(ContentBlockDeltaSchema, {
        delta: { case: 'inputJson', value: '{"x"' },
      }),
    },
  })
  const { control } = fakeControl({ stream: (_i, o) => replay([inputDelta], o?.signal) })
  const conversation = new Conversation('c', { control })
  await conversation.open()
  await flush()
  expect(conversation.blocks).toHaveLength(0)
  conversation.dispose()
})

it('overlap duplicate trimmed', async () => {
  const history = [userEvent('c', 'hi')]
  const events = [userEvent('c', 'hi', 6)]
  const { control } = fakeControl({ latest: 5, history, stream: (_i, o) => replay(events, o?.signal) })
  const conversation = new Conversation('c', { control })
  await conversation.open()
  await flush()
  expect(conversation.blocks).toHaveLength(1)
  conversation.dispose()
})

it('first non-matching message ends trimming', async () => {
  const history = [userEvent('c', 'a')]
  const events = [userEvent('c', 'b', 6), userEvent('c', 'a', 7)]
  const { control } = fakeControl({ latest: 5, history, stream: (_i, o) => replay(events, o?.signal) })
  const conversation = new Conversation('c', { control })
  await conversation.open()
  await flush()
  expect(conversation.blocks).toHaveLength(3)
  conversation.dispose()
})

it('pause keeps cursor and resume reopens from it without history', async () => {
  const events = [userEvent('c', 'later', 7)]
  const { control, order, streamRequests } = fakeControl({
    latest: 3,
    history: [userEvent('c', 'hi', 3)],
    stream: (_i, o) => replay(events, o?.signal),
  })
  const conversation = new Conversation('c', { control })
  await conversation.open()
  await flush()
  expect(conversation.cursor).toBe(7)
  conversation.pause()
  conversation.resume()
  await flush()
  expect(streamRequests).toHaveLength(2)
  expect(cursorOf(streamRequests[0])).toBe(3)
  expect(cursorOf(streamRequests[1])).toBe(7)
  expect(order.filter((name) => name === 'getHistory')).toHaveLength(1)
  conversation.dispose()
})

it('hidden during open opens no stream until resume', async () => {
  const { control, streamRequests } = fakeControl({ stream: (_i, o) => idle(o?.signal) })
  const conversation = new Conversation('c', { control })
  conversation.pause()
  await conversation.open()
  expect(streamRequests).toHaveLength(0)
  conversation.resume()
  await flush()
  expect(streamRequests).toHaveLength(1)
  conversation.dispose()
})

it('dispose during the join opens no stream', async () => {
  const { control, streamRequests } = fakeControl({ stream: (_i, o) => idle(o?.signal) })
  const conversation = new Conversation('c', { control })
  const opening = conversation.open()
  conversation.dispose()
  await opening
  await flush()
  expect(streamRequests).toHaveLength(0)
})

it('resume after a failed open re-runs the join, not a bare stream', async () => {
  const { control, order, streamRequests } = fakeControl({
    childError: new Error('boom'),
    stream: (_i, o) => idle(o?.signal),
  })
  const conversation = new Conversation('c', { control })
  await conversation.open()
  expect(conversation.error).toBe('boom')
  expect(streamRequests).toHaveLength(0)

  conversation.pause()
  conversation.resume()
  await flush()
  expect(order.filter((name) => name === 'getChild')).toHaveLength(2)
  expect(streamRequests).toHaveLength(0)
  conversation.dispose()
})

it('reconnect backoff resets on each delivered connection', async () => {
  vi.useFakeTimers()
  vi.spyOn(Math, 'random').mockReturnValue(0)
  try {
    let calls = 0
    const { control } = fakeControl({
      stream: () => {
        calls += 1
        return finite([userEvent('c', 'x', calls)])
      },
    })
    const conversation = new Conversation('c', { control })
    await conversation.open()
    await vi.advanceTimersByTimeAsync(0)
    expect(calls).toBe(1)
    // Each reconnect that delivered an event must wait only the base 250ms, not
    // the growing 500ms·2^n a stale attempt counter would produce.
    for (let i = 0; i < 6; i += 1) {
      await vi.advanceTimersByTimeAsync(250)
      await vi.advanceTimersByTimeAsync(0)
    }
    expect(calls).toBe(7)
    conversation.dispose()
  } finally {
    vi.restoreAllMocks()
    vi.useRealTimers()
  }
})

it('router keeps the full id list and the transcript slices three panes', () => {
  const route = parseRoute('#/c/a,b,c,d')
  expect(route.name).toBe('transcript')
  expect(route.ids).toEqual(['a', 'b', 'c', 'd'])

  const overflow = transcriptPanes(route.ids)
  expect(overflow.overflow).toBe(true)
  expect(overflow.shown).toEqual(['a', 'b', 'c'])

  const within = transcriptPanes(['a', 'b'])
  expect(within.overflow).toBe(false)
  expect(within.shown).toEqual(['a', 'b'])
})

it('a tool-result-only user message appends no block', async () => {
  const resultsOnly = create(EventSchema, {
    childId: 'c',
    ordinal: 0,
    payload: {
      case: 'userMessage',
      value: create(UserMessageSchema, {
        content: [
          create(ContentBlockSchema, {
            block: { case: 'toolResult', value: create(ToolResultBlockSchema, { toolUseId: 't1' }) },
          }),
        ],
      }),
    },
  })
  const { control } = fakeControl({ stream: (_i, o) => replay([resultsOnly], o?.signal) })
  const conversation = new Conversation('c', { control })
  await conversation.open()
  await flush()
  expect(conversation.blocks).toHaveLength(0)
  conversation.dispose()
})

it('stops on unauthenticated', async () => {
  const error = new ConnectError('unauthorized', Code.Unauthenticated)
  const { control, order } = fakeControl({
    stream: () => {
      throw error
    },
  })
  const conversation = new Conversation('c', { control })
  await conversation.open()
  await flush()
  expect(conversation.error).toContain('unauthorized')
  expect(order.filter((name) => name === 'streamEvents')).toHaveLength(1)
  conversation.dispose()
})
