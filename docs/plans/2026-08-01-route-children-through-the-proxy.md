# Route pi and claude children through the rafiki proxy

**Goal:** every child kind reaches its model through this repo's proxy, so capture,
failover and model resolution stop being a privilege of the `agent` kind.

**Status:** plan. Not started.

## Why

`agent` children already get all three, in-process via `pkg/llm` + `pkg/routing`.
`pi` and `claude` children bypass rafiki entirely and talk to providers directly —
verified: the only place in the tree that sets `ANTHROPIC_BASE_URL` is
`cmd/rafiki/claude.go`, the launcher added 2026-07-31.

Consequences today: no capture rows for pi/claude turns, no OpenRouter failover,
and three different model universes — which is why `--model` completion had to be
scoped per kind (`cmd/fundi/models.go`). Routing everything through the proxy
collapses that back to one catalog, which is the better end state.

## What the spike already settled

From `docs/plans/2026-07-30-phase2-spike-findings.md` §7, verified live against pi
v0.80.6:

- `pi.registerProvider(name, {baseUrl, headers})` is a real override path.
  `model-registry.ts:1026`'s `else if (config.baseUrl || config.headers)` branch
  skips `validateProviderConfig` entirely when the config has no `models` array.
- Both custom headers were delivered exactly; the baseUrl override was honoured.
- pi authenticates with **`X-Api-Key`**, not `Authorization: Bearer`. rafiki's face
  accepts either — but it is the other branch from claude's.
- pi sends **no** native session-id header, so the extension is the *only* source
  of the correlation ref. (claude sends `X-Claude-Code-Session-Id` natively.)
- pi makes **no** `/api/hello` preflight.
- `pi -p` hangs forever without `< /dev/null` — matters for any automated probe.

## Delivery vehicle: the extension already exists

`cmd/fundi/helpersembed/fundi-helpers/index.ts` is an embedded pi extension that
fundi already auto-installs into `~/.pi/agent/extensions/`, exporting
`export default function (pi: ExtensionAPI)` and calling `pi.registerCommand`.
That is the same API object the spike used. So the override is an addition to a
shipped extension, not a new per-child `pi -e` file to generate and clean up.

---

## Prerequisite: fundid must read an env file

Two independent problems, one fix, and both are live defects today.

**1. The Linux unit cannot carry a newline.** `ANTHROPIC_CUSTOM_HEADERS` accepts a
literal newline and *nothing else* as a separator (spike §4) — comma, semicolon
and `\n`-escape each silently collapse into one malformed header. Routing claude
children needs at least two headers. `cmd/fundi/service_linux.go`'s `unitQuote`
was verified on 2026-08-01 to emit a **broken two-line unit** for such a value:

```
Environment="ANTHROPIC_CUSTOM_HEADERS=X-Rafiki-Session: abc
X-Rafiki-Source: pi"
```

