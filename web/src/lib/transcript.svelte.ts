import { toJson } from '@bufbuild/protobuf'
import type { Client } from '@connectrpc/connect'
import { Code, ConnectError } from '@connectrpc/connect'
import type { Duration } from '@bufbuild/protobuf/wkt'
import type { ChildSummary, Control } from '../gen/rafiki/v1/control_pb'
import { EventTier } from '../gen/rafiki/v1/control_pb'
import type {
  ContentBlock,
  Event,
  ToolExecutionEnd,
  ToolExecutionStart,
  TurnEnd,
} from '../gen/rafiki/v1/event_pb'
import { ContentBlockSchema } from '../gen/rafiki/v1/event_pb'

/** The real Connect client. Tests pass a structural stand-in cast to this. */
export type ControlClient = Client<typeof Control>

export type BlockKind = 'user' | 'assistant' | 'tool' | 'system'

/** One tool invocation, with its result once the tool_result arrives. */
export interface ToolCallView {
  id: string
  name: string
  inputJson: string
  resultText: string
  resultBlocks: ContentBlock[]
  isError: boolean
  hasResult: boolean
  running: boolean
  durationMs?: number
}

/** One renderable unit in the transcript. */
export interface TranscriptBlock {
  kind: BlockKind
  content: ContentBlock[]
  text: string
  thinking: string
  toolCalls: ToolCallView[]
  costUsd?: number
  /** An ephemeral delta block, replaced by the next durable assistant_message. */
  inFlight: boolean
}

export interface ConversationDeps {
  control: ControlClient
}

const TRIM_WINDOW = 8

/** The rail's reconnect schedule: jittered, capped at 30s. */
export function backoffDelay(attempt: number, rand: number = Math.random()): number {
  const capped = Math.min(30_000, 500 * 2 ** Math.max(0, attempt))
  return capped * (0.5 + rand / 2)
}

/**
 * A tool execution's duration in milliseconds, or undefined when the start was
 * never seen. The proto's `duration` is a google.protobuf.Duration whose ZERO
 * value means "start never seen" (a turn resumed mid-tool, event.proto:180) — so
 * both an absent duration and a zero one render as `-` in ToolCall, never `0ms`.
 * Nanos are folded in, so a sub-second tool does not read as `0ms` either.
 */
export function toolDurationMs(duration: Duration | undefined): number | undefined {
  if (duration === undefined || (duration.seconds === 0n && duration.nanos === 0)) {
    return undefined
  }
  return Number(duration.seconds) * 1000 + duration.nanos / 1e6
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      signal.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    const onAbort = (): void => {
      clearTimeout(timer)
      resolve()
    }
    signal.addEventListener('abort', onAbort, { once: true })
  })
}

function errorMessage(err: unknown): string {
  if (err instanceof Error) {
    return err.message
  }
  return String(err)
}

/** A terminal stream failure: reconnecting would only repeat it. */
function isTerminal(err: unknown): boolean {
  if (err instanceof ConnectError) {
    if (err.code === Code.Unauthenticated) {
      return true
    }
    if (err.metadata.get('rafiki-web-gate') === 'refused') {
      return true
    }
  }
  return false
}

/**
 * A (role, content) fingerprint so the join can tell "the same turn delivered
 * twice" from "a new turn". toJson is canonical, so history and stream copies
 * of one message stringify identically.
 */
function messageKey(role: string, content: ContentBlock[]): string {
  return role + ':' + JSON.stringify(content.map((block) => toJson(ContentBlockSchema, block)))
}

interface Projected {
  text: string
  thinking: string
  toolCalls: ToolCallView[]
}

function project(content: ContentBlock[]): Projected {
  let text = ''
  let thinking = ''
  const toolCalls: ToolCallView[] = []
  for (const block of content) {
    switch (block.block.case) {
      case 'text':
        text += block.block.value.text
        break
      case 'thinking':
        thinking += block.block.value.thinking
        break
      case 'toolUse':
        toolCalls.push({
          id: block.block.value.id,
          name: block.block.value.name,
          inputJson: block.block.value.inputJson,
          resultText: '',
          resultBlocks: [],
          isError: false,
          hasResult: false,
          running: false,
        })
        break
      default:
        break
    }
  }
  return { text, thinking, toolCalls }
}

function resultBlocks(content: ContentBlock[]): ContentBlock[] {
  return content.filter((block) => block.block.case === 'toolResult')
}

/**
 * Conversation drives one child's transcript: a GetChild/GetHistory join
 * followed by a live StreamEvents subscription, ported from the cockpit's
 * openFocus/historyCmd/startFocus.
 */
export class Conversation {
  readonly childId: string
  private readonly deps: ConversationDeps

  child = $state<ChildSummary | undefined>(undefined)
  status = $state('')
  blocks = $state<TranscriptBlock[]>([])
  /** The highest durable event ordinal folded in, or the join cursor. */
  cursor = $state(-1)
  error = $state<string | undefined>(undefined)
  /** Set from turn_end for the per-turn cost in the margin. */
  turnCostUsd = $state<number | undefined>(undefined)

