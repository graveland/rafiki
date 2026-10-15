import { toJson } from '@bufbuild/protobuf'
import type { Client } from '@connectrpc/connect'
import { Code, ConnectError } from '@connectrpc/connect'
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

  private after = -1
  private trimming = false
  private recentHistory: string[] = []
  private paused = false
  private abort: AbortController | undefined

  constructor(childId: string, deps: ConversationDeps) {
    this.childId = childId
    this.deps = deps
  }

  async open(): Promise<void> {
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
    this.after = after

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
    this.openStream()
  }

  /** Close the stream but keep the cursor; the next resume replays from it. */
  pause(): void {
    this.paused = true
    this.abort?.abort()
    this.abort = undefined
  }

  /** Reopen the stream from the cursor, without refetching history. */
  resume(): void {
    if (!this.paused) {
      return
    }
    this.paused = false
    this.openStream()
  }

  dispose(): void {
    this.abort?.abort()
    this.abort = undefined
  }

  private openStream(): void {
    const controller = new AbortController()
    this.abort = controller
    void this.pump(controller.signal)
  }

  private async pump(signal: AbortSignal): Promise<void> {
    let attempt = 0
    while (!signal.aborted) {
      try {
        await this.consume(signal)
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

  private async consume(signal: AbortSignal): Promise<void> {
    const stream = this.deps.control.streamEvents(
      {
        subject: { scope: { case: 'child', value: this.childId } },
        tier: EventTier.ALL,
        cursor: { ordinals: { [this.childId]: this.cursor } },
      },
      { signal },
    )
    for await (const event of stream) {
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
      case 'contentBlockDelta': {
        const delta = event.payload.value.delta
        if (delta.case === undefined) {
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
        } else if (delta.case === 'thinking') {
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
      call.durationMs = end.duration === undefined ? undefined : Number(end.duration.seconds) * 1000
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
