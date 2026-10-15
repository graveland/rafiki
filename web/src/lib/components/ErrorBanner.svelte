<script lang="ts">
  let {
    title,
    message,
    action,
  }: {
    title: string
    message: string
    action?: { label: string; onclick: () => void }
  } = $props()
</script>

<div class="alert alert-error">
  <div>
    <h3 class="font-bold">{title}</h3>
    <div class="text-sm">{message}</div>
  </div>
  {#if action}
    <button type="button" class="btn btn-sm" onclick={() => action?.onclick()}>
      {action?.label}
    </button>
  {/if}
</div>
