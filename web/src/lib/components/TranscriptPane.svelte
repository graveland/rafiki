<script lang="ts">
  import { untrack } from 'svelte'
  import { control } from '../client'
  import { usd } from '../format'
  import { navigate, route } from '../router'
  import { Conversation } from '../transcript.svelte'
  import { SendQueue, type SendModeName } from '../sendQueue.svelte'
  import MessageBlock from './MessageBlock.svelte'

  let { id }: { id: string } = $props()

  // The pane is keyed by id, so the conversation is built once with the
  // initial id and untracked to satisfy state_referenced_locally.
  const childId = untrack(() => id)
  const conversation = new Conversation(childId, { control })
  const queue = new SendQueue(childId, { control })

  let draft = $state('')
  let deliveredIndex = 0

  $effect(() => {
    void conversation.open()
    return () => conversation.dispose()
  })

  $effect(() => {
    const onVisibility = (): void => {
      if (document.visibilityState === 'hidden') {
        conversation.pause()
      } else {
        conversation.resume()
      }
    }
    document.addEventListener('visibilitychange', onVisibility)
    return () => document.removeEventListener('visibilitychange', onVisibility)
  })

  $effect(() => {
    const blocks = conversation.blocks
    while (deliveredIndex < blocks.length) {
      const block = blocks[deliveredIndex]
      if (block.kind === 'user') {
        queue.markDelivered(block.text)
      }
      deliveredIndex += 1
    }
  })

  function submit(mode: SendModeName): void {
    const text = draft.trim()
    if (text === '') {
      return
    }
    void queue.send(text, mode)
    if (mode === 'prompt') {
      draft = ''
    }
  }

  function onKeydown(event: KeyboardEvent): void {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault()
      submit('prompt')
    }
  }

  function addChild(): void {
    const next = prompt('Child id')
    if (!next) {
      return
    }
    navigate(`#/c/${[...$route.ids, next].join(',')}`)
  }
</script>

<div class="flex h-full flex-col">
  <header class="flex flex-wrap items-center gap-2 border-b border-base-300 p-2 text-sm">
    <span class="font-semibold">{conversation.child?.name || id}</span>
    <span class="badge badge-sm">{conversation.status || conversation.child?.status || '-'}</span>
    <span class="opacity-70">{conversation.child?.model || '-'}</span>
    <span class="opacity-70">{usd(conversation.child?.costUsd)}</span>
    {#if $route.ids.length === 1}
      <button type="button" class="btn btn-xs ml-auto" aria-label="Add child" onclick={addChild}>+</button>
    {/if}
  </header>

  {#if conversation.error}
    <div class="alert alert-error m-2 text-sm">{conversation.error}</div>
  {/if}

  <div class="flex-1 space-y-2 overflow-auto p-2">
    {#each conversation.blocks as block, index (`${index}-${block.kind}`)}
      {#if block.kind === 'system'}
        <div class="text-sm italic opacity-70">{block.text}</div>
      {:else}
        <MessageBlock {block} />
      {/if}
    {/each}
    {#if conversation.turnCostUsd !== undefined}
      <div class="text-right text-xs opacity-50">{usd(conversation.turnCostUsd)} this turn</div>
    {/if}
  </div>

  <div class="border-t border-base-300 p-2">
    <textarea
      class="textarea textarea-bordered w-full"
      rows="2"
      aria-label="Message"
      placeholder="Message…"
      bind:value={draft}
      onkeydown={onKeydown}
    ></textarea>
    <div class="mt-1 flex gap-2">
      <button type="button" class="btn btn-sm btn-primary" onclick={() => submit('prompt')}>Send</button>
      <button type="button" class="btn btn-sm" onclick={() => submit('steer')}>Steer</button>
      <button type="button" class="btn btn-sm btn-error" onclick={() => submit('abort')}>Abort</button>
    </div>
    <ul class="mt-2 space-y-1 text-xs">
      {#each queue.entries as entry (entry.id)}
        <li class="flex items-center gap-2">
          <span class="badge badge-sm">{entry.status}</span>
          <span class="truncate opacity-80">{entry.text}</span>
          {#if entry.status === 'failed'}
            <span class="text-error">{entry.error}</span>
            <button
              type="button"
              class="btn btn-xs"
              onclick={() => void queue.resend(entry.id)}
            >
              Resend
            </button>
          {/if}
        </li>
      {/each}
    </ul>
  </div>
</div>