  private trimming = false
  private recentHistory: string[] = []
  private paused = false
  private disposed = false
  /** True once the join succeeded, so resume() never streams without history. */
  private opened = false
  /** The in-flight join, so a concurrent resume()/open() cannot duplicate it. */
  private opening: Promise<void> | undefined
  private abort: AbortController | undefined

  constructor(childId: string, deps: ConversationDeps) {
    this.childId = childId
    this.deps = deps
  }

  open(): Promise<void> {
    if (this.opening) {
      return this.opening
    }
    this.opening = this.doOpen().finally(() => {
      this.opening = undefined
    })
    return this.opening
  }

  private async doOpen(): Promise<void> {
    this.error = undefined
    let after: number
    try {
      // The log head is captured BEFORE the fetch: taking it afterwards would
      // drop any turn that landed while the fetch was in flight.
      const resp = await this.deps.control.getChild({ childId: this.childId })
      this.child = resp.child
      this.status = resp.child?.status ?? ''
      after = resp.child?.latestOrdinal ?? -1
    } catch (err) {
      this.error = errorMessage(err)
      return
    }

    try {
      const resp = await this.deps.control.getHistory({ childId: this.childId })
      for (const event of resp.events) {
        this.applyEvent(event, true)
      }
      this.recentHistory = this.recentHistory.slice(-TRIM_WINDOW)
      this.trimming = true
    } catch {
      after = -1
    }
    this.cursor = after
    this.opened = true
    this.openStream()
  }

  /** Close the stream but keep the cursor; the next resume replays from it. */
  pause(): void {
    this.paused = true
    this.abort?.abort()
    this.abort = undefined
  }

  /**
   * Reopen the stream from the cursor, without refetching history. Before the
   * join has succeeded (a failed open() left only an error banner) there is no
   * cursor to replay from, so re-run open() rather than stream from -1.
   */
  resume(): void {
    if (!this.paused) {
      return
    }
    this.paused = false
    // A join is still in flight; it opens the stream on completion (paused is
    // now false), so a second open() here would duplicate the history.
    if (this.opening) {
      return
    }
    if (!this.opened) {
      void this.open()
      return
    }
    this.openStream()
  }

  dispose(): void {
    this.disposed = true
    this.abort?.abort()
    this.abort = undefined
  }

  private openStream(): void {
    // A stream must not outlive its pane or run while the tab is hidden: either
    // would leak one of the tab's three conversation streams until resume.
    if (this.disposed || this.paused) {
      return
    }
    this.abort?.abort()
    const controller = new AbortController()
    this.abort = controller
    void this.pump(controller.signal)
  }

  private async pump(signal: AbortSignal): Promise<void> {
    let attempt = 0
    while (!signal.aborted) {
      let received = false
      try {
        await this.consume(signal, () => {
          if (!received) {
            received = true
            // A connection that delivered an event was healthy: the next
            // reconnect starts from a clean backoff, like the rail.
            attempt = 0
          }
        })
      } catch (err) {
        if (signal.aborted) {
          return
        }
        if (isTerminal(err)) {
          this.error = errorMessage(err)
          return
        }
      }
      if (signal.aborted) {
        return
      }
      await sleep(backoffDelay(attempt), signal)
      attempt += 1
    }
  }

  private async consume(signal: AbortSignal, onFirstEvent: () => void): Promise<void> {
    const stream = this.deps.control.streamEvents(
      {
        subject: { scope: { case: 'child', value: this.childId } },
        tier: EventTier.ALL,
        cursor: { ordinals: { [this.childId]: this.cursor } },
      },
      { signal },
    )
    let first = true
    for await (const event of stream) {
      if (first) {
        first = false
        onFirstEvent()
      }
      this.applyEvent(event, false)
    }
  }

  private applyEvent(event: Event, fromHistory: boolean): void {
    if (event.childId !== '' && event.childId !== this.childId) {
      return
    }
    if (!fromHistory) {
      const ordinal = event.ordinal
      if (ordinal !== undefined) {
        if (ordinal <= this.cursor) {
          return
        }
        this.cursor = ordinal
      }
    }
    this.applyPayload(event, fromHistory)
  }

