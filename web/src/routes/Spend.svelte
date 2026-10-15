<script lang="ts">
  import * as Plot from '@observablehq/plot'
  import { timestampFromDate } from '@bufbuild/protobuf/wkt'
  import { control } from '../lib/client'
  import { tokens, usd } from '../lib/format'
  import {
    byGroup,
    decodeRows,
    legendEntries,
    soloKey,
    toggleKey,
    totals,
    visibleRows,
    type GroupKey,
    type SpendRow,
  } from '../lib/spend'
  import { pushToast } from '../lib/toasts'

  type Range = 7 | 30 | 90

  const COLOR_SCHEME = 'observable10'

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
  // Per grouping, so hiding a model doesn't hide a provider of the same name.
  let hiddenBy = $state<Record<GroupKey, Set<string>>>({ model: new Set(), provider: new Set() })

  const hidden = $derived(hiddenBy[groupKey])
  const legend = $derived(legendEntries(rows, groupKey))
  const legendKeys = $derived(legend.map((entry) => entry.key))
  // The domain is every series, hidden or not, so a series keeps its color
  // whatever else is toggled off.
  const colorScale = $derived(Plot.scale({ color: { domain: legendKeys, scheme: COLOR_SCHEME } }))
  const shown = $derived(visibleRows(rows, groupKey, hidden))
  const filtered = $derived(legendKeys.some((key) => hidden.has(key)))

  const summary = $derived(totals(shown))
  const turnsTotal = $derived(shown.reduce((sum, row) => sum + row.turns, 0))
  const tokensTotal = $derived(
    shown.reduce((sum, row) => sum + row.inputTokens + row.outputTokens, 0),
  )

  function onLegendClick(event: MouseEvent, key: string): void {
    hiddenBy[groupKey] = event.shiftKey ? soloKey(hidden, legendKeys, key) : toggleKey(hidden, key)
  }

  function showAll(): void {
    hiddenBy[groupKey] = new Set()
  }

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
    const points = byGroup(shown, groupKey)
    const domain = legendKeys
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
        color: { domain, scheme: COLOR_SCHEME },
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
    <div class="text-xs opacity-70">Total cost{#if filtered}<span class="badge badge-xs ml-1">filtered</span>{/if}</div>
    <div class="text-lg font-bold">{usd(summary.costUsd)}</div>
  </div>
  <div class="rounded bg-base-200 p-3">
    <div class="text-xs opacity-70">Turns{#if filtered}<span class="badge badge-xs ml-1">filtered</span>{/if}</div>
    <div class="text-lg font-bold">{turnsTotal === 0 ? '-' : turnsTotal}</div>
  </div>
  <div class="rounded bg-base-200 p-3">
    <div class="text-xs opacity-70">Tokens{#if filtered}<span class="badge badge-xs ml-1">filtered</span>{/if}</div>
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

{#if legend.length > 0}
  <div class="mt-4 flex flex-wrap items-center gap-1" role="group" aria-label="series">
    {#each legend as entry (entry.key)}
      {@const off = hidden.has(entry.key)}
      <button
        type="button"
        class="btn btn-ghost btn-xs normal-case {off ? 'line-through opacity-40' : ''}"
        aria-pressed={!off}
        title="click to show/hide; shift-click to show only this"
        onclick={(event) => onLegendClick(event, entry.key)}
      >
        <span class="inline-block h-3 w-3 rounded-sm" style:background-color={colorScale.apply(entry.key)}></span>
        {entry.key}
        <span class="opacity-60">{usd(entry.costUsd, entry.priced)}</span>
      </button>
    {/each}
    {#if filtered}
      <button type="button" class="btn btn-link btn-xs" onclick={showAll}>show all</button>
    {/if}
  </div>
{/if}

<div class="mt-2" bind:this={chartEl}></div>

{#if summary.anyUnpriced}
  <p class="mt-2 text-sm opacity-70">some models have no price; their cost is not shown</p>
{/if}
