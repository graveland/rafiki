<script lang="ts">
  import { timestampDate } from '@bufbuild/protobuf/wkt'
  import type {
    ConversationExportResponse,
    ConversationSummary,
    RecallHit,
  } from '../gen/rafiki/v1/control_pb'
  import DataTable from '../lib/components/DataTable.svelte'
  import { control } from '../lib/client'
  import { relTime, tokens, usd } from '../lib/format'
  import { navigate } from '../lib/router'
  import { pushToast } from '../lib/toasts'

  type Mode = 'conversations' | 'recall'

  interface ConversationRow {
    kind: 'conversation'
    id: string
    name: string
    model: string
    status: string
    turns: number
    inputTokens: bigint
    outputTokens: bigint
    costUsd: number
    createdAt?: Date
    closedAt?: Date
  }

  interface RecallRow {
    kind: 'recall'
    id: string
    source: string
    title: string
    snippet: string
    conversationId: string
    conversationName: string
    repo: string
    score: number
    when?: Date
  }

  type Row = ConversationRow | RecallRow

  let mode = $state<Mode>('conversations')
  let query = $state('')
  let rows = $state<Row[]>([])
  let searching = $state(false)
  let exportTitle = $state('')
  let exportText = $state<string | null>(null)

  // conversation id -> live child id. A fundi child's conversation UUID is its
  // childstore session_id, which ChildSummary carries, so this is the one
  // wire-available link from a conversation hit to the child id the transcript
  // route keys on. A proxy child's conversation is correlated by external_ref,
  // which no wire field exposes, so its hits fall through to the export view.
  let liveChildren: Record<string, string> = {}

  const conversationColumns = [
    {
      key: 'name',
      label: 'Name',
      sortable: true,
      render: (row: Row) => (row.kind === 'conversation' ? orDash(row.name) : ''),
    },
    {
      key: 'model',
      label: 'Model',
      sortable: true,
      render: (row: Row) => (row.kind === 'conversation' ? orDash(row.model) : ''),
    },
    {
      key: 'status',
      label: 'Status',
      sortable: true,
      render: (row: Row) => (row.kind === 'conversation' ? orDash(row.status) : ''),
    },
    {
      key: 'turns',
      label: 'Turns',
      sortable: true,
      align: 'right' as const,
      render: (row: Row) => (row.kind === 'conversation' ? String(row.turns) : ''),
    },
    {
      key: 'tokens',
      label: 'Tokens',
      align: 'right' as const,
      render: (row: Row) =>
        row.kind === 'conversation' ? tokens(row.inputTokens + row.outputTokens) : '',
    },
    {
      key: 'cost',
      label: 'Cost',
      sortable: true,
      align: 'right' as const,
      render: (row: Row) => (row.kind === 'conversation' ? usd(row.costUsd) : ''),
    },
    {
      key: 'created',
      label: 'Created',
      sortable: true,
      render: (row: Row) => (row.kind === 'conversation' ? relTime(row.createdAt) : ''),
    },
  ]

  const recallColumns = [
    {
      key: 'source',
      label: 'Source',
      sortable: true,
      render: (row: Row) => (row.kind === 'recall' ? orDash(row.source) : ''),
    },
    {
      key: 'title',
      label: 'Title',
      sortable: true,
      render: (row: Row) => (row.kind === 'recall' ? orDash(row.title) : ''),
    },
    {
      key: 'snippet',
      label: 'Snippet',
      render: (row: Row) => (row.kind === 'recall' ? orDash(row.snippet) : ''),
    },
    {
      key: 'conversation',
      label: 'Conversation',
      render: (row: Row) => (row.kind === 'recall' ? orDash(row.conversationName) : ''),
    },
    {
      key: 'repo',
      label: 'Repo',
      sortable: true,
      render: (row: Row) => (row.kind === 'recall' ? orDash(row.repo) : ''),
    },
    {
      key: 'score',
      label: 'Score',
      sortable: true,
      align: 'right' as const,
      render: (row: Row) => (row.kind === 'recall' ? row.score.toFixed(3) : ''),
    },
    {
      key: 'when',
      label: 'When',
      sortable: true,
      render: (row: Row) => (row.kind === 'recall' ? relTime(row.when) : ''),
    },
  ]

  let columns = $derived(mode === 'conversations' ? conversationColumns : recallColumns)

  function orDash(value: string): string {
    return value.trim() === '' ? '-' : value
  }

  function errorMessage(error: unknown): string {
    return error instanceof Error ? error.message : String(error)
  }

  function toConversationRow(summary: ConversationSummary): ConversationRow {
    return {
      kind: 'conversation',
      id: summary.id,
      name: summary.name,
      model: summary.model,
      status: summary.status,
      turns: summary.turns,
      inputTokens: summary.inputTokens,
      outputTokens: summary.outputTokens,
      costUsd: summary.totalCostUsd,
      createdAt: summary.createdAt ? timestampDate(summary.createdAt) : undefined,
      closedAt: summary.closedAt ? timestampDate(summary.closedAt) : undefined,
    }
  }

  function toRecallRow(hit: RecallHit): RecallRow {
    return {
      kind: 'recall',
      id: hit.id,
      source: hit.source,
      title: hit.title !== '' ? hit.title : hit.name,
      snippet: hit.snippet,
      conversationId: hit.conversationId,
      conversationName: hit.conversationName,
      repo: hit.repo,
      score: hit.score,
      when: hit.when ? timestampDate(hit.when) : undefined,
    }
  }

  async function loadLiveChildren(): Promise<Record<string, string>> {
    const resp = await control.listChildren({})
    const map: Record<string, string> = {}
    for (const child of resp.children) {
      if (child.status === 'exited' || child.sessionId === '') {
        continue
      }
      map[child.sessionId] = child.childId
    }
    return map
  }

  async function runSearch(): Promise<void> {
    const text = query.trim()
    if (text === '' || searching) {
      return
    }
    searching = true
    exportText = null
    exportTitle = ''
    try {
      const [live, found] = await Promise.all([
        loadLiveChildren().catch(() => ({}) as Record<string, string>),
        mode === 'conversations'
          ? control.conversationSearch({ text }).then((resp) => resp.rows.map(toConversationRow))
          : control.recall({ query: text }).then((resp) => resp.hits.map(toRecallRow)),
      ])
      liveChildren = live
      rows = found
    } catch (error) {
      rows = []
      pushToast({ kind: 'error', text: errorMessage(error) })
    } finally {
      searching = false
    }
  }

  function rowLabel(row: Row): string {
    return row.kind === 'conversation' ? orDash(row.name) : orDash(row.title)
  }

  function openRow(row: Row): void {
    const conversationId = row.kind === 'conversation' ? row.id : row.conversationId
    if (conversationId === '') {
      return
    }
    const childId = liveChildren[conversationId]
    if (childId !== undefined) {
      navigate(`#/c/${childId}`)
      return
    }
    void openExport(conversationId, rowLabel(row))
  }

  async function openExport(conversationId: string, title: string): Promise<void> {
    try {
      const resp = await control.conversationExport({ conversationId })
      exportTitle = title === '' ? conversationId : title
      exportText = renderExport(resp)
    } catch (error) {
      pushToast({ kind: 'error', text: errorMessage(error) })
    }
  }

  function renderExport(resp: ConversationExportResponse): string {
    const lines: string[] = []
    lines.push(`conversation ${resp.conversationId}`)
    lines.push(
      `owner ${orDash(resp.owner)} · persona ${orDash(resp.persona)} · ` +
        `source ${orDash(resp.source)} · driven_by ${orDash(resp.drivenBy)}`,
    )
    for (const turn of resp.turns) {
      lines.push('')
      const model = turn.model === '' ? '' : ` (${turn.model})`
      lines.push(`#${turn.ordinal} ${turn.role}${model}`)
      const body = renderContent(turn.content)
      if (body !== '') {
        lines.push(body)
      }
    }
    return lines.join('\n')
  }

  function renderContent(content: Uint8Array): string {
    const raw = new TextDecoder().decode(content)
    if (raw.trim() === '') {
      return ''
    }
    let parsed: unknown
    try {
      parsed = JSON.parse(raw)
    } catch {
      return raw
    }
    if (!Array.isArray(parsed)) {
      return raw
    }
    return parsed
      .map((block) => renderBlock(block))
      .filter((part) => part !== '')
      .join('\n')
  }

  function renderBlock(block: unknown): string {
    if (typeof block !== 'object' || block === null) {
      return JSON.stringify(block) ?? ''
    }
    const typed = block as Record<string, unknown>
    const type = typeof typed.type === 'string' ? typed.type : ''
    switch (type) {
      case 'text':
        return typeof typed.text === 'string' ? typed.text : ''
      case 'thinking':
        return typeof typed.thinking === 'string' ? `[thinking] ${typed.thinking}` : ''
      case 'tool_use':
        return `[tool_use ${String(typed.name ?? '')}] ${JSON.stringify(typed.input ?? null)}`
      case 'tool_result':
        return `[tool_result] ${renderContentBlocks(typed.content)}`
      case 'image':
        return '[image]'
      default:
        return JSON.stringify(block) ?? ''
    }
  }

  function renderContentBlocks(content: unknown): string {
    if (typeof content === 'string') {
      return content
    }
    if (Array.isArray(content)) {
      return content
        .map((part) => renderBlock(part))
        .filter((part) => part !== '')
        .join('\n')
    }
    return ''
  }

  function selectMode(next: Mode): void {
    if (mode === next) {
      return
    }
    mode = next
    rows = []
    exportText = null
    exportTitle = ''
  }