  private applyPayload(event: Event, fromHistory: boolean): void {
    switch (event.payload.case) {
      case 'userMessage': {
        const content = event.payload.value.content
        if (!fromHistory && this.trimDuplicate('user', content)) {
          return
        }
        this.recordHistory('user', content, fromHistory)
        const projected = project(content)
        const block: TranscriptBlock = {
          kind: 'user',
          content,
          text: projected.text,
          thinking: projected.thinking,
          toolCalls: projected.toolCalls,
          inFlight: false,
        }
        // A user message that carries nothing but tool results answers an
        // earlier tool_use; it is not a turn of its own.
        this.routeResults(content)
        if (content.some((item) => item.block.case !== 'toolResult')) {
          this.blocks = [...this.blocks, block]
        }
        break
      }
      case 'assistantMessage': {
        const content = event.payload.value.content
        if (!fromHistory && this.trimDuplicate('assistant', content)) {
          return
        }
        this.recordHistory('assistant', content, fromHistory)
        // The durable message supersedes any in-flight delta block.
        this.blocks = this.blocks.filter(
          (block) => !(block.inFlight && block.kind === 'assistant'),
        )
        const projected = project(content)
        this.blocks = [
          ...this.blocks,
          {
            kind: 'assistant',
            content,
            text: projected.text,
            thinking: projected.thinking,
            toolCalls: projected.toolCalls,
            costUsd: event.payload.value.costUsd,
            inFlight: false,
          },
        ]
        break
      }
      case 'systemMessage': {
        // A stored system row is a harness injection (a heartbeat nudge, an
        // environment update), not a user turn: it renders under its own kind.
        const content = event.payload.value.content
        if (!fromHistory && this.trimDuplicate('system', content)) {
          return
        }
        this.recordHistory('system', content, fromHistory)
        const projected = project(content)
        this.blocks = [
          ...this.blocks,
          {
            kind: 'system',
            content,
            text: projected.text,
            thinking: projected.thinking,
            toolCalls: projected.toolCalls,
            inFlight: false,
          },
        ]
        break
      }
      case 'contentBlockDelta': {
        const delta = event.payload.value.delta
        // An input_json delta belongs to a tool_use fragment, not the assistant
        // text: it must not open an empty in-flight block.
        if (delta.case !== 'text' && delta.case !== 'thinking') {
          break
        }
        const value = delta.value
        const last = this.blocks[this.blocks.length - 1]
        let block: TranscriptBlock
        if (last && last.inFlight && last.kind === 'assistant') {
          block = last
        } else {
          block = { kind: 'assistant', content: [], text: '', thinking: '', toolCalls: [], inFlight: true }
          this.blocks = [...this.blocks, block]
        }
        if (delta.case === 'text') {
          block.text += value
        } else {
          block.thinking += value
        }
        break
      }
      case 'turnEnd':
        this.applyTurnEnd(event.payload.value)
        break
      case 'agentStatus':
        this.status = event.payload.value.state
        break
      case 'toolExecutionStart':
        this.applyToolStart(event.payload.value)
        break
      case 'toolExecutionEnd':
        this.applyToolEnd(event.payload.value)
        break
      case 'error': {
        const block: TranscriptBlock = {
          kind: 'system',
          content: [],
          text: event.payload.value.message,
          thinking: '',
          toolCalls: [],
          inFlight: false,
        }
        this.blocks = [...this.blocks, block]
        break
      }
      case 'childExited':
        this.status = 'exited'
        break
      default:
        break
    }
  }

  private recordHistory(role: string, content: ContentBlock[], fromHistory: boolean): void {
    if (!fromHistory) {
      return
    }
    this.recentHistory = [...this.recentHistory, messageKey(role, content)].slice(-TRIM_WINDOW)
  }

  private trimDuplicate(role: string, content: ContentBlock[]): boolean {
    if (!this.trimming) {
      return false
    }
    // Accepted edge: a genuinely repeated identical message right after the
    // join is dropped along with the join overlap it is indistinguishable from.
    // A dropped message also never reaches markDelivered, so its queued send
    // entry stays "queued" forever.
    if (this.recentHistory.includes(messageKey(role, content))) {
      return true
    }
    this.trimming = false
    return false
  }

  private routeResults(content: ContentBlock[]): void {
    const results = resultBlocks(content)
    for (const result of results) {
      if (result.block.case !== 'toolResult') {
        continue
      }
      const value = result.block.value
      for (let i = this.blocks.length - 1; i >= 0; i -= 1) {
        const call = this.blocks[i].toolCalls.find((candidate) => candidate.id === value.toolUseId)
        if (call) {
          call.hasResult = true
          call.isError = value.isError
          call.resultBlocks = value.content
          call.resultText = value.content
            .filter((block) => block.block.case === 'text')
            .map((block) => (block.block.case === 'text' ? block.block.value.text : ''))
            .join('')
          break
        }
      }
    }
  }

  private applyTurnEnd(end: TurnEnd): void {
    this.turnCostUsd = end.costUsd
  }

  private applyToolStart(start: ToolExecutionStart): void {
    this.updateToolCall(start.toolUseId, (call) => {
      call.running = true
      if (start.name !== '') {
        call.name = start.name
      }
    })
  }

  private applyToolEnd(end: ToolExecutionEnd): void {
    this.updateToolCall(end.toolUseId, (call) => {
      call.running = false
      call.hasResult = true
      call.isError = end.isError
      call.durationMs = toolDurationMs(end.duration)
    })
  }

  private updateToolCall(id: string, update: (call: ToolCallView) => void): void {
    for (let i = this.blocks.length - 1; i >= 0; i -= 1) {
      const call = this.blocks[i].toolCalls.find((candidate) => candidate.id === id)
      if (call) {
        update(call)
        return
      }
    }
  }
}
