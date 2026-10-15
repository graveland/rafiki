import { flushSync, mount, unmount } from 'svelte'
import { expect, it, vi } from 'vitest'

// A jsdom mount of the real Agents route, pinning the set-budget dialog's
// number-input contract. The field is `type="number"`, so Svelte's bind:value
// writes a number (or null/undefined when cleared) into the dialog state — the
// old `.trim()` validation threw a TypeError on the first keystroke and crashed
// the dialog. This test types, clears and types again to prove the value is
// validated on its real runtime type and that Set is gated correctly.
//
// The rail is mocked: it is a module-level streaming subscriber whose onMount
// would otherwise open a real event stream, and the dialog only needs its rows.
const rail = vi.hoisted(() => ({ rows: new Map<string, unknown>() }))
vi.mock('../../lib/rail.svelte', () => ({
  children: rail.rows,
  latest: new Map(),
  startRail: () => {},
  stopRail: () => {},
  PARENT_LABEL: 'rafiki/parent',
}))

import Agents from '../../routes/Agents.svelte'

function findButton(root: ParentNode, text: string): HTMLButtonElement {
  const button = [...root.querySelectorAll('button')].find(
    (candidate) => candidate.textContent?.trim() === text,
  )
  if (!button) {
    throw new Error(`no button labelled ${JSON.stringify(text)}`)
  }
  return button as HTMLButtonElement
}

/** Type `value` into a bound number input the way a browser would. */
function typeNumber(input: HTMLInputElement, value: string): void {
  input.value = value
  input.dispatchEvent(new Event('input', { bubbles: true }))
  flushSync()
}

it('set-budget dialog validates the number input without crashing', () => {
  rail.rows.set('child-1', {
    id: 'child-1',
    name: 'kid',
    status: 'working',
    kind: 'fundi',
    model: 'm',
    labels: {},
  })

  const target = document.createElement('div')
  document.body.appendChild(target)
  const app = mount(Agents, { target })
  flushSync()

  findButton(target, 'Set budget').click()
  flushSync()

  const modal = target.querySelector('.modal-box') as HTMLElement
  const input = modal.querySelector('input[type="number"]') as HTMLInputElement
  expect(input).not.toBeNull()
  const setButton = findButton(modal, 'Set')

  // A cleared field is not a budget: Set stays disabled (an empty field must
  // never be read as the daemon's "unlimited" 0).
  expect(setButton.disabled).toBe(true)

  // Typing a valid number enables Set — this is the keystroke that used to throw.
  typeNumber(input, '5')
  expect(setButton.disabled).toBe(false)

  // Clearing it disables Set again (and does not crash).
  typeNumber(input, '')
  expect(setButton.disabled).toBe(true)

  // A typed 0 is the documented unlimited sentinel: enabled, and labelled.
  typeNumber(input, '0')
  expect(setButton.disabled).toBe(false)
  expect(modal.textContent).toContain('Unlimited')

  unmount(app)
  target.remove()
})
