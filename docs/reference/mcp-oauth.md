# MCP OAuth

How a remote MCP server that authenticates with OAuth 2.1 gets its credential,
and what the agent runtime does with it. Operator configuration — the
`[mcp_oauth]` destination policy in `rafikid.toml` — is in
[configuration](../guide/configuration.md#mcp-oauth).

This page documents the **runtime seam** (`pkg/fundi`): when a daemon-side
factory is consulted, and what the runtime guarantees about the answer. The
factory itself, and the durable credential store behind it, belong to
`pkg/mcpoauth` (engine, policy, canonical URLs) and `pkg/mcpauthdb` (Postgres
rows); where a rule below is theirs, it says so.

**No token ever enters a project file, a CLI invocation or a child process.**
`.mcp.json` describes a server; it never carries its authentication state. There
is no `"oauth": true` switch, `MCPServerConfig` gains no OAuth field, and a spawn
never starts an interactive flow — login is an explicit, separate act.

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

- `canonicalEndpoint` is the entry's `url` as written; the runtime does not
  normalize it. Canonicalizing (and refusing a URL that is not a canonical
  HTTP(S) one) is the factory's job, so that binding, login and logout all agree
  on one spelling.
- `headers` is the entry's configured header map, passed so the factory can
  apply it to that server's own requests. It is not a licence to send those
  headers to discovery, registration or token URLs.
- The returned `*http.Client` is **copied**, and only its `Transport` is
  replaced (see [containment](#header-and-credential-containment)); `Timeout`,
  `Jar` and `CheckRedirect` are preserved. Whatever destination guard the
  factory has must therefore live in the client's `Transport`. A `nil` client,
  or a client whose `Transport` is nil, means there is no guarded transport and
  `http.DefaultTransport` is used as the wrapper's base.
- An `error` **refuses that server**. It is not "no credential": a refused dial
  is logged and skipped, and the entry is never retried anonymously.

The handler and the client are independent: either may be nil on its own, and a
nil handler means requests are sent unauthenticated. A non-nil handler may in
turn return a **nil token source** — that is how a public server with no stored
credential keeps working.

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
| `via_executor` (command **or** proxy) | never |

Presence, not a non-empty value, is the switch: an explicit empty
`Authorization` is still the operator's decision about that entry, and OAuth
overwriting it would let the daemon's stored credential beat the project's
configuration. `Authorization` is matched case-insensitively because the key
comes from a JSON object and its spelling is the author's.

`via_executor` entries are excluded for a second reason: their transport is
built by the executor plane, on the machine that holds the argv and the
environment. An owner-bound credential resolved on the daemon has no meaning
there, and the executor path never reaches the factory — both the `command` and
the `proxy` shape.

## Precedence

1. An `Authorization` header on the entry — any capitalization, any value.
2. Stored OAuth, when the factory exists and answers.
3. No auth at all, when the factory is absent or the owner has no credential but
   the server is public.

A stored grant is never consulted for an entry that names its own
`Authorization`. The reverse is also true: OAuth never *overwrites* an explicit
header. Unrelated configured headers are sent alongside a bearer, not instead of
it.

## What the handler is expected to be

The runtime treats the handler as **passive**, because nothing interactive can
run inside a spawn. `Authorize` is only reached when the server refuses a
request (401/403), and the runtime requires it to return an error immediately —
never discover, prompt or block.

- A refusal with no usable credential produces an error carrying the named
  login hint, and that error is passed through **untouched**: it rides the
  existing skip log (`agent/tools: mcp: failed to connect to server, skipping`)
  at dial time, and the tool-call error on a later call — including after a
  session has been redialed. One server needing a login is a skipped server, not
  a failed spawn.
- Any other failure stays non-interactive. The runtime adds no retry of its own
  and never falls back to an anonymous dial.
- The runtime does not inspect the error to classify it. Distinguishing "nothing
  stored" from "logged out" from "store unavailable" is `pkg/mcpoauth`'s, and
  the hint wording (`server <name> needs a login: run `rafiki mcp login <name>``)
  is produced there; the runtime's contribution is that the text reaches the
  user intact through the paths above.
- Whether a revoked credential, an expired refresh or a logout takes effect on
  the next acquisition is likewise the factory's: the runtime asks again on
  every dial and every redial, and caches nothing across them.

## Header and credential containment

Configured MCP headers are attached to **exactly the configured endpoint** —
scheme, host (port included), escaped path and query. Anything else is not that
server, whatever produced the URL, and in practice it was produced by a
`Location` header, because a redirect is the only way this client asks for a URL
the entry did not name.

`pkg/fundi/tools` `headerRoundTripper` enforces this on every request, including
the hops Go's own redirect logic builds:

- a hop that is not the exact endpoint gets **none** of the configured headers;
- a hop to a different **origin** also loses `Authorization`, which covers the
  bearer the SDK itself stamps on each request. This is the case `net/http`
  cannot handle for us: it copies `Authorization` across a redirect by
  *hostname*, so a hop to the same name on another port would otherwise carry a
  token the transport never intended to send;
- the configured keys are **removed** from such a request, not merely not
  added. A request can arrive already carrying them — and `Authorization` does,
  from the client's own copy — and forwarding it would hand a static credential
  to whatever answered.

The wrapper is installed over the client's own `Transport` (or
`http.DefaultTransport` when that is nil), so the factory's destination guard
keeps running underneath. The wrapper itself checks no destinations.

### Behaviour change: redirects no longer carry configured headers

Before this seam, `headerRoundTripper` set the configured headers on **every**
request it carried, including redirect hops to any host — a static `X-Api-Key`
or a hand-pasted `Authorization` followed a `Location` anywhere. The exact-
endpoint rule closed that, and it applies to every direct-URL entry, not only
OAuth-backed ones.

The visible consequence is a **same-origin hop to a different path**: `/mcp` →
`/mcp/`, or a 307 to `/v2/mcp`, no longer carries the configured headers, and
such a server will answer 401/403 for that entry. This is deliberate — the entry
named one resource, and a credential for it is not a credential for whatever
answers elsewhere on the same host — but it is a change, and a server that
redirects the endpoint like that needs its `url` pointed at the final location.
A same-origin hop still carries the OAuth bearer (the origin did not change); a
cross-origin hop carries neither.

A default-port spelling difference (`h` and `h:443`) counts as a different
endpoint, which fails closed.

## What this seam does not do

The destination boundary (SSRF), the owner-scoped endpoint binding and the
refresh machinery are the factory's, and are documented where they live:

- **Destination policy** — public HTTPS by default; loopback, private,
  link-local and other non-public address space refused; every resolved address
  checked at dial time and on every redirect hop; no proxy; bounded response
  size and redirect count. Implemented in `pkg/mcpoauth`; the operator's
  allowlists are in [configuration](../guide/configuration.md#mcp-oauth). The
  runtime only requires that the client it is handed already enforces this, and
  never weakens it: it keeps the factory's transport as the wrapper's base and
  never substitutes `http.DefaultTransport`.
- **Binding** — a credential is bound to one canonical endpoint and one
  validated resource/issuer, and a passive dial uses that exact binding without
  rediscovery. The runtime's part is only to pass the configured URL verbatim
  on every dial and redial.
- **Refresh and generation fences** — the store is the authority on every call;
  a logout or a newer login wins over anything cached. Nothing in `pkg/fundi`
  holds a token across dials.

## Tests

`pkg/fundi/tools/mcp_test.go`, all `TestMcpOAuth*`, driven against a real
streamable-HTTP fixture server wherever the wire matters:

| Test | What it pins |
|---|---|
| `TestMcpOAuthAuthorizationHeaderSkipsTheFactory` | five spellings, including an empty value: factory never called, configured header on the wire |
| `TestMcpOAuthUnrelatedHeadersCoexistWithTheBearer` | a configured map with NO `Authorization` key still consults the factory; the unrelated header and the bearer travel on the same request, and the factory's transport stays the base |
| `TestMcpOAuthPublicServerWorksWithoutACredential` | nil token source, no `Authorization`, tools registered |
| `TestMcpOAuth401CarriesTheNamedLoginHintThroughTheSkipLog` | 401 at dial: hint on the skip line, no spawn failure |
| `TestMcpOAuth401AfterARedialCarriesTheSameLoginHint` | 401 on a **later** call after a redial: hint on the tool-call error |
| `TestMcpOAuthCredentialLookupFailureRefusesTheServer` | factory error: zero HTTP requests, no anonymous retry |
| `TestMcpOAuthFactoryIsNotCalledForStdioOrExecutor` | stdio, `via_executor.command` and `via_executor.proxy` never consult the factory |
| `TestMcpOAuthFactoryIsSuppliedOnRedial` | a 404-killed session redials through the factory and keeps its bearer |
| `TestMcpOAuthConfiguredHeadersStayOnTheExactEndpoint` | client-level: endpoint vs another path vs same-hostname-other-port |
| `TestMcpOAuthRedirectFromTheEndpointCarriesNoCredentials` | real SDK session, redirect **from** the configured endpoint: neither bearer nor configured header follows |
| `TestMcpOAuthNonEndpointRequestsStripConfiguredHeaders` | the `Del` loop on a cross-origin hop: the bearer is dropped and a request arriving with the configured keys loses them; a same-origin hop keeps them |
| `TestMcpOAuthEndpointMatchCoversEveryURLPart` | `sameMCPEndpoint`/`sameMCPOrigin` on query, port, path, scheme, host |
| `TestMcpOAuthWithoutAFactoryPreservesStandaloneBehaviour` | no factory: headers still sent, tools registered |
| `TestMcpOAuthNilFactoryClientLeavesPrivateServersOffTheDestinationPolicy` | a nil client from the factory leaves the entry on `http.DefaultTransport`, so a private 127.0.0.1 server still dials with no `Authorization`; a client the factory DOES return becomes the transport and refuses |
| `TestMcpOAuthRuntimeOptionThreadsTheFactory` (`pkg/fundi/runtime_test.go`) | `RuntimeOptions.MCPHTTP` is what reaches `ConnectMCP` |
