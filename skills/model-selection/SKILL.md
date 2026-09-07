---
name: model-selection
description: Use when choosing which model to run a subagent on, deciding between the Anthropic subscription and OpenRouter, or judging whether a model has gone bad - covers agent_models queries, the quota gate, and the two ways model health degrades.
---

# Choosing a model for a subagent

## Never name a model id in a plan, a skill, or a habit

The catalog is 400+ models whose prices, availability and **provider health**
move week to week. An id written down is stale on a schedule you do not
control.

Write a **query** instead, and resolve it when you dispatch:

- `agent_models` with **no arguments** returns a distribution — how many models,
  price range and median, context range, tool/vision counts, how many carry
  benchmark scores. Use it to aim the second call. It does not return 400 rows
  and you should not want it to.
- Then narrow: `needs: [tools]` at minimum (a model that cannot tool-call
  spawns, attaches and does nothing), a price bound, a context bound, and a
  sort. Ask for a handful, not a page.
- A model the catalog cannot answer for — no price, no context, no score — is
  **kept**, not excluded. Every locally-served model looks like that.

**Kind scoping is real.** A `claude` child can only run Anthropic models; give
it an OpenRouter id and it spawns, attaches, and never answers. A `fundi` child
needs a provider-qualified id.

## The subscription is scarce, not free

The Anthropic subscription's 5h and 7d rolling windows are a **budget with
opportunity cost**. Trivial work that consumes 5h headroom costs you the real
work that cannot run at hour four. That is why cheap OpenRouter models are
genuinely cheaper for simple jobs even against a sunk subscription cost.

The rule: **spend subscription where capability is load-bearing; buy the rest
at under 10c/Mtok.**

`quota_status` reports your own captured 5h/7d utilization. Consult it **once
per plan**, not per dispatch — it is a rolling window that does not move
between two spawns, and a call per dispatch is a turn per dispatch. Re-check
when a high-stakes dispatch settles.

"No data captured yet" is normal, not an error: it means nothing has billed the
subscription through the proxy. Treat it as no signal, not as headroom.

## Seats by ceremony rung

| Rung | Implementer | Reviewer |
|---|---|---|
| 0 | you, inline — no spawn | none |
| 1 | cheapest tool-capable OpenRouter model, tight `max_cost` | you read the diff |
| 2 | `kind: claude` on sonnet if quota is healthy, else a mid-tier OpenRouter model | mid-tier OpenRouter |
| 3 | `kind: claude` on the strongest available, or a top OpenRouter model | strongest available, always a separate seat |

When the subscription is heavily used, demote **rung 2** to OpenRouter and
leave rung 3 alone. Rung 3 is the last work to give up the subscription,
because a weaker model there costs you a bug rather than a dollar.

**Turn count beats token price.** The cheapest models routinely take two or
three times the turns on multi-step work and cost more overall. The cheapest
tier is right when the brief contains the code to write — transcription plus
tests. For anything requiring inference from prose, start mid-tier.

**Always set the model explicitly.** An omitted model inherits the daemon
default, which silently defeats every choice above.

## Model health degrades along two axes. Review sees one.

- **Quality** — rounds-to-accept, blocked work, specs missed. Review catches
  this.
- **Economics** — the same work suddenly costing several times more. Review
  catches this **never**, and it is usually a *provider-side* regression rather
  than anything about the model: a cache layer stops hitting, and output
  quality is unchanged while cost multiplies.

The second is the one that actually happens. Watch for it, because nothing else
will.

### On an economic anomaly, switch models. Do not diagnose.

Switching costs one spawn. Debugging someone else's cache layer costs an
afternoon and you do not own the fix.

Your instrument is `max_cost` sized to what the work should cost. A cheap task
breaching a tight cap **is** the anomaly — that is the signal, not a request
for more money.

## Record what you observed, not what you thought

One append-only line per dispatch in your plan's ledger. Observations only: a
model narrating its impression of another model reads plausibly and is
worthless.

```
MODEL <model-id> rung=2 task=2.3 outcome=accepted rounds=1 cap=0.40 spend=0.11 note=clean first pass
MODEL <model-id> rung=1 task=1.2 outcome=budget   rounds=0 cap=0.10 spend=0.10 note=blew cap on a 6-line edit
```

`outcome` is one of `accepted`, `rejected`, `blocked`, `budget`, `error`.
`spend` is `unknown` if you cannot observe it. `note` is a few words; it is not
a place for reasoning.

At the end of the work, write a **verdict per model used**, and make the
recommendation a **rung ceiling** — "fine at rung <=1, three budget breaches at
rung 2" is actionable; "good" is not.

**"Insufficient evidence" is a real verdict and often the right one.** One
plan is a small sample, and a model that failed twice may simply have drawn the
two hard tasks. No evidence beats false evidence. A verdict needs three
dispatches at a rung — or one strong single-shot signal, and a cheap task
breaching a tight cap is strong.

**Do not read another plan's model log.** Aggregating across plans is someone
else's job; acting on last week's small sample is how you refuse a model that
has been fine for a month.
