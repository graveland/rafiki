<script lang="ts">
  import type { ContentBlock } from '../../gen/rafiki/v1/event_pb'
  import type { TranscriptBlock } from '../transcript.svelte'
  import ToolCall from './ToolCall.svelte'

  let { block }: { block: TranscriptBlock } = $props()

  function base64(data: Uint8Array): string {
    let binary = ''
    for (const byte of data) {
      binary += String.fromCharCode(byte)
    }
    return btoa(binary)
  }

  function imageUrl(blockValue: ContentBlock): string {
    if (blockValue.block.case !== 'image') {
      return ''
    }
    return `data:${blockValue.block.value.mediaType};base64,${base64(blockValue.block.value.data)}`
  }

  let images = $derived(block.content.filter((item) => item.block.case === 'image'))
</script>

<div class="rounded-lg border border-base-300 p-3">
  <div class="text-xs uppercase opacity-60">{block.kind}</div>
  {#if block.thinking !== ''}
    <details class="mt-1">
      <summary class="cursor-pointer text-xs opacity-70">thinking</summary>
      <pre class="mt-1 whitespace-pre-wrap text-sm opacity-80">{block.thinking}</pre>
    </details>
  {/if}
  {#if block.text !== ''}
    <p class="mt-1 whitespace-pre-wrap">{block.text}</p>
  {/if}
  {#each images as image, index (index)}
    <img class="mt-1 max-h-64" src={imageUrl(image)} alt="attachment" />
  {/each}
  {#each block.toolCalls as call (call.id)}
    <div class="mt-2">
      <ToolCall {call} />
    </div>
  {/each}
</div>
