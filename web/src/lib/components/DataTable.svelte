<script lang="ts" generics="T">
  interface Column {
    key: string
    label: string
    sortable?: boolean
    align?: 'left' | 'center' | 'right'
    render?: (row: T) => string
  }

  let {
    columns,
    rows,
    rowKey,
    indent,
    filterText = '',
    onrowclick,
  }: {
    columns: Column[]
    rows: T[]
    rowKey: (row: T) => string
    indent?: (row: T) => number
    filterText?: string
    onrowclick?: (row: T) => void
  } = $props()

  let sortKey = $state<string | null>(null)
  let sortDirection = $state<'asc' | 'desc'>('asc')

  function cellValue(row: T, key: string): unknown {
    return (row as Record<string, unknown>)[key]
  }

  function compare(a: unknown, b: unknown): number {
    if (a == null && b == null) {
      return 0
    }
    if (a == null) {
      return 1
    }
    if (b == null) {
      return -1
    }
    if (typeof a === 'number' && typeof b === 'number') {
      return a - b
    }
    return String(a).localeCompare(String(b))
  }

  let visible = $derived.by(() => {
    let list = rows
    const needle = filterText.trim().toLowerCase()
    if (needle !== '') {
      list = list.filter((row) =>
        columns.some((column) =>
          String(cellValue(row, column.key) ?? '')
            .toLowerCase()
            .includes(needle),
        ),
      )
    }
    if (sortKey !== null) {
      const key = sortKey
      const direction = sortDirection === 'asc' ? 1 : -1
      list = [...list].sort((a, b) => compare(cellValue(a, key), cellValue(b, key)) * direction)
    }
    return list
  })

  function toggleSort(column: Column): void {
    if (!column.sortable) {
      return
    }
    if (sortKey === column.key) {
      sortDirection = sortDirection === 'asc' ? 'desc' : 'asc'
    } else {
      sortKey = column.key
      sortDirection = 'asc'
    }
  }

  function cellText(row: T, column: Column): string {
    if (column.render) {
      return column.render(row)
    }
    const value = cellValue(row, column.key)
    return value == null ? '-' : String(value)
  }

  function alignClass(column: Column): string {
    if (column.align === 'right') {
      return 'text-right'
    }
    if (column.align === 'center') {
      return 'text-center'
    }
    return 'text-left'
  }

  function indentStyle(row: T): string | undefined {
    const depth = indent ? indent(row) : 0
    return depth > 0 ? `padding-left: ${depth * 1.25}rem` : undefined
  }
</script>

<div class="overflow-x-auto">
  <table class="table table-zebra">
    <thead>
      <tr>
        {#each columns as column (column.key)}
          <th class={alignClass(column)}>
            {#if column.sortable}
              <button
                type="button"
                class="cursor-pointer select-none"
                onclick={() => toggleSort(column)}
              >
                {column.label}
                {#if sortKey === column.key}
                  <span aria-hidden="true">{sortDirection === 'asc' ? '▲' : '▼'}</span>
                {/if}
              </button>
            {:else}
              {column.label}
            {/if}
          </th>
        {/each}
      </tr>
    </thead>
    <tbody>
      {#each visible as row (rowKey(row))}
        <tr
          class={onrowclick ? 'cursor-pointer' : ''}
          role={onrowclick ? 'button' : undefined}
          tabindex={onrowclick ? 0 : undefined}
          onclick={onrowclick ? () => onrowclick?.(row) : undefined}
          onkeydown={onrowclick
            ? (event) => {
                if (event.key === 'Enter' || event.key === ' ') {
                  event.preventDefault()
                  onrowclick?.(row)
                }
              }
            : undefined}
        >
          {#each columns as column, index (column.key)}
            <td class={alignClass(column)} style={index === 0 ? indentStyle(row) : undefined}>
              {cellText(row, column)}
            </td>
          {/each}
        </tr>
      {/each}
    </tbody>
  </table>
</div>
