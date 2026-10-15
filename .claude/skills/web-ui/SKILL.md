---
name: web-ui
description: Use when touching web/, pkg/webui, or cmd/rafiki/cmd_web.go — the rafiki web UI, its localhost proxy and auth gate, the browser stream budget, and the transcript history/stream join.
---

# The rafiki web UI (`web/`, `pkg/webui`, `rafiki web`)

The browser client, its localhost proxy and gate, and the transcript join.
Same discipline as CLAUDE.md: rule + where it's enforced, no incident narration.

- **Stack.** Svelte 5 (runes) + Vite + TypeScript, Tailwind v4 + daisyUI,
  Connect-web (`@connectrpc/connect-web`) over the generated TS client
  (`web/src/gen/`, produced by `make web`'s `pnpm run gen` from
  `control.proto`/`event.proto`), Vitest + jsdom for tests, and Observable Plot
  on the Spend screen. Generated `web/src/gen/`, `web/node_modules/` and
  `pkg/webui/dist/*` (except `.gitkeep`) are never committed.
- **The pane snippet contract** (`SplitPanes.svelte`, `Transcript.svelte`).
  SplitPanes renders each pane as `{@render pane.snippet(id)}` — ONE stable
  snippet shared by every pane, never a fresh bound closure per id and never a
  raw value. The render tag wraps the argument as a getter (Svelte 5 snippet
  params are getters), so a per-id closure both changes the function — tearing
  the pane's each-block down on every route change — and passes a raw id where a
  getter is expected. Enforced by `SplitPanes.test.ts`.
- **The gate and the session model.** `rafiki web` binds `127.0.0.1` only
  (`cmd/rafiki/cmd_web.go`); rafikid never serves the UI. Every request —
  static assets included — must carry `Host: 127.0.0.1:<port>` or
  `localhost:<port>`, and every non-GET must carry an exactly matching `Origin`
  (a missing one is refused); a refusal carries `Rafiki-Web-Gate: refused`
  (`pkg/webui/gate.go`). The printed link carries a **single-use** launch code in
  the URL fragment (`#k=<code>`); the browser exchanges it at
  `POST /_rafiki/session` for a per-tab secret kept in `sessionStorage` and sent
  as `X-Rafiki-Web` on every Control and `/_rafiki/*` call. **No cookies**
  (host-scoped, not port-scoped), no `Access-Control-*` header on any response.
  The proxy forwards `/rafiki.v1.Control/*` with no allowlist and no message
  parsing; it strips `Authorization`/`Cookie`/`X-Rafiki-Web`/`Origin`, and the
  bearer + protocol epoch come from `Upstream.Client` (`bearerTransport`).
  `/rafiki.v1.Login/*` is never proxied.
- **One login trigger.** A Connect `Unauthenticated` on a call WITHOUT the gate
  header (`client.ts` `authInterceptor`) → `needs-login`; a `Rafiki-Web-Gate:
  refused` response → `gate-refused` (the session is gone). Check the Connect
  code, not HTTP 401. `LinkUsedError`/`GateRefusedError`/`NoSessionError` map to
  their own auth states.
- **The login flow.** `BeginLogin` opens a popup at the daemon's OIDC URL → the
  daemon redirects the popup to `#/login-done` at the PINNED callback port
  (`pkg/webui/login.go`, hard-coded `127.0.0.1`) → `LoginDone` signals completion
  over a BroadcastChannel (origin-scoped) → the opener swaps its upstream. A tab
  the user opened at `localhost:<port>` (the gate allows it) never hears
  login-complete — the channel is origin-scoped: the token is saved and the
  upstream swapped, so a reload recovers, but its banner sticks.
- **The connection budget.** Browsers allow ~6 HTTP/1.1 connections per origin.
  Spend it deliberately: **1 rail stream + ≤3 live conversation streams per
  tab** (`router.transcriptPanes`, `MAX_TRANSCRIPT_PANES = 3`; the overflow ids
  stay in the route and a banner says so). A hidden pane pauses — closes its
  stream and KEEPS its cursor, resuming by replay (`TranscriptPane`'s
  visibility listener). A second tab shares the origin's budget; the shell warns
  (`otherTabs`).
- **The transcript join rules** (`transcript.svelte.ts`, `Conversation`): read
  the child's latest event ordinal from `getChild` → fold `getHistory` →
  `StreamEvents {child, tier ALL}` from cursor `after = latest`. `latest` is
  **-1, never 0**, for a child with no events (0 is a real ordinal). On a
  `getHistory` error, replay from -1. History/stream overlap is trimmed by a
  `(role, content)` fingerprint (`messageKey`/`trimDuplicate`); a durable
  `assistant_message` supersedes the in-flight delta block; a `user_message`
  carrying only tool results routes into its `tool_use` instead of opening a
  turn. `resume()` never streams before the join succeeded (`opened`), and an
  in-flight `open()` (`opening`) blocks a duplicate join. A tool duration is
  `seconds*1000 + nanos/1e6`, and a zero/unset proto duration means "start never
  seen" and renders `-` (`toolDurationMs`).
- **Send-queue rules** (`sendQueue.svelte.ts`): `Send` is not idempotent, so it
  is never auto-retried — a failed entry persists and only a user-triggered
  `resend` re-issues it. Status changes must go through the `$state` entry (the
  reactive proxy), never the raw pushed object, or the UI never sees them. ABORT
  carries no content (an empty composer sends `[]`, not a blank text block).
- **The dev loop.** `pnpm dev` in `web/`, then `rafiki web --dev-assets
  http://localhost:5173` (static requests reverse-proxy to Vite; `--dev-assets`
  is the one way to run without `make web`). Verify with
  `make check check-web` — `make check` is Go-only and can stay green while the
  generated TS client breaks the web build.
