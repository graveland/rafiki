<script lang="ts">
  import { get } from 'svelte/store'
  import { authState, type AuthState } from './lib/authState'
  import { otherTabs } from './lib/session'
  import { startLogin } from './lib/login'
  import { route } from './lib/router'
  import Agents from './routes/Agents.svelte'
  import Transcript from './routes/Transcript.svelte'
  import Search from './routes/Search.svelte'
  import Spend from './routes/Spend.svelte'
  import LoginDone from './routes/LoginDone.svelte'
  import ErrorBanner from './lib/components/ErrorBanner.svelte'
  import Toasts from './lib/components/Toasts.svelte'

  const DAEMON = window.location.host

  let authEpoch = $state(0)
  let loginError = $state<string | null>(null)
  let otherTabsDismissed = $state(false)

  let previousAuthState: AuthState = get(authState)

  $effect(() => {
    const current = $authState
    if (current === 'ok' && previousAuthState !== 'ok') {
      authEpoch += 1
    }
    previousAuthState = current
  })

  function signIn(): void {
    loginError = null
    startLogin().catch((error: unknown) => {
      loginError = error instanceof Error ? error.message : String(error)
    })
  }
</script>

<div class="navbar bg-base-100">
  <div class="flex-1">
    <a class="btn btn-ghost text-xl" href="#/agents">rafiki</a>
  </div>
  <div class="flex-none">
    <ul class="menu menu-horizontal px-1">
      <li><a href="#/agents">Agents</a></li>
      <li><a href="#/search">Search</a></li>
      <li><a href="#/spend">Spend</a></li>
    </ul>
  </div>
</div>

<main>
  <div class="space-y-2 p-2">
    {#if $authState === 'needs-login'}
      <ErrorBanner
        title="Sign in required"
        message={loginError ?? `Sign in to ${DAEMON} required`}
        action={{ label: 'Sign in', onclick: signIn }}
      />
    {:else if $authState === 'gate-refused'}
      <ErrorBanner
        title="Session ended"
        message="This rafiki web session has ended (rafiki web restarted?). Re-open the link printed by `rafiki web`."
      />
    {:else if $authState === 'link-used'}
      <ErrorBanner
        title="Link already used"
        message="This link was already used. Run `rafiki web` again for a new one."
      />
    {:else if $authState === 'no-session'}
      <ErrorBanner
        title="No session"
        message="No rafiki web session in this tab. Open the link printed by `rafiki web`, or open this page from an existing rafiki web tab."
      />
    {/if}

    {#if $otherTabs > 0 && !otherTabsDismissed}
      <div class="alert alert-info">
        <span
          >Another rafiki web tab is open; browsers allow only a few live connections per site, so
          streams may stall.</span
        >
        <button
          type="button"
          class="btn btn-xs btn-ghost"
          aria-label="dismiss"
          onclick={() => (otherTabsDismissed = true)}
        >
          ✕
        </button>
      </div>
    {/if}
  </div>

  {#key authEpoch}
    {#if $route.name === 'agents'}
      <Agents />
    {:else if $route.name === 'transcript'}
      <Transcript />
    {:else if $route.name === 'search'}
      <Search />
    {:else if $route.name === 'spend'}
      <Spend />
    {:else if $route.name === 'login-done'}
      <LoginDone />
    {/if}
  {/key}
</main>

<Toasts />
