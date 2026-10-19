import type { MessageInitShape } from '@bufbuild/protobuf'
import { timestampDate, timestampFromDate } from '@bufbuild/protobuf/wkt'
import type { Client } from '@connectrpc/connect'
import { Code, ConnectError } from '@connectrpc/connect'
import { SvelteMap } from 'svelte/reactivity'
import {
  Control,
  EventTier,
  StreamEventsRequestSchema,
} from '../gen/rafiki/v1/control_pb'
import type { ChildSummary } from '../gen/rafiki/v1/control_pb'
import type { Event } from '../gen/rafiki/v1/event_pb'
import { control as defaultControl } from './client'

export type ControlClient = Client<typeof Control>

/** The daemon-written label carrying a child's parent id (rail.ParentLabel). */
export const PARENT_LABEL = 'rafiki/parent'

export interface ChildRow {
  id: string
  name: string
  status: string
  kind: string
  model: string
  costUsd?: number
  maxCost?: number
  startedAt?: Date
  labels: { [key: string]: string }
  parentId?: string
}

// Six small messages, mirroring the cockpit's rail filter
// (pkg/tui/rail/attention.go). Content-bearing events are deliberately absent:
// the screen needs to know a turn HAPPENED, not what it said.
const RAIL_TYPES = [
  'turn_end',
  'agent_status',
  'error',
  'retry',
  'child_spawned',
  'child_exited',
  'child_renamed',
]

const BASE_BACKOFF_MS = 500
const MAX_BACKOFF_MS = 30_000
// Bounds how far back a reconnect replays a child the cursor does not name,
// mirroring the cockpit's replayFloor.
const REPLAY_FLOOR_MS = 24 * 60 * 60 * 1000

/** One module-level rail per browser tab. */
export const children = new SvelteMap<string, ChildRow>()
export const latest = new SvelteMap<string, number>()

let client: ControlClient = defaultControl
let running = false
let generation = 0
let controller: AbortController | null = null
let reconnectTimer: ReturnType<typeof setTimeout> | null = null
let backoffAttempt = 0

export function setControlForTest(injected: ControlClient): void {
  client = injected
}

/**
 * Starts the rail: seed from ListChildren, then follow a durable, all-children
 * event subscription. A no-op while it is already running; a restart after
 * stopRail. Seeding is synchronous state, so the guard closes the double-mount
 * race the shell's remount can otherwise open.
 */
export function startRail(): void {
  if (running) {
    return
  }
  running = true
  generation++
  backoffAttempt = 0
  void run(generation)
}

/** Stops the rail and clears the board, so a restart re-seeds cleanly. */
export function stopRail(): void {
  if (!running && controller === null && reconnectTimer === null) {
    return
  }
  running = false
  generation++
  if (reconnectTimer !== null) {
    clearTimeout(reconnectTimer)
    reconnectTimer = null
  }
  controller?.abort()
  controller = null
  children.clear()
  latest.clear()
}

async function run(gen: number): Promise<void> {
  try {
    await seed()
  } catch (error) {
    if (gen !== generation) {
      return
    }
    if (isTerminal(error)) {
      running = false
      return
    }
    // A failed seed leaves children/latest empty, so reconnecting the STREAM
    // alone would follow a board that never got its rows. Retry the whole run.
    scheduleReconnect(gen, true)
    return
  }
  if (gen !== generation) {
    return
  }
  await openStream(gen)
}

async function seed(): Promise<void> {
  const response = await client.listChildren({})
  for (const summary of response.children) {
    insertRow(summary)
    // latest_ordinal is optional: unset means the child has no events, and
    // that must seed -1, never 0 — 0 is a real ordinal.
    latest.set(summary.childId, summary.latestOrdinal ?? -1)
  }
}

async function openStream(gen: number): Promise<void> {
  if (gen !== generation) {
    return
  }
  controller = new AbortController()
  const { signal } = controller
  let received = false
  try {
    const stream = client.streamEvents(buildRequest(), { signal })
    for await (const event of stream) {
      if (gen !== generation) {
        return
      }
      if (!received) {
        received = true
        backoffAttempt = 0
      }
      applyEvent(event)
    }
    if (gen !== generation || signal.aborted) {
      return
    }
    scheduleReconnect(gen)
  } catch (error) {
    if (gen !== generation || signal.aborted) {
      return
    }
    if (isTerminal(error)) {
      // Unauthenticated or a gate refusal: authState owns the response; the
      // rail stops rather than hammering a wall.
      running = false
      return
    }
    scheduleReconnect(gen)
  }
}

