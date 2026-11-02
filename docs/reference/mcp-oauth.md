# MCP OAuth

How a remote MCP server that authenticates with OAuth 2.1 gets its credential,
and what the runtime does with it. Operator configuration — the `[mcp_oauth]`
destination policy in `rafikid.toml` — is in
[configuration](../guide/configuration.md#mcp-oauth); this page is the contract
between the daemon that holds the credential and the agent runtime that dials
the server.

**No token ever enters a project file, a CLI invocation or a child process.**
`.mcp.json` describes a server; it never carries its authentication state. There
is no `"oauth": true` switch, `MCPServerConfig` gains no OAuth field, and a spawn
never starts an interactive flow — login is an explicit, separate act.

## Pieces

| Piece | Where | What it owns |
|---|---|---|
| Project entry | `pkg/fundi/tools` `MCPServerConfig` | `url`, `headers`, `command`, `via_executor`. No OAuth. |
| Runtime seam | `pkg/fundi/tools` `MCPHTTPFactory`, `RuntimeOptions.MCPHTTP` | When the factory is consulted, and how its answer is applied. |
| Factory | `pkg/mcpoauth` (engine, policy, canonical URLs) and `pkg/mcpauthdb` (durable rows) | Resolution of the owner's exact binding, token acquisition, the guarded HTTP client. |
| Durable state | Postgres, owner-scoped | Grant, endpoint binding, login/refresh fences, endpoint tombstones. |

## The runtime API

`RuntimeOptions.MCPHTTP` is an **out-of-band** factory. `nil` means this process
holds no MCP credentials — the standalone `rafikid fundi`, a daemon with no
database — and every direct-URL entry then dials exactly as it did before the
seam existed.

```go
type MCPHTTPFactory func(
    ctx context.Context,
    serverName, canonicalEndpoint string,
    headers map[string]string,
) (auth.OAuthHandler, *http.Client, error)
```

- `canonicalEndpoint` is the entry's `url` as written. Canonicalizing it (and
  refusing a URL that is not a canonical HTTP(S) one) belongs to the factory's
  package, so that every caller — runtime, login, logout — agrees on one
  spelling.
- `headers` is the entry's configured header map, passed so the factory can
  apply it to **MCP endpoint requests only**. It is not a licence to send those
  headers to discovery, registration or token URLs.
- The returned `*http.Client` becomes the MCP endpoint's transport, so it must
  already enforce the destination policy below. A `nil` client means "no guarded
  client" and the plain default client is used.
- An `error` **refuses that server**. It is not "no credential": a refused dial
  is logged and skipped, and the entry is never retried anonymously.

The factory is called at **dial time, once per session**, from
`tools.ConnectMCP`/`dialMCPServer`. The redial closure carries it, so a session
that dies and is replaced mid-life repeats the same owner-bound resolution and
never degrades into an anonymous dial.

### When the factory is consulted

Exactly one case: a **direct `url` entry with no `Authorization` header of its
own**.

| Entry | Factory? |
|---|---|
| `url`, no `Authorization` key | yes |
| `url` with `Authorization` under any capitalization, **including an empty value** | never — the entry's own auth wins |
| `url` with unrelated headers (`X-Api-Key`, `X-Mcp-Tenant`, …) | yes — those coexist with OAuth |
| stdio (`command`) | never |
| `via_executor` (command or proxy) | never |

Presence, not a non-empty value, is the switch: an explicit empty
`Authorization` is still the operator's decision about that entry, and OAuth
overwriting it would let the daemon's stored credential beat the project's
configuration. `Authorization` is matched case-insensitively because the key
comes from a JSON object and its spelling is the author's.

`via_executor` entries are excluded for a second reason: their synthetic
transport is built by the executor plane, on the machine that holds the argv and
the environment. An owner-bound credential resolved on the daemon has no meaning
there.

## Precedence

1. An `Authorization` header on the entry — any capitalization, any value.
2. Stored OAuth, when the factory exists and answers.
3. No auth at all, when the factory is absent or the owner has no binding but
   the server is public.

A stored grant is never consulted for an entry that names its own
`Authorization`. The reverse is also true: OAuth never *overwrites* an explicit
header. Unrelated configured headers are sent alongside a bearer, not instead of
it.

## Passive behaviour

The dial-time handler is passive. `Authorize` is only reached when the server
refuses a request (401/403), and it **never discovers, prompts or blocks**:
nothing interactive can run inside a spawn.

