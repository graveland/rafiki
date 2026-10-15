<script lang="ts">
  import { dismissToast, toasts } from '../toasts'
</script>

<div class="toast toast-end z-50">
  {#each $toasts as toast (toast.id)}
    <div class="alert {toast.kind === 'error' ? 'alert-error' : 'alert-info'}">
      <span>{toast.text}</span>
      <button
        type="button"
        class="btn btn-xs btn-ghost"
        aria-label="close"
        onclick={() => dismissToast(toast.id)}
      >
        ✕
      </button>
    </div>
  {/each}
</div>