function buildRequest(): MessageInitShape<typeof StreamEventsRequestSchema> {
  const ordinals: { [key: string]: number } = {}
  for (const [id, ordinal] of latest) {
    ordinals[id] = ordinal
  }
  return {
    subject: { scope: { case: 'all', value: true } },
    tier: EventTier.DURABLE,
    types: RAIL_TYPES,
    cursor: {
      ordinals,
      floor: timestampFromDate(new Date(Date.now() - REPLAY_FLOOR_MS)),
    },
  }
}

function applyEvent(event: Event): void {
  const id = event.childId
  if (id === '') {
    return
  }
  if (event.ordinal !== undefined) {
    const current = latest.get(id)
    latest.set(id, current === undefined ? event.ordinal : Math.max(current, event.ordinal))
  }
  const payload = event.payload
  switch (payload.case) {
    case 'childSpawned':
      // A provisional row keeps status events arriving before the fetch lands
      // from being dropped; getChild enriches it.
      if (!children.has(id)) {
        children.set(id, {
          id,
          name: payload.value.name,
          status: 'spawning',
          kind: '',
          model: '',
          labels: {},
          parentId: payload.value.parentId || undefined,
        })
      }
      void fetchChild(id)
      break
    case 'childExited':
      patchRow(id, { status: 'exited' })
      break
    case 'agentStatus':
      patchRow(id, { status: payload.value.state })
      break
    case 'childRenamed':
      patchRow(id, { name: payload.value.name })
      break
    case 'turnEnd': {
      const cost = payload.value.costUsd
      if (cost !== undefined) {
        const row = children.get(id)
        if (row) {
          patchRow(id, { costUsd: (row.costUsd ?? 0) + cost })
        }
      }
      break
    }
    default:
      break
  }
}

async function fetchChild(id: string): Promise<void> {
  try {
    const response = await client.getChild({ childId: id })
    if (response.child) {
      insertRow(response.child)
    }
  } catch (error) {
    // The provisional row stands; only a terminal error is worth stopping for.
    if (isTerminal(error)) {
      running = false
    }
  }
}

function insertRow(summary: ChildSummary): void {
  children.set(summary.childId, {
    id: summary.childId,
    name: summary.name,
    status: summary.status,
    kind: summary.kind,
    model: summary.model,
    costUsd: summary.costUsd,
    maxCost: summary.maxCost,
    startedAt: summary.startedAt ? timestampDate(summary.startedAt) : undefined,
    labels: { ...summary.labels },
    parentId: summary.labels[PARENT_LABEL] || undefined,
  })
}

function patchRow(id: string, patch: Partial<ChildRow>): void {
  const row = children.get(id)
  if (!row) {
    return
  }
  children.set(id, { ...row, ...patch })
}

// reseed re-runs run() (seed then stream) instead of reopening the stream
// alone; a seed failure must retry the seed, not follow an empty board.
function scheduleReconnect(gen: number, reseed = false): void {
  if (gen !== generation) {
    return
  }
  const delay = backoffDelay(backoffAttempt)
  backoffAttempt++
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null
    if (gen !== generation) {
      return
    }
    void (reseed ? run(gen) : openStream(gen))
  }, delay)
}

// min(30s, 500ms·2^n) · (0.5 + random/2), the cockpit's jittered backoff.
function backoffDelay(attempt: number): number {
  const base = Math.min(MAX_BACKOFF_MS, BASE_BACKOFF_MS * 2 ** attempt)
  return base * (0.5 + Math.random() / 2)
}

function isTerminal(error: unknown): boolean {
  if (!(error instanceof ConnectError)) {
    return false
  }
  if (error.metadata.get('rafiki-web-gate') === 'refused') {
    return true
  }
  return error.code === Code.Unauthenticated
}