- **No stored credential** ⇒ a handler whose token source is nil. No
  `Authorization` is sent, and a public server connects and works normally.
- **401 / insufficient scope** ⇒ the handler returns an error carrying the named
  login hint, `server <name> needs a login: run `rafiki mcp login <name>``. It
  rides the existing skip log (`agent/tools: mcp: failed to connect to server,
  skipping`) and, on a later tool call, the tool-call error — never a global
  spawn failure, because one server needing a login must not cost the child its
  other tools. No server-supplied response text is copied into the hint.
- **Any other failure** stays non-interactive. There is no invisible retry and
  no fallback to an anonymous dial.
- **Credential-store failure** (database down, ciphertext unopenable) refuses
  that server. `ErrUnavailable` is never reported — or acted on — as a missing
  credential, so an outage cannot silently downgrade an authenticated endpoint.
- **Logout mid-session** takes effect on the next acquisition: the store is the
  authority on every call, not a cached token.

## Strict endpoint binding

A credential is bound to one **canonical endpoint** (scheme, host, port, path and
query preserved — an endpoint is not replaced by its origin, and `/mcp` is not
stripped) and validated against exactly one resource and issuer. A passive dial
uses that exact binding:

- it does **not** rediscover the resource and does **not** guess one from the
  endpoint's origin;
- a metadata claim alone cannot attach an existing credential to a new endpoint;
  a different endpoint needs its own login;
- logout removes every endpoint binding to the deleted grant.

## Header and credential containment

Configured MCP headers are attached to **exactly the configured endpoint** —
scheme, host (port included), path and query. Anything else is not that server,
whatever produced the URL, and in practice it was produced by a `Location`
header, because a redirect is the only way the client asks for a URL the entry
did not name.

`pkg/fundi/tools` `headerRoundTripper` enforces this on every request, including
hops Go's own redirect logic builds:

- a redirect hop receives none of the configured headers, not even a
  same-origin hop to a different path;
- a hop to a different **origin** also loses `Authorization`, which covers the
  bearer the SDK itself stamps on each request. This matters specifically
  because `net/http` copies `Authorization` across a redirect by *hostname*: a
  hop to the same name on another port would otherwise carry a token the
  transport never intended to send;
- the configured headers are **removed** from such a request, not merely not
  added — the client may already have copied them onto it.

The wrapper is never installed over `http.DefaultTransport` when the factory
supplied a client: the guarded transport is the base, so the destination policy
keeps applying underneath. The wrapper itself checks no destinations.

## The destination boundary (SSRF)

Every outbound leg of the flow — protected-resource metadata, authorization
server metadata, dynamic client registration, the code exchange, refresh and
revocation — and every authenticated request to the MCP endpoint go through one
dedicated client (`pkg/mcpoauth`): public HTTPS by default; loopback, RFC 1918,
IPv6 unique-local, link-local, CGNAT, documentation, benchmarking and multicast
space refused; every resolved address checked at dial time and on every redirect
hop (DNS rebinding); no proxy; bounded operation, response size and redirect
count.

An operator can relax the destination set only from `rafikid.toml`
(`[mcp_oauth]`), never from a project config, a login flag or remote metadata.
The SDK's own checks still apply: an operator allowance cannot make the SDK
accept a cleartext non-loopback discovery URL.

## Errors

Identity, not message text (`pkg/mcpoauth/store.go`):

| Error | Meaning |
|---|---|
| `ErrMissing` | genuinely nothing stored for this owner and endpoint/resource |
| `ErrLoginRequired` | a row exists but needs a fresh explicit login |
| `ErrStaleFence` | the generation captured is no longer current; begin again |
| `ErrUnavailable` | the store could not answer; never read as "no credential" |

The runtime adds one rule of its own: a factory error refuses the dial. It is
logged with the server name and the reason, that server's tools are not
registered, and the rest of the child's MCP servers are unaffected.

## Tests

`pkg/fundi/tools/mcp_test.go` — `TestMcpOAuth*`. They assert on the HTTP
requests a real SDK session sends to a fixture server: case-variant and empty
`Authorization` skipping the factory, unrelated headers coexisting with a
bearer, a public server working with no stored credential, the 401 hint on the
skip log, a credential-store failure sending zero requests, stdio and
`via_executor` never consulting the factory, header containment across a
redirect to another origin (same hostname, different port), and the redial
carrying the factory. `pkg/fundi/runtime_test.go` pins that
`RuntimeOptions.MCPHTTP` is what reaches `ConnectMCP`.
