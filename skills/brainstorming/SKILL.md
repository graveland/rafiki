---
name: brainstorming
description: Use before any creative work - a new feature, component, subsystem, or a change in behaviour. Turns an idea into an agreed design before any code is written, scaling the ceremony to the size of the change.
---

# Brainstorming an idea into a design

Turn an idea into an agreed design through dialogue, then stop. Ceremony scales
with the task; **the approval gate never does.**

> Do not write code, scaffold anything, or invoke an implementation skill until
> you have told your human partner what you intend and they have said yes.

## Classify first, and say so out loud

Announce the path in one line — "this looks bounded, so I'll present a short
design here rather than write a spec" — so it can be overridden.

- **Spike** — a feasibility question whose output is an *answer*, not code you
  keep. Present the question and your probe in two or three sentences, get a
  nod, find out as cheaply as correctness allows. Anything you build is labelled
  throwaway.
- **Bounded** — a well-scoped change to a flow that **already exists in this
  repo**. A new flag, a small endpoint, a one-file fix. Ask the questions that
  matter, present a short design in chat, stop for approval, then implement.
  No design document.
- **Architectural** — a new subsystem, a change to how components fit together,
  or a change to an interface others depend on. Full path: questions,
  approaches, sectioned design, a written design doc, then a plan.

Bounded measures **the repo, not your familiarity**. If there is no existing
flow to read, it is not bounded. When torn between two paths, take the heavier
one. Hidden complexity found mid-task **upgrades** the path — stop and say so.
Nothing downgrades.

## The questions

- Look at the current state first: the files, the recent commits, the design
  docs in `docs/plans/`, `docs/reference/`, and CLAUDE.md.
- **CLAUDE.md is the repo's accumulated hard-won knowledge.** Read the entries
  touching your area before proposing anything; most of them exist because
  someone already got it wrong in exactly the way you are about to.
- Ask **one question per message**. Prefer multiple choice with your
  recommendation first and your reasoning stated.
- If the request is really several independent subsystems, say so before
  spending questions on details. Decompose, then brainstorm the first piece.
- YAGNI ruthlessly. Cut features from every approach you propose.

## Verify before you argue

Two failure modes cost more than any amount of discussion, and both look like
confident analysis:

- **Check where code actually executes before critiquing a design that touches
  a filesystem.** The daemon, the executor and a child can be three different
  machines.
- **Check the mechanism before declaring something impossible.** If your human
  partner says a sibling project already does it, they are usually right and
  you are usually generalising from the examples you happened to see.

Read the code. A grep is cheaper than a wrong paragraph.

## The design

For an architectural change: propose two or three approaches with trade-offs,
lead with your recommendation and why. Then present the design **in sections**,
scaled to their complexity, asking after each whether it holds.

Design for isolation: units with one clear purpose, well-defined interfaces,
understandable without reading their internals. In existing code, follow
existing patterns and fix problems that are genuinely in your way — never
propose unrelated refactoring.

## Writing it down

Architectural designs go to `docs/plans/YYYY-MM-DD-<topic>-design.md`.

**Design docs and plans are scaffolding. They are not committed** — that
directory is gitignored on purpose. What survives is code, tests, and
knowledge; knowledge graduates to `docs/reference/` or CLAUDE.md in the same
commit as the code it describes.

Then review your own doc once with fresh eyes: placeholders, self-contradiction,
scope, and any requirement that could be read two ways. Fix inline, then ask
your human partner to review it before you go further.

## Terminal states

- **Spike** ends with a reported recommendation.
- **Bounded** ends with approval, then ordinary implementation.
- **Architectural** ends by writing a plan. That is the only skill you invoke
  next.
