<script lang="ts">
  import type { Snippet } from 'svelte'

  interface Pane {
    id: string
    snippet: Snippet
  }

  let { panes }: { panes: Pane[] } = $props()

  const MIN_FRACTION = 0.1

  let containerEl = $state<HTMLDivElement | null>(null)
  let sizes = $state<number[]>([])
  let dragging = $state<number | null>(null)
  let startX = 0
  let startSizes: number[] = []

  $effect(() => {
    if (sizes.length !== panes.length) {
      sizes =
        panes.length === 0 ? [] : Array.from({ length: panes.length }, () => 1 / panes.length)
    }
  })

  function startDrag(index: number, event: PointerEvent): void {
    event.preventDefault()
    dragging = index
    startX = event.clientX
    startSizes = [...sizes]
  }

  function onDragMove(event: PointerEvent): void {
    if (dragging === null || !containerEl) {
      return
    }
    const width = containerEl.getBoundingClientRect().width
    if (width === 0) {
      return
    }
    const delta = (event.clientX - startX) / width
    const left = startSizes[dragging] + delta
    const right = startSizes[dragging + 1] - delta
    if (left < MIN_FRACTION || right < MIN_FRACTION) {
      return
    }
    const next = [...sizes]
    next[dragging] = left
    next[dragging + 1] = right
    sizes = next
  }

  function endDrag(): void {
    dragging = null
  }
</script>

<svelte:window onpointermove={onDragMove} onpointerup={endDrag} />

<div class="flex h-full w-full" bind:this={containerEl}>
  {#each panes as pane, index (pane.id)}
    <div class="min-w-0 overflow-auto" style="flex: {sizes[index] ?? 1 / panes.length} 1 0%">
      {@render pane.snippet()}
    </div>
    {#if index < panes.length - 1}
      <div
        class="w-1 shrink-0 cursor-col-resize bg-base-300 hover:bg-primary"
        role="separator"
        aria-orientation="vertical"
        onpointerdown={(event) => startDrag(index, event)}
      ></div>
    {/if}
  {/each}
</div>
