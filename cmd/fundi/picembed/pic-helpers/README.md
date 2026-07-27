# pic-helpers

A pi extension that exposes fundi-attach-aware slash commands.

## Installed commands

| Command | Description |
|---|---|
| `/reload` | Reload extensions, skills, prompts, and themes (calls ctx.reload()) |

## Install

```bash
pic install-extension
```

This copies the extension to `~/.pi/agent/extensions/pic-helpers/`. Pi auto-discovers it from there for every run (daemon-managed or native).

## Uninstall

```bash
pic install-extension --remove
```

## Verify

Inside any pi session:

```text
/reload
```

Should reload extensions, skills, prompts, and themes (per pi's docs on `ctx.reload()`).

## Note

If you start pi with `--no-extensions`, pic-helpers will not load and `/reload` will not be available.
