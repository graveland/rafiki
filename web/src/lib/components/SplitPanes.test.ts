import { flushSync, mount, unmount } from 'svelte'
import { expect, it } from 'vitest'
import Harness from './SplitPanesHarness.svelte'
import { addPane, setPanes } from './paneHarness.svelte'

// A jsdom mount of the real SplitPanes. The snippet contract is what keeps pane
// state (stream, history, draft, failed sends) alive across a "+": the pane
// snippet must be ONE stable function, called by SplitPanes as
// `{@render pane.snippet(id)}`. A fresh closure per pane (the old bound() shape)
// changes the render tag's function — and passes a raw id where prod expects a
// getter — so the pane is torn down (or throws) rather than preserved.
it('adding a pane preserves the existing panes', () => {
  const target = document.createElement('div')
  document.body.appendChild(target)
  setPanes(['a', 'b'])

  const app = mount(Harness, { target })
  flushSync()

  const aBefore = target.querySelector('[data-pane="a"]')
  const bBefore = target.querySelector('[data-pane="b"]')
  expect(aBefore).not.toBeNull()
  expect(bBefore).not.toBeNull()

  addPane('c')
  flushSync()

  // The third pane renders, and the first two DOM nodes are the SAME objects —
  // i.e. their panes were not destroyed and recreated.
  expect(target.querySelectorAll('[data-pane]')).toHaveLength(3)
  // Call contract: the snippet receives the pane id (as a getter the render tag
  // supplies) and renders it.
  const c = target.querySelector('[data-pane="c"]')
  expect(c?.getAttribute('data-pane')).toBe('c')
  expect(c?.textContent).toBe('c')
  expect(target.querySelector('[data-pane="a"]')).toBe(aBefore)
  expect(target.querySelector('[data-pane="b"]')).toBe(bBefore)

  unmount(app)
  target.remove()
})
