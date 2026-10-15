<script lang="ts">
  import { onMount } from 'svelte'
  import { ConnectError } from '@connectrpc/connect'
  import ConfirmDialog from '../lib/components/ConfirmDialog.svelte'
  import DataTable from '../lib/components/DataTable.svelte'
  import { control } from '../lib/client'
  import { usd, relTime } from '../lib/format'
  import { navigate } from '../lib/router'
  import { children as childRows, startRail, stopRail, type ChildRow } from '../lib/rail.svelte'
  import { pushToast } from '../lib/toasts'

  type DialogKind = 'kill' | 'resume' | 'close' | 'budget' | 'labels' | 'close-all-exited'

  const RESERVED_PREFIX = 'rafiki/'

  let filter = $state('')
  let dialog = $state<DialogKind | null>(null)
  let target = $state<ChildRow | null>(null)
  let budgetInput = $state('')
  let labelsInput = $state('')

  const rows = $derived([...childRows.values()])

  // The set-budget dialog's value. max_cost 0 is the daemon's documented
  // "unlimited" sentinel (control.proto SetBudgetRequest), so an EMPTY field —
  // Number('') === 0 — must never be read as an accidental unlimited. Require a
  // typed value: the Set button is disabled (and confirmDialog re-checks) until
  // the input is a non-negative number, and a typed 0 is labelled "unlimited".
  const budgetValue = $derived(Number(budgetInput))
  const budgetValid = $derived(
    budgetInput.trim() !== '' && Number.isFinite(budgetValue) && budgetValue >= 0,
  )

  onMount(() => {
    startRail()
    return () => stopRail()
  })

  function depthOf(row: ChildRow): number {
    let depth = 0
    let parent = row.parentId
    while (parent) {
      const ancestor = childRows.get(parent)
      if (!ancestor) {
        break
      }
      depth++
      parent = ancestor.parentId
    }
    return depth
  }

  function labelsText(row: ChildRow): string {
    return Object.entries(row.labels)
      .map(([key, value]) => `${key}=${value}`)
      .join(', ')
  }

  // Only the operator's own labels are editable: keys under rafiki/ are
  // daemon-written and refused by SetLabels.
  function editableLabels(row: ChildRow): string {
    return Object.entries(row.labels)
      .filter(([key]) => !key.startsWith(RESERVED_PREFIX))
      .map(([key, value]) => `${key}=${value}`)
      .join('\n')
  }

  function parseLabels(
    text: string,
    row: ChildRow,
  ): { set: { [key: string]: string }; remove: string[] } {
    const set: { [key: string]: string } = {}
    for (const line of text.split('\n')) {
      const trimmed = line.trim()
      if (trimmed === '') {
        continue
      }
      const eq = trimmed.indexOf('=')
      if (eq <= 0) {
        continue
      }
      const key = trimmed.slice(0, eq).trim()
      if (key === '' || key.startsWith(RESERVED_PREFIX)) {
        continue
      }
      set[key] = trimmed.slice(eq + 1).trim()
    }
    const remove = Object.keys(row.labels).filter(
      (key) => !key.startsWith(RESERVED_PREFIX) && !(key in set),
    )
    return { set, remove }
  }

  function ask(kind: DialogKind, row?: ChildRow): void {
    dialog = kind
    target = row ?? null
    if (kind === 'budget' && row) {
      budgetInput = row.maxCost !== undefined ? String(row.maxCost) : ''
    }
    if (kind === 'labels' && row) {
      labelsInput = editableLabels(row)
    }
  }

  function closeDialog(): void {
    dialog = null
    target = null
  }

  function errorText(error: unknown): string {
    if (error instanceof ConnectError) {
      return error.rawMessage || error.message
    }
    if (error instanceof Error) {
      return error.message
    }
    return String(error)
  }

  // Kill / close / set-budget / set-labels are safe to repeat: retry once
  // automatically on a network error, then surface the failure. Resume is NOT
  // in the binding retry-safe set, so it gets a single attempt.
  async function withRetry(
    label: string,
    run: () => Promise<unknown>,
    tries = 2,
  ): Promise<boolean> {
    for (let attempt = 0; attempt < tries; attempt++) {
      try {
        await run()
        return true
      } catch (error) {
        if (attempt === tries - 1) {
          pushToast({ kind: 'error', text: `${label} failed: ${errorText(error)}` })
          return false
        }
      }
    }
    return false
  }

  async function confirmDialog(): Promise<void> {
    const row = target
    switch (dialog) {
      case 'kill': {
        if (row && (await withRetry('kill', () => control.kill({ childId: row.id })))) {
          closeDialog()
        }
        return
      }
      case 'resume': {
        if (
          row &&
          (await withRetry('resume', () => control.resume({ childId: row.id }), 1))
        ) {
          closeDialog()
        }
        return
      }
      case 'close': {
        if (row && (await withRetry('close', () => control.close({ childId: row.id })))) {
          closeDialog()
        }
        return
      }
      case 'budget': {
        // Re-check here as well as on the disabled button: an empty field is not
        // a valid budget (0 = unlimited must be typed, not defaulted).
        if (!budgetValid) {
          pushToast({
            kind: 'error',
            text: 'budget must be a non-negative number (0 means unlimited)',
          })
          return
        }
        const value = budgetValue
        if (
          row &&
          (await withRetry('set budget', () =>
            control.setBudget({ childId: row.id, maxCost: value }),
          ))
        ) {
          closeDialog()
        }
        return
      }
      case 'labels': {
        if (!row) {
          return
        }
        const { set, remove } = parseLabels(labelsInput, row)
        if (
          await withRetry('set labels', () => control.setLabels({ childId: row.id, set, remove }))
        ) {
          closeDialog()
        }
        return
      }
      case 'close-all-exited': {
        if (await withRetry('close all exited', () => control.closeAllExited({}))) {
          closeDialog()
        }
        return
      }
      default:
        return
    }
  }

  function dialogTitle(kind: DialogKind): string {
    switch (kind) {
      case 'kill':
        return 'Kill child'
      case 'resume':
        return 'Resume child'
      case 'close':
        return 'Close child'
      case 'budget':
        return 'Set budget'
      case 'labels':
        return 'Set labels'
      case 'close-all-exited':
        return 'Close all exited'
    }
  }

  function dialogVerb(kind: DialogKind): string {
    switch (kind) {
      case 'kill':
        return 'Kill'
      case 'resume':
        return 'Resume'
      case 'close':
        return 'Close'
      default:
        return ''
    }
  }
