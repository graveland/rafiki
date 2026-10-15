<script lang="ts">
  import SplitPanes from './SplitPanes.svelte'
  import { ids } from './paneHarness.svelte'
</script>

<!--
  Mirrors Transcript.svelte's contract: ONE stable snippet passed to every pane,
  rendered by SplitPanes as `{@render pane.snippet(id)}`.
-->
{#snippet pane(id: string)}
  <span data-pane={id}>{id}</span>
{/snippet}

<SplitPanes panes={ids.map((id) => ({ id, snippet: pane }))} />
