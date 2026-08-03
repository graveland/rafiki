# Migrating from fundi to rafiki

The `fundi` and `fundid` binaries are now `rafiki` and `rafikid`. This was a
deliberate **hard cut**: no environment-variable aliases, no directory
fallbacks, no automatic migration. Nothing in the shipped tree reads an old
name. Run these steps once.

The database schema is untouched — `rafiki_schema_migrations` was already
correctly named, and no row is rewritten.

---

## 1. Stop and uninstall the old service

Do this **before** installing the new binaries, while the old ones are still on
your `$PATH`. The launchd label and systemd unit changed
(`dev.graveland.fundi` → `dev.graveland.rafiki`, `fundi.service` →
`rafiki.service`), so a new `service install` will not find or replace the old
registration — you would end up with two.

```sh
fundi service uninstall
```

If you have already replaced the binaries, remove the old registration by hand:

```sh
# macOS
launchctl bootout "gui/$(id -u)/dev.graveland.fundi" 2>/dev/null
rm -f ~/Library/LaunchAgents/dev.graveland.fundi.plist

# Linux
systemctl --user disable --now fundi.service 2>/dev/null
rm -f ~/.config/systemd/user/fundi.service
```

## 2. Discard the old state

Session records are disposable — the durable data lives in the database.

```sh
rm -rf ~/.config/fundi ~/.local/share/fundi ~/.local/state/fundi
rm -rf "${XDG_RUNTIME_DIR:?}/fundi"   # only if XDG_RUNTIME_DIR is set
```

Two more by hand:

- Remove the orphaned `fundi-helpers` extension from your pi config directory.
  The bundled extension is now installed as `rafiki-helpers`; the old copy will
  otherwise sit there being loaded alongside it.
- Rename any per-repo `<repo>/.fundi/skills` to `<repo>/.rafiki/skills`.

Your presets file moved leaf name but not location — it is still read from pi's
directory, because pi reads it too:
`~/.pi/agent/fundi-presets.json` → `~/.pi/agent/rafiki-presets.json`.

## 3. Rename your environment

```sh
sed -i '' -E \
  -e 's/FUNDI_AGENT_DB/RAFIKI_DB/' \
  -e 's/(FUNDI|RAFIKI)_PROXY_URL/RAFIKI_URL/' \
  -e 's/RAFIKI_PROXY_TOKEN/RAFIKI_TOKEN/' \
  -e 's/FUNDI_/RAFIKI_/' \
  .env
```

Order matters: `FUNDI_AGENT_DB` must be rewritten before the general `FUNDI_`
rule, or it lands on `RAFIKI_AGENT_DB` — a variable that does not exist.

**`FUNDI_PROXY_TOKEN` is deliberately absent from that `sed`.** It is the one
variable a script cannot rewrite, because which name it becomes depends on what
it currently *means*:

- if you also set a proxy URL, it was the token being **sent** to that host, and
  becomes `RAFIKI_TOKEN`;
- if the daemon serves the proxy face itself, it was a token being **accepted**,
  and becomes `RAFIKI_SERVE_TOKEN`.

Those two used to be the same variable wearing opposite meanings depending on
context. They are now separate, and neither is overloaded.

### What merged

| before | after |
|---|---|
| `FUNDI_AGENT_DB`, `RAFIKI_DB` | `RAFIKI_DB` (always the same database) |
| `FUNDI_PROXY_URL`, `RAFIKI_PROXY_URL`, `RAFIKI_URL` | `RAFIKI_URL` |
| `RAFIKI_PROXY_TOKEN`, `RAFIKI_TOKEN`, client half of `FUNDI_PROXY_TOKEN` | `RAFIKI_TOKEN` (what this process *presents*) |
| server half of `FUNDI_PROXY_TOKEN` | `RAFIKI_SERVE_TOKEN` (what the face *accepts*) |

`RAFIKI_PROXY_KINDS` and `RAFIKI_PROXY_LISTEN` keep the `PROXY_` infix — one
names which child kinds get routed, the other what address the face binds.
Neither is a URL or a credential.

The `PIC_*` / `PI_CONTROLLER_*` fallbacks from the *previous* rename are gone
too. `paths.Get` reads exactly one name now.

## 4. Rebuild and reinstall

```sh
make build && make build-attach
rafikid migrate --db "$RAFIKI_DB"   # schema unchanged; a no-op on a current database
rafiki service install
```

### The DSN moved out of the unit file

Installs from before this change wrote `RAFIKI_DB` into the launchd plist or
systemd unit, both of which are world-readable (0644) — and a postgres DSN
carries a password. It now goes to `~/.config/rafiki/service.env` (0600)
alongside the API keys.

Nothing rewrites an existing unit on its own. Re-run the install from a shell
that has the DSN:

```sh
set -a; . ./.env; set +a
rafiki service install
rafiki service restart
```

Then confirm the unit is clean:

```sh
grep -c RAFIKI_DB ~/Library/LaunchAgents/dev.graveland.rafiki.plist   # expect 0
grep -c RAFIKI_DB ~/.config/rafiki/service.env                        # expect 1
```

If the DSN's password was ever in a unit file, treat it as exposed and rotate it.

**Clear stale binaries from `bin/`.** `rafiki attach` resolves its TUI binary as
a sibling of whichever executable you ran, so a leftover `bin/fundi` will keep
working and quietly mask the migration:

```sh
rm -f bin/fundi bin/fundid bin/fundi-attach
```

---

## Command mapping

| before | after |
|---|---|
| `fundi <verb>` | `rafiki <verb>` |
| `fundid` | `rafikid` |
| `rafiki serve` | *gone — `rafikid` serves the proxy face itself* |
| `rafiki migrate` | `rafikid migrate` |
| `rafiki agent <verb>` | `rafikid agent <verb>` |
| `rafiki claude` | `rafiki claude` *(unchanged)* |
| `fundid agent` | `rafikid fundi` |
| `--kind agent` | `--kind fundi` |
| `--kind pi`, `--kind claude` | unchanged |

There is no longer a standalone proxy binary. `rafikid` serves the face on
`:8035` and gained everything `rafiki serve` could do that it previously could
not: a config file of named client tokens, configurable `openai_routes` and
`default_model`, `/metrics`, OTLP tracing, and `--dev`.

## Two things that do not migrate cleanly

**Auto-label prefix.** Reserved auto-labels changed from `fundi/*` to
`rafiki/*`, and labels are **persisted per child**. A child spawned before the
upgrade keeps `fundi/kind=…` on its record forever — nothing rewrites old rows.
A `--has-label rafiki/kind=…` filter written after upgrading will silently not
match it. Kill and forget pre-existing children, or expect the seam.

**Captured owner identity.** The proxy face's per-boot token identity changed
from `fundi-child` to `rafiki-child`. Historical database rows keep the old
string. This is deliberate: it is a captured identity, not a key, and rewriting
history to match a rename is worse than a visible discontinuity in it.

## Why `fundi` still appears

The word survives, narrowly and on purpose. It names the **native agent
runtime** — one of three child kinds — not the product:

- `rafiki create --kind fundi` (alongside `--kind pi` and `--kind claude`)
- `pkg/fundi/`, the runtime package
- `rafikid fundi`, the standalone one-child-on-stdio mode

Both names are Swahili and the split is deliberate: a *fundi* is the craftsman
who does the work; a *rafiki* is the friend who keeps the history. The proxy,
the capture store and the insights CLI are rafiki. The thing that actually runs
your code is fundi.