</script>

{#snippet actionsCell(row: ChildRow)}
  <details class="dropdown dropdown-end">
    <summary class="btn btn-ghost btn-xs" onclick={(event) => event.stopPropagation()}>⋯</summary>
    <ul class="dropdown-content menu z-10 w-44 rounded-box bg-base-100 p-2 shadow">
      <li>
        <button
          type="button"
          onclick={(event) => {
            event.stopPropagation()
            ask('resume', row)
          }}
        >
          Resume
        </button>
      </li>
      <li>
        <button
          type="button"
          onclick={(event) => {
            event.stopPropagation()
            ask('budget', row)
          }}
        >
          Set budget
        </button>
      </li>
      <li>
        <button
          type="button"
          onclick={(event) => {
            event.stopPropagation()
            ask('labels', row)
          }}
        >
          Set labels
        </button>
      </li>
      <li>
        <button
          type="button"
          onclick={(event) => {
            event.stopPropagation()
            ask('kill', row)
          }}
        >
          Kill
        </button>
      </li>
      <li>
        <button
          type="button"
          onclick={(event) => {
            event.stopPropagation()
            ask('close', row)
          }}
        >
          Close
        </button>
      </li>
    </ul>
  </details>
{/snippet}

<div class="flex items-center gap-2 py-2">
  <input type="text" class="input input-sm w-64" placeholder="Filter agents" bind:value={filter} />
  <button type="button" class="btn btn-sm btn-error" onclick={() => ask('close-all-exited')}>
    Close all exited
  </button>
</div>

<DataTable
  columns={[
    { key: 'name', label: 'Name', sortable: true },
    { key: 'status', label: 'State', sortable: true },
    { key: 'kind', label: 'Kind', sortable: true },
    { key: 'model', label: 'Model' },
    { key: 'cost', label: 'Cost', align: 'right', render: (row: ChildRow) => usd(row.costUsd) },
    { key: 'budget', label: 'Budget', align: 'right', render: (row: ChildRow) => usd(row.maxCost) },
    { key: 'started', label: 'Started', render: (row: ChildRow) => relTime(row.startedAt) },
    { key: 'labels', label: 'Labels', render: (row: ChildRow) => labelsText(row) },
    { key: 'actions', label: '', align: 'right', cell: actionsCell },
  ]}
  rows={rows}
  rowKey={(row) => row.id}
  indent={depthOf}
  filterText={filter}
  onrowclick={(row) => navigate(`#/c/${row.id}`)}
/>

{#if dialog === 'kill' || dialog === 'resume' || dialog === 'close'}
  <ConfirmDialog
    title={dialogTitle(dialog)}
    body={`${dialogVerb(dialog)} ${target?.name ?? ''} (${target?.id ?? ''})?`}
    confirmLabel={dialogTitle(dialog)}
    danger={dialog === 'kill' || dialog === 'close'}
    onconfirm={() => void confirmDialog()}
    oncancel={closeDialog}
  />
{:else if dialog === 'close-all-exited'}
  <ConfirmDialog
    title="Close all exited"
    body="Close every exited child? This cannot be undone."
    confirmLabel="Close all exited"
    danger={true}
    onconfirm={() => void confirmDialog()}
    oncancel={closeDialog}
  />
{:else if dialog === 'budget'}
  <div class="modal modal-open">
    <div class="modal-box">
      <h3 class="text-lg font-bold">Set budget</h3>
      <p class="py-2 text-sm opacity-70">Spend cap in USD for {target?.name ?? ''}.</p>
      <input type="number" min="0" step="0.01" class="input w-full" bind:value={budgetInput} />
      <p class="py-1 text-xs opacity-70">
        {#if budgetValid && budgetValue === 0}
          Unlimited — no spend cap.
        {:else if !budgetValid}
          Enter a non-negative number; 0 means unlimited.
        {:else}
          0 means unlimited.
        {/if}
      </p>
      <div class="modal-action">
        <button type="button" class="btn" onclick={closeDialog}>Cancel</button>
        <button
          type="button"
          class="btn btn-primary"
          disabled={!budgetValid}
          onclick={() => void confirmDialog()}
        >
          Set
        </button>
      </div>
    </div>
  </div>
{:else if dialog === 'labels'}
  <div class="modal modal-open">
    <div class="modal-box">
      <h3 class="text-lg font-bold">Set labels</h3>
      <p class="py-2 text-sm opacity-70">One key=value per line. Keys under rafiki/ are reserved.</p>
      <textarea class="textarea w-full" rows="6" bind:value={labelsInput}></textarea>
      <div class="modal-action">
        <button type="button" class="btn" onclick={closeDialog}>Cancel</button>
        <button type="button" class="btn btn-primary" onclick={() => void confirmDialog()}>Set</button>
      </div>
    </div>
  </div>
{/if}
