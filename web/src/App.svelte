<script lang="ts">
  import { route } from './lib/router'
  import Agents from './routes/Agents.svelte'
  import Transcript from './routes/Transcript.svelte'
  import Search from './routes/Search.svelte'
  import Spend from './routes/Spend.svelte'
  import LoginDone from './routes/LoginDone.svelte'
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
</main>
