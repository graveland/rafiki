import { flushSync, tick } from 'svelte'
import { expect, it, vi } from 'vitest'
import { SendQueue } from './sendQueue.svelte'
import type { ControlClient } from './transcript.svelte'

function fakeControl(send: (input: unknown) => Promise<{ messageId: string }>) {
  return { send } as unknown as ControlClient
}

// Runes only compile in a `.svelte.ts`-infixed module, so this reactivity test
// lives beside sendQueue.test.ts in its own module. An $effect is the only way
// to observe a $state mutation, and the whole point of the fix is that dispatch
// must mutate the PROXIED entry so such an effect sees sending -> queued.
it('a dispatch status change is visible through the state proxy', async () => {
  let release: () => void = () => {}
  const send = vi.fn(
    () =>
      new Promise<{ messageId: string }>((resolve) => {
        release = () => resolve({ messageId: 'm1' })
      }),
  )
  const queue = new SendQueue('c', { control: fakeControl(send) })
  const seen: string[] = []
  const stop = $effect.root(() => {
    $effect(() => {
      const entry = queue.entries[0]
      seen.push(entry ? entry.status : 'none')
    })
  })
  try {
    flushSync()
    const done = queue.send('hi', 'prompt')
    await Promise.resolve()
    flushSync()
    expect(seen).toContain('sending')

    release()
    await done
    flushSync()
    await tick()
    // Pre-fix, dispatch mutated the raw object pushed into the array, so the
    // effect never re-ran and 'queued' never appeared.
    expect(seen).toContain('queued')
  } finally {
    stop()
  }
})
