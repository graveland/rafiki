<script lang="ts">
  let {
    title,
    body,
    confirmLabel = 'Confirm',
    danger = false,
    onconfirm,
    oncancel,
  }: {
    title: string
    body: string
    confirmLabel?: string
    danger?: boolean
    onconfirm: () => void
    oncancel: () => void
  } = $props()
</script>

<div class="modal modal-open">
  <div class="modal-box">
    <h3 class="text-lg font-bold">{title}</h3>
    <p class="py-4">{body}</p>
    <div class="modal-action">
      <button type="button" class="btn" onclick={() => oncancel()}>Cancel</button>
      <button
        type="button"
        class="btn {danger ? 'btn-error' : 'btn-primary'}"
        onclick={() => onconfirm()}
      >
        {confirmLabel}
      </button>
    </div>
  </div>
</div>
