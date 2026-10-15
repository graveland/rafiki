<script lang="ts">
  import ErrorBanner from '../lib/components/ErrorBanner.svelte'
  import SplitPanes from '../lib/components/SplitPanes.svelte'
  import TranscriptPane from '../lib/components/TranscriptPane.svelte'
  import { route, transcriptPanes } from '../lib/router'

  let selection = $derived(transcriptPanes($route.ids))
</script>

<!--
  ONE stable snippet, passed to every pane and called by SplitPanes as
  `{@render pane.snippet(id)}`. The render tag wraps the id argument as a getter
  (prod snippet params are getters), so passing a fresh bound closure per id —
  which also tears the pane's each-block down on every route change — is wrong.
-->
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
    <SplitPanes panes={selection.shown.map((id) => ({ id, snippet: pane }))} />
  </div>
</div>
