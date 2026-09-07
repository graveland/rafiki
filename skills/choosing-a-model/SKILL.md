---
name: choosing-a-model
description: How to pick a model for a subagent from rafiki's catalog
---

# Choosing a model

`agent_models` with no arguments returns a SUMMARY — how many models there
are, price and context ranges, and how many support tools or vision. It does
not return the whole catalog, because the catalog is 400+ entries and reading
it would fill your context without telling you anything about cost.

Narrow first, then list. Pass filters and a `limit`; the response always states
how many models matched before the cap.

## What the catalog cannot tell you

Absent values mean UNKNOWN, never zero. A model with no benchmark score is
unscored, not bad; a model with no price is usually locally served rather than
free. Filters admit unknowns deliberately, so a narrow query still surfaces
local models.

## Match the model to the work

A subagent that must call tools needs tool support — the catalog reports this
as a tri-state, and "unknown" is not "no". Prefer a cheap model for mechanical
work and reserve expensive ones for judgement. Cost is charged to your budget.
