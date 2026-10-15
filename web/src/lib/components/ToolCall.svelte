<script lang="ts">
  import type { ToolCallView } from '../transcript.svelte'

  let { call }: { call: ToolCallView } = $props()

  interface DiffLine {
    kind: 'add' | 'del' | 'hunk' | 'ctx'
    text: string
  }

  function looksLikeDiff(text: string): boolean {
    const lines = text.split('\n')
    let adds = 0
    let dels = 0
    for (const line of lines) {
      if (line.startsWith('+++') || line.startsWith('---')) {
        return true
      }
      if (line.startsWith('+')) {
        adds += 1
      } else if (line.startsWith('-')) {
        dels += 1
      }
    }
    return adds > 0 && dels > 0
  }

  function diffLines(text: string): DiffLine[] {
    return text.split('\n').map((line) => {
      if (line.startsWith('@@')) {
        return { kind: 'hunk' as const, text: line }
      }
      if (line.startsWith('+')) {
        return { kind: 'add' as const, text: line }
      }
      if (line.startsWith('-')) {
        return { kind: 'del' as const, text: line }
      }
      return { kind: 'ctx' as const, text: line }
    })
  }

  function formatInput(json: string): string {
    if (json === '') {
      return '(no input)'
    }
    try {
      return JSON.stringify(JSON.parse(json), null, 2)
    } catch {
      return json
    }
  }

  let resultIsDiff = $derived(looksLikeDiff(call.resultText))
  let resultLines = $derived(diffLines(call.resultText))
</script>

<div class="rounded border border-base-300 p-2 text-sm">
  <div class="flex items-center gap-2">
    <span class="font-mono font-semibold">{call.name === '' ? 'tool' : call.name}</span>
    {#if call.running}
      <span class="loading loading-spinner loading-xs"></span>
    {:else if call.durationMs !== undefined}
      <span class="text-xs opacity-60">{call.durationMs}ms</span>
    {/if}
    {#if call.hasResult && call.isError}
      <span class="badge badge-error badge-sm">error</span>
    {/if}
  </div>
  {#if call.inputJson !== ''}
    <details class="mt-1">
      <summary class="cursor-pointer text-xs opacity-70">input</summary>
      <pre class="mt-1 overflow-auto rounded bg-base-200 p-2 text-xs">{formatInput(call.inputJson)}</pre>
    </details>
  {/if}
  {#if call.hasResult}
    <details class="mt-1" open>
      <summary class="cursor-pointer text-xs opacity-70">result</summary>
      {#if resultIsDiff}
        <pre class="mt-1 overflow-auto rounded bg-base-200 p-2 text-xs">{#each resultLines as line, index (index)}<span
              class:bg-success={line.kind === 'add'}
              class:bg-error={line.kind === 'del'}
              class:text-info={line.kind === 'hunk'}
              class="block">{line.text}</span
            >{/each}</pre>
      {:else}
        <pre class="mt-1 overflow-auto rounded bg-base-200 p-2 text-xs">{call.resultText}</pre>
      {/if}
    </details>
  {/if}
</div>
