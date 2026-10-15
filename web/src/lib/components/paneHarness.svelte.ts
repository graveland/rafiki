// Shared state for the SplitPanes mount harness. Kept in a `.svelte.ts` module
// (not the component's <script module>) so both the harness component and the
// test can import the same reactive ids with proper types — plain tsc cannot see
// named exports of a `.svelte` file.
export const ids = $state<string[]>([])

export function setPanes(next: string[]): void {
  ids.length = 0
  for (const id of next) {
    ids.push(id)
  }
}

export function addPane(id: string): void {
  ids.push(id)
}