</script>

<h1>Search</h1>

<div class="flex flex-wrap items-center gap-2">
  <div class="tabs tabs-boxed">
    <button
      type="button"
      class="tab {mode === 'conversations' ? 'tab-active' : ''}"
      onclick={() => selectMode('conversations')}
    >
      Conversations
    </button>
    <button
      type="button"
      class="tab {mode === 'recall' ? 'tab-active' : ''}"
      onclick={() => selectMode('recall')}
    >
      Recall
    </button>
  </div>
  <input
    type="text"
    class="input input-bordered grow"
    placeholder={mode === 'conversations' ? 'Search conversations' : 'Search recall'}
    bind:value={query}
    onkeydown={(event) => {
      if (event.key === 'Enter') {
        event.preventDefault()
        void runSearch()
      }
    }}
  />
  {#if searching}
    <span class="text-sm opacity-70">Searching…</span>
  {/if}
</div>

<div class="mt-4">
  <DataTable
    {columns}
    {rows}
    rowKey={(row) => `${row.kind}:${row.id}`}
    onrowclick={(row) => openRow(row)}
  />
</div>

{#if exportText !== null}
  <section class="mt-4">
    <div class="flex items-center justify-between">
      <h2 class="text-lg font-bold">{exportTitle}</h2>
      <button
        type="button"
        class="btn btn-sm"
        onclick={() => {
          exportText = null
          exportTitle = ''
        }}
      >
        Close
      </button>
    </div>
    <pre class="mt-2 overflow-x-auto rounded bg-base-200 p-3 text-xs whitespace-pre-wrap">{exportText}</pre>
  </section>
{/if}
