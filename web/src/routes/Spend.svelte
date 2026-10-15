<script lang="ts">
  import * as Plot from '@observablehq/plot'
  import { timestampFromDate } from '@bufbuild/protobuf/wkt'
  import { control } from '../lib/client'
  import { tokens, usd } from '../lib/format'
  import { byGroup, decodeRows, totals, type GroupKey, type SpendRow } from '../lib/spend'
  import { pushToast } from '../lib/toasts'

  type Range = 7 | 30 | 90

  interface StatsJson {
    tokens?: { input_tokens?: number; output_tokens?: number }
    failures?: { error_rate?: number }
  }

  interface QuotaView {
    fiveH?: number
    sevenD?: number
    status: string
  }

  let range = $state<Range>(7)
  let groupKey = $state<GroupKey>('model')
  let rows = $state<SpendRow[]>([])
  let stats = $state<StatsJson | undefined>(undefined)
  let quota = $state<QuotaView | undefined>(undefined)
  let loading = $state(false)
  let chartEl: HTMLDivElement | undefined = $state(undefined)

  const summary = $derived(totals(rows))
  const turnsTotal = $derived(rows.reduce((sum, row) => sum + row.turns, 0))
  const tokensTotal = $derived(
    rows.reduce((sum, row) => sum + row.inputTokens + row.outputTokens, 0),
  )

  function sinceFor(days: Range): Date {
    return new Date(Date.now() - days * 86_400_000)
  }

  function errorMessage(error: unknown): string {
    return error instanceof Error ? error.message : String(error)
  }

  function parseStats(json: string): StatsJson | undefined {
    if (json === '') {
      return undefined
    }
    try {
      return JSON.parse(json) as StatsJson
    } catch {
      return undefined
    }
  }

  function utilization(value?: number): string {
    if (value === undefined) {
      return '-'
    }
    return value <= 1 ? `${Math.round(value * 100)}%` : `${value}`
  }

  async function load(days: Range): Promise<void> {
    loading = true
    try {
      const since = timestampFromDate(sinceFor(days))
      const [spend, statsResp, quotaResp] = await Promise.all([
        control.conversationQuery({ name: 'spend', since }),
        control.conversationStats({ since }).catch(() => undefined),
        control.getRateLimitStatus({}).catch(() => undefined),
      ])
      rows = decodeRows(spend)
      stats = statsResp === undefined ? undefined : parseStats(statsResp.statsJson)
      quota =
        quotaResp === undefined
          ? undefined
          : {
              fiveH: quotaResp.fiveH?.utilization,
              sevenD: quotaResp.sevenD?.utilization,
              status: quotaResp.overallStatus,
            }
    } catch (error) {
      rows = []
      stats = undefined
      quota = undefined
      pushToast({ kind: 'error', text: errorMessage(error) })
    } finally {
      loading = false
    }
  }

  $effect(() => {
    void load(range)
  })

  $effect(() => {
    const el = chartEl
    const points = byGroup(rows, groupKey)
    if (el === undefined) {
      return
    }
    el.replaceChildren(
      Plot.plot({
        width: el.clientWidth || 640,
        height: 320,
        marginLeft: 60,
        x: { label: 'day' },
        y: { label: 'cost (USD)', grid: true },
        color: { legend: true },
        marks: [Plot.barY(points, { x: 'day', y: 'costUsd', fill: 'key' }), Plot.ruleY([0])],
      }),
    )
  })
</script>

<h1>Spend</h1>

<div class="flex flex-wrap items-center gap-2">
  <div class="join">
    {#each [7, 30, 90] as const as days (days)}
      <button
        type="button"
        class="btn join-item btn-sm {range === days ? 'btn-active' : ''}"
        onclick={() => (range = days)}
      >
        {days}d
      </button>
    {/each}
  </div>
  <div class="join">
    {#each ['model', 'provider'] as const as key (key)}
      <button
        type="button"
        class="btn join-item btn-sm {groupKey === key ? 'btn-active' : ''}"
        onclick={() => (groupKey = key)}
      >
        {key}
      </button>
    {/each}
  </div>
  {#if loading}
    <span class="text-sm opacity-70">Loading…</span>
  {/if}
</div>

<div class="mt-4 grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-6">
  <div class="rounded bg-base-200 p-3">
    <div class="text-xs opacity-70">Total cost</div>
    <div class="text-lg font-bold">{usd(summary.costUsd)}</div>
  </div>
  <div class="rounded bg-base-200 p-3">
    <div class="text-xs opacity-70">Turns</div>
    <div class="text-lg font-bold">{turnsTotal === 0 ? '-' : turnsTotal}</div>
  </div>
  <div class="rounded bg-base-200 p-3">
    <div class="text-xs opacity-70">Tokens</div>
    <div class="text-lg font-bold">{tokens(tokensTotal === 0 ? undefined : tokensTotal)}</div>
  </div>
  <div class="rounded bg-base-200 p-3">
    <div class="text-xs opacity-70">Input tokens</div>
    <div class="text-lg font-bold">{tokens(stats?.tokens?.input_tokens)}</div>
  </div>
  <div class="rounded bg-base-200 p-3">
    <div class="text-xs opacity-70">Output tokens</div>
    <div class="text-lg font-bold">{tokens(stats?.tokens?.output_tokens)}</div>
  </div>
  <div class="rounded bg-base-200 p-3">
    <div class="text-xs opacity-70">Error rate</div>
    <div class="text-lg font-bold">
      {stats?.failures?.error_rate === undefined
        ? '-'
        : `${(stats.failures.error_rate * 100).toFixed(1)}%`}
    </div>
  </div>
  <div class="rounded bg-base-200 p-3">
    <div class="text-xs opacity-70">Quota 5h</div>
    <div class="text-lg font-bold">{utilization(quota?.fiveH)}</div>
  </div>
  <div class="rounded bg-base-200 p-3">
    <div class="text-xs opacity-70">Quota 7d</div>
    <div class="text-lg font-bold">{utilization(quota?.sevenD)}</div>
  </div>
  <div class="rounded bg-base-200 p-3">
    <div class="text-xs opacity-70">Quota status</div>
    <div class="text-lg font-bold">{quota?.status ? quota.status : '-'}</div>
  </div>
</div>

<div class="mt-4" bind:this={chartEl}></div>

{#if summary.anyUnpriced}
  <p class="mt-2 text-sm opacity-70">some models have no price; their cost is not shown</p>
{/if}
