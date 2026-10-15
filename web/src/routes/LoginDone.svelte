<script lang="ts">
  import { onMount } from 'svelte'
  import { SESSION_CHANNEL } from '../lib/session'

  onMount(() => {
    const channel = new BroadcastChannel(SESSION_CHANNEL)
    channel.postMessage({ type: 'login-complete' })
    window.close()
  })
</script>

<div class="p-8 text-center">
  <p>Signed in — you can close this window.</p>
</div>
