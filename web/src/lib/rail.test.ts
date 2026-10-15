import { create } from '@bufbuild/protobuf'
import { Code, ConnectError } from '@connectrpc/connect'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import {
  AgentStatusSchema,
  ChildSpawnedSchema,
  EventSchema,
} from '../gen/rafiki/v1/event_pb'
import type { Event } from '../gen/rafiki/v1/event_pb'
import { ChildSummarySchema } from '../gen/rafiki/v1/control_pb'
import type {
  ChildSummary,
  StreamEventsRequest,
} from '../gen/rafiki/v1/control_pb'
import {
  children,
  latest,
  setControlForTest,
  startRail,
  stopRail,
  type ControlClient,
} from './rail.svelte'

interface StreamScript {
  events?: Event[]
  error?: unknown
}

class FakeControl {
  listCalls = 0
  streamReqs: StreamEventsRequest[] = []
  getChildCalls: string[] = []
  summaries: ChildSummary[] = []
  scripts: StreamScript[] = []
  childById: { [id: string]: ChildSummary } = {}

  listChildren(): Promise<{ children: ChildSummary[] }> {
    this.listCalls++
    return Promise.resolve({ children: this.summaries })
  }

  getChild(request: { childId: string }): Promise<{ child: ChildSummary | undefined }> {
    this.getChildCalls.push(request.childId)
    return Promise.resolve({ child: this.childById[request.childId] })
  }

  streamEvents(request: StreamEventsRequest): AsyncIterable<Event> {
    const script = this.scripts[this.streamReqs.length] ?? { events: [] }
    this.streamReqs.push(request)
    return (async function* generate() {
      for (const event of script.events ?? []) {
        yield event
      }
      if (script.error !== undefined) {
        throw script.error
      }
    })()
  }
}

function summary(init: {
  childId: string
  name?: string
  status?: string
  latestOrdinal?: number
}): ChildSummary {
  return create(ChildSummarySchema, {
    childId: init.childId,
    name: init.name ?? init.childId,
    status: init.status ?? 'idle',
    kind: 'fundi',
    model: 'openrouter/x',
    latestOrdinal: init.latestOrdinal,
  })
}

function statusEvent(childId: string, state: string, ordinal?: number): Event {
  return create(EventSchema, {
    childId,
    ordinal,
    payload: {
      case: 'agentStatus',
      value: create(AgentStatusSchema, { state }),
    },
  })
}

function spawnedEvent(childId: string, parentId = '', ordinal = 0): Event {
  return create(EventSchema, {
    childId,
    ordinal,
    payload: {
      case: 'childSpawned',
      value: create(ChildSpawnedSchema, { childId, parentId, name: childId }),
    },
  })
}

let fake: FakeControl

async function flush(): Promise<void> {
  await vi.advanceTimersByTimeAsync(0)
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.spyOn(Math, 'random').mockReturnValue(0)
  fake = new FakeControl()
  setControlForTest(fake as unknown as ControlClient)
})

afterEach(() => {
  stopRail()
  vi.restoreAllMocks()
  vi.useRealTimers()
})

it('seeds cursor from list', async () => {
  fake.summaries = [summary({ childId: 'c_1', latestOrdinal: 5 })]
  startRail()
  await flush()

  expect(fake.listCalls).toBe(1)
  expect(fake.streamReqs.length).toBe(1)
  expect(fake.streamReqs[0].cursor?.ordinals).toEqual({ c_1: 5 })
  expect(latest.get('c_1')).toBe(5)
  expect(children.get('c_1')?.status).toBe('idle')
})

it('child_spawned fetches and inserts row', async () => {
  fake.summaries = []
  fake.childById['c_9'] = summary({ childId: 'c_9', name: 'worker', status: 'streaming' })
  fake.scripts = [{ events: [spawnedEvent('c_9')] }]

  startRail()
  await flush()
  await flush()

  expect(fake.getChildCalls).toContain('c_9')
  expect(children.get('c_9')?.name).toBe('worker')
  expect(children.get('c_9')?.status).toBe('streaming')
})

it('ordinal max-tracked and unset ordinal ignored', async () => {
  fake.summaries = [summary({ childId: 'c_1', latestOrdinal: 2 })]
  fake.scripts = [
    {
      events: [statusEvent('c_1', 'streaming', 4), statusEvent('c_1', 'idle')],
    },
  ]

  startRail()
  await flush()

  expect(latest.get('c_1')).toBe(4)
  expect(children.get('c_1')?.status).toBe('idle')
})

it('unset latest_ordinal seeds -1', async () => {
  fake.summaries = [summary({ childId: 'c_1' })]
  startRail()
  await flush()

  expect(latest.get('c_1')).toBe(-1)
  expect(fake.streamReqs[0].cursor?.ordinals).toEqual({ c_1: -1 })
})

it('startRail idempotent and restartable', async () => {
  fake.summaries = [summary({ childId: 'c_1' })]
  startRail()
  startRail()
  await flush()

  expect(fake.listCalls).toBe(1)

  stopRail()
  startRail()
  await flush()

  expect(fake.listCalls).toBe(2)
  expect(fake.streamReqs.length).toBe(2)
})

it('reconnects with current cursor', async () => {
  fake.summaries = [summary({ childId: 'c_1', latestOrdinal: 0 })]
  fake.scripts = [
    { events: [statusEvent('c_1', 'streaming', 7)] },
    { events: [] },
  ]

  startRail()
  await flush()
  expect(fake.streamReqs.length).toBe(1)

  // The first stream ended; the backoff timer reopens with the cursor advanced.
  await vi.advanceTimersByTimeAsync(250)
  await flush()

  expect(fake.streamReqs.length).toBe(2)
  expect(fake.streamReqs[1].cursor?.ordinals).toEqual({ c_1: 7 })
})

it('stops on unauthenticated', async () => {
  fake.summaries = [summary({ childId: 'c_1' })]
  fake.scripts = [{ error: new ConnectError('no credential', Code.Unauthenticated) }]

  startRail()
  await flush()
  expect(fake.streamReqs.length).toBe(1)

  await vi.advanceTimersByTimeAsync(60_000)
  expect(fake.streamReqs.length).toBe(1)
})
