---
name: managing-presets
description: Use when filling, tuning or creating preset seats for subagent dispatches - covers the agent_models survey, the four-seat group convention (coordinator, implementer, reviewer, final-reviewer), rafiki preset put/get, hard provider bans, and the tuning loop that compares what children actually ran on against their preset.
---

# Managing presets

Presets are the operator's seat policy: named `<group>:<role>` rows in the
daemon's database that fix what a spawned agent runs on — kind, provider,
model, tools, system prompt, budgets. Dispatches name a preset
(`agent_spawn`'s `preset`); they never pick a model. This skill is how a seat
gets filled, checked, and tuned.

By convention a group has four seats: `<group>:coordinator`,
`<group>:implementer`, `<group>:reviewer`, `<group>:final-reviewer`. Reviewers
are always a fresh spawn — never the agent (or the model) that wrote the code.
The whole-change review runs on `:final-reviewer` and is budgeted first.

## Survey before you write

Never carry a model id in memory or in a habit. Prices, availability and
provider health move week to week, and an id you remember is stale on a
schedule you do not control. Survey at tuning time:

- `agent_models` with **no arguments** returns a distribution — how many
  models, price range and median, context range, tool and vision counts, how
  many carry benchmark scores. Use it to aim the second call; it does not
  return every row, and you should not want it to.
- Then narrow (`sort=code` puts the strongest code models first) and ask for a
  handful rather than a page. The response states how many matched before the
  cap.
- A model the catalog cannot answer for — no price, no context, no score — is
  **kept**, not excluded. Locally-served models look exactly like that.

### Qualities, and when each is load-bearing

- **Tool calling** is non-negotiable for any agent that must act, and the
  catalog reports it as a **tri-state**: "unknown" is not "no". A model that
  cannot call tools spawns, attaches, and does nothing.
- **Context** measured against the brief plus everything it will read, not
  against the brief alone.
- **Benchmark scores** are weak third-party evidence. Absent means *unscored*,
  never zero — an unbenchmarked model is not a bad one.
- **Turn count beats token price.** The cheapest tier routinely takes two or
  three times the turns on multi-step work and costs more overall. It is right
  when the brief contains the code to write — transcription plus tests. For
  anything that must be inferred from prose, use a stronger seat.
- **Kind scoping is a mechanism, not a policy.** A `claude`-kind preset can
  only resolve Anthropic ids; a `fundi` preset needs a provider-qualified one.
  Hand either the wrong shape and it spawns, attaches, and never answers.

## Hard bans (enforce these in every seat)

- **Subagent dispatches are BANNED on Anthropic models** (anything containing
  `anthropic`/`claude` in the id). They burn the personal subscription's 5h/7d
  windows (`quota_status`) and have run an order of magnitude over comparable
  OpenRouter dispatches. Only pick a model over $1/mtok when the human says to.
- **Avoid `deepseek-v4-flash`** — it cannot reliably make tool calls.
- `z-ai/glm-5.3-flash` has completed full implement/review/fix waves and is a
  sound default seat; `z-ai/glm-5.3` is the stronger final-review step up when
  the work earns it. These are observations, not promises — the survey outranks
  them.

## Writing a seat

```sh
rafiki preset put <group>:<role> -f - <<'EOF'
{
  "kind": "fundi",
  "provider": "openrouter",
  "model": "openrouter/z-ai/glm-5.3-flash",
  "description": "implementer seat: full tool access, tight budget",
  "max_cost": 0.7
}
EOF
```

- `-f -` reads stdin, so it works whatever the session's provenance.
- For `tools`/`skills`/`mcp_servers`: omitted = the kind's default (everything);
  `[]` = none. Never collapse the two.
- A `claude`-kind preset honours only `model`, `provider`,
  `append_system_prompt`, `executor`, `labels` and budgets — every fundi-only
  knob is rejected.
- Spawn-time fields may override `model`/`thinking`/budgets and only narrow
  tools/skills/mcp_servers/context_files. A preset edit never reaches a running
  or resumed child (resolution happens once, at spawn).

Only create, change or delete a preset when the human asked you to. For a
one-off, override `model` on `agent_spawn` instead of editing the seat.

## Tuning a group

1. **Fill or adjust the four seats** against the survey above, then sanity-run:
   `rafiki preset list <group>:` and `rafiki preset get <group>:implementer`.
2. **Compare intent against reality.** For recent children, read their
   `rafiki/preset` label and the `rafiki/preset-id` row (agent_list / the
   cockpit), and check the model each child actually served on against the
   model the preset row names. Flag every divergence and ask why: a preset
   edited after the child spawned is fine (resume never re-resolves); a child
   that silently ran on the daemon default means its dispatch omitted the
   preset — a dispatch bug, not a tuning problem.
3. **Retire a model that failed, don't retry it.** Evidence from a plan's model
   log (rounds-to-accept, budget breaches, blocked work) outweighs the
   benchmark scores that recommended it. One plan is a small sample; three
   dispatches at a role, or one strong single-shot signal, is the bar for
   acting.