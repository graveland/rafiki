---
name: coordinating-subagents
description: How to spawn, watch and steer rafiki subagents, and when not to
---

# Coordinating subagents

`agent_spawn` creates a separate, cross-process, potentially cross-machine,
dollar-metered rafiki agent. It outlives your conversation, appears in
`rafiki list`, and is budget/depth/executor-constrained. In Claude Code this
tool is exposed as `mcp__rafiki__agent_spawn`.

Reach for it when the work should survive independently, run on different
hardware, use a different model, or be watched from outside your session. For
a lightweight helper scoped to just this conversation, use your own built-in
subagent mechanism instead.

## Budgets are inherited and finite

A child's depth, cost and children limits are carved out of yours. A child you
give no budget cannot spawn at all. Spend deliberately: a spawn you cannot
justify in one sentence is usually work you should do yourself.

## Do not poll for completion

You are notified when a subagent settles. Polling `agent_list` in a tight loop
burns tokens and tells you nothing the notification would not. Ask once, then
do other work or stop.
