# Authentication

## First user: claiming a fresh daemon

There is no bootstrap mode: no listener accepts an unauthenticated command
that could mint a user, and Connect's `CreateUser` never mints an admin.
The ONLY first-admin path is `rafikid user create <name> --admin` on the
DAEMON HOST — it opens `RAFIKI_DB` directly, so it needs no running daemon,
no socket and no token:

```sh
rafikid user create brent --admin --token   # on the machine that can reach the database
```

The daemon with zero users logs this once at startup:
`no users exist: create one on this host with rafikid user create <name>
--admin --token`. With `--token` the plaintext token is printed exactly once (the
daemon stores only its digest) and cannot be recovered afterward; write it
into the operator profile's token file (`rafiki profile add <name> --url …
--token …` for a remote daemon). Without `--token` the user has no credential
until it logs in with `rafiki login` (next section) or someone runs `rafikid user
token mint <name>`. `rafiki user create` over Connect mints only ordinary
non-admin users and requires an admin credential — so a fresh pod has no
self-service claim path, by design. The stale-token recovery path for an
EXISTING user is `rafikid user token mint <name>` — it never authenticates
to a daemon, so a stale credential cannot block minting its replacement
(`rafikid user create` answers "already exists" for a user that is still
there).

## Single sign-on (`rafiki login`)

When the daemon has `oidc.toml` configured (`<ConfigDir>/oidc.toml`, loaded
by `pkg/oidclogin.LoadConfig`), a user authenticates through the daemon's
OIDC identity provider instead of holding a long-lived token:

```sh
rafiki login                # on the client machine; writes the token to the profile
rafiki login --no-browser   # headless: print the authorize URL, paste the redirect back
```

The client binds a loopback listener, gets an authorize URL from the daemon's
`Login` service, and the IdP's redirect completes the flow; every message
goes to stderr, stdout stays silent. `--no-browser` skips the browser only:
the callback listener still binds (so a pinned port must be free), and the flag
also accepts the redirect URL pasted on stdin (an `ssh -L` forward landing on
another machine, a headless box). The flow waits 10 minutes; a
pasted URL parses only for its query string.

**`oidc.toml` keys** (all validated at daemon startup; a broken file logs and
disables login rather than stopping the daemon — a missing file is the
ordinary state):

| Key | Meaning |
|---|---|
| `issuer` | the IdP's issuer URL — `https`, or plain `http` against loopback for tests |
| `client_id` | the OAuth client id registered at the IdP |
| `client_secret_env` | the NAME of the environment variable holding the client secret (e.g. `RAFIKI_OIDC_CLIENT_SECRET`, see `.env.example`); the variable must be set in the daemon's environment |
| `email_domains` | one or more bare domains (no `@`); the verified email must land on one. Entries are lowercased to match normalization |
| `session_ttl` | the minted token's lifetime (Go duration); default `12h` |
| `redirect_port` | `0` (default) = each client's own ephemeral loopback port; nonzero pins the callback port for every client |
| `scopes` | default `openid email profile` |

**Bootstrap.** Create the first user with an email on the daemon host, then
log in from the client:

```sh
rafikid user create brent --email brent@example.com --admin   # on the daemon host
rafiki login                                                  # on the client
```

The first login binds the identity by email: the IdP's verified email must
resolve to that active user, and the `(issuer, subject)` pair is stored —
every later login resolves by it, so an email change at the IdP does not
re-point the binding.

**Migrating an existing user.** An email can be added to an existing user on
the daemon host; the initial long-lived token is then revoked from the
client side:

```sh
rafikid user update brent --email brent@example.com   # on the daemon host
rafiki token list                                     # find the `initial` token's id
rafiki token revoke <id>                              # the token stops working at once
```

**Provider recipes.**

- **Vault** — an OIDC provider plus a CONFIDENTIAL client with redirect URI
  `http://127.0.0.1/oidc/callback`: Vault matches loopback redirects
  port-agnostically (RFC 8252's any-port loopback rule), so the client's
  ephemeral port works and `redirect_port` stays unset. The scope template
  must emit `email` (and optionally `email_verified`).
- **Okta** — a web app with the EXACT redirect URI
  `http://127.0.0.1:<port>/oidc/callback`, and `redirect_port = <port>` in
  `oidc.toml`: Okta does not implement RFC 8252's any-port loopback, so the
  daemon pins the port and every client must listen there (`rafiki login`
  rebinds automatically; a taken port tells you to free it and retry, since
  `--no-browser` still needs that port).

**Service tokens for automation.** Something that cannot walk an SSO flow (CI,
a script, a cron job) uses a minted token instead:

```sh
rafiki token mint --name ci --ttl 720h    # plaintext printed exactly once
rafiki token list                         # alias: ls; live tokens only, secrets never shown
rafiki token revoke <id>                  # immediate; cuts that token's open streams
```

(On the daemon host, the direct-DSN twins work with no running daemon:
`rafikid user update <name> --email …`, and `rafikid user token
mint|ls|revoke <name>`. The host-CLI `revoke` writes the database only — it
takes effect on new requests within the daemon's 5s auth-cache window and
does not interrupt streams already open on a running daemon; use `rafiki
token revoke` for that.)

**Executor ownership.** Every executor carries a durable `owner_user_id`, and
an executor serves only its owner's children: selection requires the child's
owner id to equal the executor's (NULL/empty on both sides counts as equal —
an unowned executor serves only unowned children), with no admin exception.
Token-less work on the local unix socket is UNOWNED — the anonymous socket
owns nothing — so it runs only on executors that were minted without a
token-profile identity. See `docs/reference/control-protocol.md`'s
"Executor administration" for the full rule.

## Provider logins at rest (`RAFIKI_SECRETS_KEY`)

A provider whose kind is `openai-responses-chatgpt` bills the user's own
ChatGPT subscription, so its credential is a per-user login rather than an API
key (see
[proxy.md](proxy.md#provider-subscription-logins-rafiki-providers-login)). What
that login returns is stored sealed, never usable in plaintext:

- **What is stored**, one row per user AND provider in
  `conversations.provider_credential`: the OAuth refresh token, the access token,
  the access token's expiry, the id of the key that sealed them, the ChatGPT
  account id, and that provider's cached model list with its etag. Only the two
  tokens are sealed; the model list is not a secret.
- **How**: AES-256-GCM under `RAFIKI_SECRETS_KEY` (base64 of exactly 32 bytes),
  with the associated data `owner_user_id|provider|field`, so a row copied to
  another owner, provider or field fails to open instead of yielding someone
  else's credential. No RPC returns a token and no child process is given one: a
  child gets a sender bound to its owner.
- **Where the key lives**: the daemon's `service.env` (0600), read with the real
  environment outranking the file. `rafiki setup` writes one on a machine that has
  none, carries an already EXPORTED one into the file (a hand-started daemon's key
  is the one that stays), and prints the path it wrote, so keep a copy of that
  file somewhere safe. Do not export an EMPTY `RAFIKI_SECRETS_KEY` in a shell you
  source: an empty value outranks the file, and the daemon then reports a key that
  is set as unset.

**Never rotate the key by replacing the value.** A new key opens nothing that was
sealed under the old one, so replacing it logs every user out of the ChatGPT
providers; rotating on purpose means re-registering every login afterwards.

A daemon that starts without the key (or with a value that is not base64 of 32
bytes) does not refuse to start: it runs, reports those providers UNAVAILABLE
with the reason, and refuses a login rather than storing a token unsealed. There
is no plaintext fallback and no daemon-wide credential.
