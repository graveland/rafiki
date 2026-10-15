<script lang="ts">
  import ErrorBanner from '../lib/components/ErrorBanner.svelte'
  import SplitPanes from '../lib/components/SplitPanes.svelte'
  import TranscriptPane from '../lib/components/TranscriptPane.svelte'
  import { route, transcriptPanes } from '../lib/router'
  import type { Snippet } from 'svelte'

  let selection = $derived(transcriptPanes($route.ids))

  // The public Snippet type hides the internal anchor argument that {@render}
  // passes at runtime, so binding an id into a zero-arg pane snippet needs one
  // cast. https://svelte.dev/docs/svelte/snippet
  function bound(snippet: unknown, id: string): Snippet {
    const fn = snippet as (anchor: Node, id: string) => unknown
    return ((anchor: Node) => fn(anchor, id)) as unknown as Snippet
  }
</script>

{#snippet pane(id: string)}
  <TranscriptPane {id} />
{/snippet}

<div class="flex h-[calc(100vh-4rem)] flex-col">
  {#if selection.overflow}
    <div class="p-2">
      <ErrorBanner
        title="Too many panes"
        message="At most 3 live transcript panes are shown."
      />
    </div>
  {/if}
  <div class="min-h-0 flex-1">
    <SplitPanes panes={selection.shown.map((id) => ({ id, snippet: bound(pane, id) }))} />
  </div>
</div>