systemd's `Environment=` is line-based; the second line is not a directive. The
spike anticipated exactly this ("the Linux unit must be able to carry a literal
newline").

**2. Credentials cannot live in the unit.** `cmd/fundid/agent_runtime.go:114`
documents the precedence as *daemon env < forwarded env < explicit `req.APIKey`*,
so the daemon's own environment is the **base** — an autonomous or resumed agent
child with no caller has only that. But unit files are world-readable (0644), so
`daemonEnvVars` deliberately excludes `ANTHROPIC_API_KEY` / `OPENROUTER_API_KEY`.
Today that hole is papered over by `--forward-env` from an interactive shell.

**Fix: `fundid` reads an optional env file at startup**, default
`$XDG_CONFIG_HOME/fundi/service.env`, overridable with `FUNDI_ENV_FILE`.

Chosen over the init-system mechanisms because there is no cross-platform one:
systemd has `EnvironmentFile=`, launchd has no equivalent at all, and the launchd
workaround is a `/bin/sh -c '. file; exec fundid'` wrapper that makes the service
definition harder to read and breaks `ProgramArguments` introspection. Reading it
in the daemon is one implementation, identical on both platforms, and it takes
arbitrary values including newlines because it is not a unit file.

Semantics: real environment wins over the file (so `FUNDI_AGENT_DB=… fundid`
still overrides), missing file is not an error, parse failures are logged and
skipped rather than fatal, and the file is required to be `0600` — a warning, not
a refusal, since refusing to boot over a permission bit is worse than the leak it
prevents.

---

## Design

### Configuration

Three new daemon-scoped variables, added to `daemonEnvVars` so the existing
capture-into-the-unit mechanism carries them:

| variable | meaning |
|---|---|
| `FUNDI_PROXY_URL` | rafiki base URL. **Empty disables all of this** — children keep talking to providers directly, so the change is opt-in and cannot break an existing install. |
| `FUNDI_PROXY_TOKEN` | static bearer/api-key for the proxy. Belongs in `service.env`, not the unit. |
| `FUNDI_PROXY_KINDS` | which kinds to route, default `pi,claude`. An escape hatch for bisecting a regression without a rebuild. |

### claude children

Inject the same environment `cmd/rafiki/claude.go` already assembles — extract
`buildClaudeInvocation`'s env logic into something both callers share rather than
writing it twice:

- `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`
- `ANTHROPIC_CUSTOM_MODEL_OPTION` + `_NAME` when a model is set (never
  `ANTHROPIC_MODEL` — allowlist-validated client-side, rejects non-Anthropic ids)
- `ANTHROPIC_CUSTOM_HEADERS` with `X-Rafiki-Source: claude` — note claude already
  sends `X-Claude-Code-Session-Id`, so a second correlation header is optional and
  should be added only if the proxy does not already key on it.
- strip inherited `ANTHROPIC_*` and the provider keys, as that function does.

### pi children

The daemon injects into the pi child's environment:

- `FUNDI_PROXY_URL`, `FUNDI_PROXY_TOKEN`
- `FUNDI_SESSION_REF` — the child's session id, since pi sends no native one

`fundi-helpers/index.ts` reads them at load and, when the URL is non-empty, calls

```ts
pi.registerProvider("anthropic", {
  baseUrl: process.env.FUNDI_PROXY_URL,
  headers: {
    "X-Api-Key": process.env.FUNDI_PROXY_TOKEN ?? "",
    "X-Rafiki-Session": process.env.FUNDI_SESSION_REF ?? "",
    "X-Rafiki-Source": "pi",
  },
});
```

`X-Api-Key`, not Bearer — that is what pi sends and what the spike verified.

### RESOLVED 2026-08-01 — the whole catalog is reachable, but only via a synthesised provider

Probed live against pi v0.80.6 and a capture server. Three results, in order:

1. **Override-only does NOT open the catalog.** `registerProvider("rafiki",
   {baseUrl, headers})` with no `models` array → `Error: Model "rafiki/glm-5.2"
   not found. Use --list-models to see available models.` The override branch
   delivers headers and baseUrl (spike §7 was right) but pi still resolves the id
   against its registry first. So overriding `"anthropic"` only ever routes the
   `anthropic/*` ids pi *already knows*.
2. **Adding `models` demands a key**: `Provider rafiki: "apiKey" or "oauth" is
   required when defining models.` — `validateProviderConfig`, which the
   override-only branch had been skipping.
3. **With `models` + `apiKey` it works.** The capture server received:

```
POST /v1/messages
  X-Api-Key:        spike-token      (Authorization: null — pi does not send Bearer)
  X-Rafiki-Session: spike-sess-1
  X-Rafiki-Source:  pi
  model:            glm-5.2
```

**The load-bearing detail is that last line.** pi strips the provider prefix
before the wire: `rafiki/glm-5.2` arrives as `glm-5.2`. That is exactly a rafiki
short alias, so `ResolveModel` handles it with no translation layer — a
synthesised provider named `rafiki` composes with rafiki's own resolution for
free. Had pi sent the prefixed id, every entry would have needed rewriting.

So the answer is **yes**, with a caveat that changes the shape of the work: fundi
must synthesise a *full* provider definition — every model listed, each with
`id`, `name`, `cost`, `contextWindow`, `maxTokens`, `input`, `reasoning` — rather
than a three-line override. That is more work than this plan originally assumed,
but it is mechanical, and rafiki already holds the inputs: `pkg/models` has the
ids and `pkg/routing`'s catalog has context windows (`ContextWindow`) and pricing
(`pkg/routing/cost.go`).

Still unverified, and worth five minutes before task 6: whether
`api: "anthropic-messages"` is required or was merely accepted, and which model
fields are genuinely mandatory versus supplied out of caution. Both only affect
how much catalog metadata has to be plumbed.

Consequence for task 7: the kind-scoped completion in `cmd/fundi/models.go`
collapses — a proxied pi child can offer the same list as an agent child, plus
pi's own local `vmlx-*` providers, which stay direct because there is no reason
to route a local MLX server through a cloud proxy.

## Task order

1. **Spike the open question.** ~30 min. Everything downstream depends on it.
2. **Env-file support in fundid** (+ tests). Fixes the newline defect and the
   credential hole independently of the feature; land it even if 1 disappoints.
3. **Fix `unitQuote`** to reject or escape a newline rather than emitting a broken
   unit — with the env file as the documented place such values belong.
4. **Config plumbing** (`FUNDI_PROXY_*` into `daemonEnvVars`, spawn-time wiring).
5. **claude routing**, sharing the env builder with `cmd/rafiki/claude.go`.
6. **pi routing**, extending `fundi-helpers`.
7. **Re-scope `--model` completion** per the answer to 1.
8. **Live verification.** The spike's own constraint applies: this cannot be
   asserted, it has to be run. Spawn one child of each kind, confirm a row lands
   in `conversations.conversation_turn` with the right `external_ref`.

## Out of scope

- Making the database required / deleting the `mem-…` fallback. Separate phase-2
  item with its own failure mode (`fundid` unable to start).
- `renderRing` retirement — falsified by the spike, not happening.
