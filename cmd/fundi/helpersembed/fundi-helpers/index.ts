import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";

// ─── Inline type stubs ────────────────────────────────────────────────────────
//
// fundi-helpers is loaded in two contexts:
//   - daemon's pi child (via jiti): @earendil-works/pi-coding-agent available
//     through VIRTUAL_MODULES but NOT through normal module resolution.
//   - fundi-attach build (via bun import in session.ts): node_modules live in
//     attach/ which is a sibling, not a parent, of this file's location.
//
// Defining the types we need inline keeps fundi-helpers self-contained and avoids
// module-resolution issues in both contexts.

interface ExtensionCommandContext {
    reload(): Promise<void>;
    [key: string]: unknown;
}

interface ExtensionAPI {
    registerCommand(
        name: string,
        options: {
            description?: string;
            handler: (args: string, ctx: ExtensionCommandContext) => Promise<void>;
        }
    ): void;
    [key: string]: unknown;
}

/**
 * fundi-helpers — context-aware pi extension.
 *
 * Three contexts, gated on env vars set by the controller / fundi-attach:
 *
 * - Daemon's pi child (FUNDI_CHILD_ID set, FUNDI_ATTACH_TUI unset):
 *   registers /reload.  Pi's interactive /reload builtin is TUI-only — in
 *   --mode rpc there's no builtin handler, so an extension command is the
 *   only way for fundi-attach to trigger ctx.reload() server-side.
 *
 * - fundi-attach TUI (FUNDI_ATTACH_TUI=1): factory is a no-op.
 *   RemoteAgentSession.bindExtensions() calls setupTuiAutocomplete()
 *   directly to register an autocomplete provider that queries the daemon
 *   for available slash commands.
 *
 * - Anywhere else (e.g. user runs `pi` interactively against the same
 *   ~/.pi/agent/extensions/ directory): factory is a no-op.  Don't shadow
 *   pi's interactive /reload builtin.
 *
 * Single source file, three roles. fundi-attach relative-imports this file at
 * compile time via attach/src/session.ts. `fundi install-extension` writes it
 * to disk for the daemon's pi child to auto-discover.
 *
 * Type note: all types are defined inline above — no @earendil-works imports —
 * to keep fundi-helpers self-contained and avoid module-resolution issues in
 * both the daemon and the fundi-attach build contexts.
 */
export default function (pi: ExtensionAPI): void {
    // The child-id variable is set by the daemon, so it must be read under the
    // name the daemon actually sets. This test was checking only
    // PI_CONTROLLER_CHILD_ID, which the daemon stopped setting when the Go side
    // migrated to FUNDI_CHILD_ID — so inDaemonChild was permanently false and
    // /reload was never registered at all. Accept both spellings, exactly as
    // internal/envvar.Get does.
    const inDaemonChild =
        envIsSet("FUNDI_CHILD_ID", "PI_CONTROLLER_CHILD_ID") &&
        !envFlag("FUNDI_ATTACH_TUI", "PIC_ATTACH_TUI");
    if (inDaemonChild) {
        registerDaemonChildCommands(pi);
    }
    // Other contexts: no-op. TUI autocomplete is wired separately.
}

function registerDaemonChildCommands(pi: ExtensionAPI): void {
    pi.registerCommand("reload", {
        description: "Reload extensions, skills, prompts, and themes",
        handler: async (_args, ctx) => {
            await ctx.reload();
        },
    });
}

// ─── TUI autocomplete (fundi-attach context only) ───────────────────────────────

/** Minimal command info shape — mirrors RpcSlashCommand from pi's RPC types. */
interface CommandInfo {
    name: string;
    description?: string;
}

/**
 * Minimal AutocompleteProvider types defined inline to avoid a runtime
 * dependency on @earendil-works/pi-tui. Structurally identical to the real
 * types; TypeScript's structural typing lets us cast when needed.
 */
interface AutocompleteItem {
    value: string;
    label: string;
    description?: string;
}
interface AutocompleteSuggestions {
    items: AutocompleteItem[];
    prefix: string;
}
interface AutocompleteProvider {
    getSuggestions(
        lines: string[],
        cursorLine: number,
        cursorCol: number,
        options: { signal: AbortSignal; force?: boolean }
    ): Promise<AutocompleteSuggestions | null>;
    applyCompletion(
        lines: string[],
        cursorLine: number,
        cursorCol: number,
        item: AutocompleteItem,
        prefix: string
    ): { lines: string[]; cursorLine: number; cursorCol: number };
    shouldTriggerFileCompletion?(
        lines: string[],
        cursorLine: number,
        cursorCol: number
    ): boolean;
}

// ─── Environment and socket resolution ───────────────────────────────────────
//
// Deliberately duplicated from attach/src/{env,client}.ts rather than imported:
// this file is loaded via jiti inside the daemon's pi child, where attach/'s
// modules are not resolvable (see the header note). Keep these in step with
// those, and all of them in step with the Go side's internal/{envvar,paths}.

/** Mirrors envvar.Get: current name wins, old spelling still honoured. */
function envValue(name: string, legacy?: string): string | undefined {
    const current = process.env[name];
    if (current !== undefined && current !== "") return current;
    if (legacy === undefined) return undefined;
    const old = process.env[legacy];
    return old !== undefined && old !== "" ? old : undefined;
}

function envFlag(name: string, legacy?: string): boolean {
    return envValue(name, legacy) === "1";
}

function envIsSet(name: string, legacy?: string): boolean {
    return envValue(name, legacy) !== undefined;
}

/** The per-application leaf every XDG base directory gets. Mirrors paths.appName. */
const APP_NAME = "fundi";

/**
 * Resolve the daemon socket path the way the Go side does.
 *
 * This used to fall back to ~/.pi/run/controller.sock — pi-controller's socket.
 * fundi moved to XDG precisely so the two can coexist, so that fallback reached
 * the wrong daemon whenever the socket path was not exported explicitly.
 */
function defaultSocketPath(): string {
    const explicit = envValue("FUNDI_SOCKET", "PI_CONTROLLER_SOCKET");
    if (explicit) return explicit;

    // paths.RuntimeDir(): $XDG_RUNTIME_DIR/fundi when absolute, else the state
    // dir — XDG_RUNTIME_DIR is a Linux/systemd convention, normally unset on
    // macOS. Relative values are ignored, as the spec requires.
    const runtime = process.env["XDG_RUNTIME_DIR"];
    if (runtime && path.isAbsolute(runtime)) {
        return path.join(runtime, APP_NAME, "controller.sock");
    }
    const state = process.env["XDG_STATE_HOME"];
    if (state && path.isAbsolute(state)) {
        return path.join(state, APP_NAME, "controller.sock");
    }
    return path.join(os.homedir(), ".local", "state", APP_NAME, "controller.sock");
}

/**
 * Pure helper: apply daemon command filtering to autocomplete suggestions.
 *
 * Given the text the user typed up to the cursor, the cached daemon commands,
 * and any existing base suggestions, returns a filtered+merged suggestion set
 * or null when there is no slash-command prefix at the cursor position.
 *
 * Exported for unit testing; also used internally by setupTuiAutocomplete.
 */
export function filterCommandSuggestions(
    typedUpToCursor: string,
    cachedCommands: CommandInfo[],
    baseItems: AutocompleteItem[]
): { items: AutocompleteItem[]; prefix: string } | null {
    const match = typedUpToCursor.match(/(?:^|\s)(\/\S*)$/);
    if (!match) return null;

    const slashCmd = match[1]!; // e.g. "/" or "/r" or "/reload"
    const cmdName = slashCmd.slice(1); // strip leading "/"

    const daemonItems: AutocompleteItem[] = cachedCommands
        .filter((c) => c.name.startsWith(cmdName))
        .map((c) => ({
            // pi-tui's CombinedAutocompleteProvider.applyCompletion prepends '/'
            // when the prefix starts with one (slash-command branch).  Storing
            // the bare name avoids the resulting double slash.
            value: c.name,
            label: `/${c.name}`,
            description: c.description,
        }));

    if (daemonItems.length === 0) return null;

    return {
        items: [...baseItems, ...daemonItems],
        // prefix is what the TUI replaces when the user picks a completion.
        prefix: slashCmd,
    };
}

/**
 * Map claude's slash_commands (names only) into the CommandInfo shape the
 * autocomplete provider consumes. claude advertises no descriptions.
 */
export function slashCommandsToCommandInfo(names: string[]): CommandInfo[] {
    return names.map((name) => ({ name, description: undefined }));
}

/**
 * Wire the TUI autocomplete provider into the ExtensionUIContext.
 *
 * Called by RemoteAgentSession.bindExtensions() when uiContext is available.
 * Reads FUNDI_ATTACH_CHILD_ID and FUNDI_SOCKET from the environment
 * (both set by main.ts before the TUI starts). Fetches slash commands from
 * the daemon's pi child once at startup and caches them for the session.
 *
 * Returns a `refresh` function so callers can re-fetch the command list
 * (e.g. after /reload changes the daemon's set of extensions/skills/prompts).
 *
 * @param addProvider - uiContext.addAutocompleteProvider (bound to the context)
 */
export function setupTuiAutocomplete(
    addProvider: (factory: (current: unknown) => unknown) => void
): { refresh: () => Promise<void> } {
    const socketPath = defaultSocketPath();
    const childId = envValue("FUNDI_ATTACH_CHILD_ID", "PIC_ATTACH_CHILD_ID") ?? "";

    let cachedCommands: CommandInfo[] = [];

    const refresh = async (): Promise<void> => {
        if (!childId) return;
        try {
            // pi children: get_commands is a pi rpc-mode RPC that returns the
            // child's loaded extension/skill/prompt commands.
            //
            // claude children: claude (stream-json) has no such RPC, but the
            // daemon captures the slash_commands it advertises in its init
            // frame and serves them from its store via ctrl_get — no round-trip
            // to the child, no 30s timeout.
            //
            // Other backends: the TUI's built-in slash commands come from the
            // base provider regardless, so they simply get none here.
            const kind = await fetchChildKind(socketPath, childId);
            if (kind === "pi") {
                cachedCommands = await fetchCommandsFromDaemon(socketPath, childId);
            } else if (kind === "claude") {
                cachedCommands = slashCommandsToCommandInfo(
                    await fetchSlashCommandsFromDaemon(socketPath, childId)
                );
            } else {
                cachedCommands = [];
            }
        } catch (err: unknown) {
            console.error(
                "[fundi-helpers] failed to refresh daemon commands:",
                err instanceof Error ? err.message : String(err)
            );
        }
    };

    // Initial fetch — fire-and-forget so we don't block bindExtensions.
    void refresh();

    addProvider((current: unknown) => {
        const base = current as AutocompleteProvider;
        const provider: AutocompleteProvider = {
            async getSuggestions(lines, cursorLine, cursorCol, options) {
                const baseResult = await base.getSuggestions(lines, cursorLine, cursorCol, options);

                const line = lines[cursorLine] ?? "";
                const typed = line.slice(0, cursorCol);

                const merged = filterCommandSuggestions(
                    typed,
                    cachedCommands,
                    baseResult?.items ?? []
                );

                return merged ?? baseResult;
            },

            applyCompletion(lines, cursorLine, cursorCol, item, prefix) {
                // Delegate to the base provider; CombinedAutocompleteProvider's
                // applyCompletion replaces `prefix` with `item.value` in-place,
                // which is correct for both built-in and daemon-fetched commands.
                return base.applyCompletion(lines, cursorLine, cursorCol, item, prefix);
            },

            shouldTriggerFileCompletion(lines, cursorLine, cursorCol) {
                return (
                    base.shouldTriggerFileCompletion?.(lines, cursorLine, cursorCol) ?? false
                );
            },
        };
        return provider;
    });

    return { refresh };
}

// ─── One-shot daemon UDS client ───────────────────────────────────────────────

/**
 * Fetch a child's kind ("pi" | "claude" | ...) via a one-shot ctrl_get. This is
 * a daemon-level request answered from its store (not forwarded to the child),
 * so it is fast and reliable regardless of backend. Returns "" on any failure;
 * callers treat unknown as non-pi and skip the pi-only command fetch.
 */
async function fetchChildKind(socketPath: string, childId: string): Promise<string> {
    return new Promise<string>((resolve) => {
        const socket = net.createConnection(socketPath);
        let buf = "";
        let done = false;
        const finish = (kind: string): void => {
            if (done) return;
            done = true;
            clearTimeout(timer);
            socket.destroy();
            resolve(kind);
        };
        const timer = setTimeout(() => finish(""), 5_000);

        socket.on("connect", () => {
            socket.write(JSON.stringify({ type: "ctrl_get", childId }) + "\n");
        });
        socket.on("data", (chunk: Buffer) => {
            buf += chunk.toString("utf8");
            const lines = buf.split("\n");
            buf = lines.pop() ?? "";
            for (const line of lines) {
                if (!line.trim()) continue;
                let msg: Record<string, unknown>;
                try {
                    msg = JSON.parse(line) as Record<string, unknown>;
                } catch {
                    continue;
                }
                if (msg["type"] !== "ctrl_response") continue;
                const data = msg["data"] as { kind?: string } | undefined;
                finish(typeof data?.kind === "string" ? data.kind : "");
                return;
            }
        });
        socket.on("error", () => finish(""));
        socket.on("close", () => finish(""));
    });
}

/**
 * Fetch a claude child's advertised slash commands via a one-shot ctrl_get,
 * read from the daemon's store (not forwarded to the child). Returns [] on any
 * failure.
 */
async function fetchSlashCommandsFromDaemon(socketPath: string, childId: string): Promise<string[]> {
    return new Promise<string[]>((resolve) => {
        const socket = net.createConnection(socketPath);
        let buf = "";
        let done = false;
        const finish = (cmds: string[]): void => {
            if (done) return;
            done = true;
            clearTimeout(timer);
            socket.destroy();
            resolve(cmds);
        };
        const timer = setTimeout(() => finish([]), 5_000);

        socket.on("connect", () => {
            socket.write(JSON.stringify({ type: "ctrl_get", childId }) + "\n");
        });
        socket.on("data", (chunk: Buffer) => {
            buf += chunk.toString("utf8");
            const lines = buf.split("\n");
            buf = lines.pop() ?? "";
            for (const line of lines) {
                if (!line.trim()) continue;
                let msg: Record<string, unknown>;
                try {
                    msg = JSON.parse(line) as Record<string, unknown>;
                } catch {
                    continue;
                }
                if (msg["type"] !== "ctrl_response") continue;
                const data = msg["data"] as { slashCommands?: string[] } | undefined;
                const sc = data?.slashCommands;
                finish(Array.isArray(sc) ? sc : []);
                return;
            }
        });
        socket.on("error", () => finish([]));
        socket.on("close", () => finish([]));
    });
}

/**
 * Fetch slash commands from the daemon's pi child via a dedicated one-shot
 * UDS connection.
 *
 * Protocol:
 *  1. Connect to the daemon socket (no auth needed on UDS).
 *  2. ctrl_subscribe to the child so event frames are delivered to us.
 *  3. ctrl_send {type:"get_commands", id} to forward the RPC to the child.
 *     Sent immediately; the child's stdin buffers it if rebindSession is
 *     still running, so this is safe for cold-start.
 *  4. If the child emits {type:"ready"} (post-rebindSession signal),
 *     resend get_commands defensively in case the first frame was lost.
 *     Duplicate replies are tolerated: we resolve on the first.
 *  5. Wait for a ctrl_event wrapping the child's {type:"response",
 *     command:"get_commands", id} reply, parse the commands list, return.
 *
 * Times out after 30 s — generous enough to cover slow init (MCP servers,
 * extension loading) without hanging the autocomplete cache forever.
 */
async function fetchCommandsFromDaemon(
    socketPath: string,
    childId: string
): Promise<CommandInfo[]> {
    return new Promise<CommandInfo[]>((resolve, reject) => {
        const socket = net.createConnection(socketPath);
        let buf = "";
        const reqId = `gc-${Math.random().toString(36).slice(2, 10)}`;
        let done = false;

        const cleanup = (result: CommandInfo[] | Error): void => {
            if (done) return;
            done = true;
            clearTimeout(timer);
            socket.destroy();
            if (result instanceof Error) reject(result);
            else resolve(result);
        };

        const timer = setTimeout(() => {
            cleanup(new Error(`get_commands timed out for child ${childId}`));
        }, 30_000);

        const sendGetCommands = (): void => {
            if (done) return;
            socket.write(
                JSON.stringify({
                    type: "ctrl_send",
                    childId,
                    frame: { type: "get_commands", id: reqId },
                }) + "\n"
            );
        };

        socket.on("connect", () => {
            socket.write(JSON.stringify({ type: "ctrl_subscribe", childId }) + "\n");
            sendGetCommands();
        });

        socket.on("data", (chunk: Buffer) => {
            buf += chunk.toString("utf8");
            const lines = buf.split("\n");
            buf = lines.pop() ?? "";

            for (const line of lines) {
                if (!line.trim()) continue;
                let msg: Record<string, unknown>;
                try {
                    msg = JSON.parse(line) as Record<string, unknown>;
                } catch {
                    continue;
                }

                if (msg["type"] !== "ctrl_event") continue;
                if (msg["childId"] !== childId) continue;

                const ev = msg["event"] as Record<string, unknown> | undefined;
                if (!ev) continue;

                if (ev["type"] === "ready") {
                    sendGetCommands();
                    continue;
                }

                if (ev["type"] !== "response") continue;
                if (ev["command"] !== "get_commands") continue;
                if (ev["id"] !== reqId) continue;

                const data = ev["data"] as
                    | { commands?: Array<{ name: string; description?: string }> }
                    | undefined;
                const commands: CommandInfo[] = (data?.commands ?? []).map((c) => ({
                    name: c.name,
                    description: c.description,
                }));
                cleanup(commands);
            }
        });

        socket.on("error", (err: Error) => cleanup(err));

        // If the connection closes without a response (e.g. child exited),
        // return an empty list rather than leaving the promise pending.
        socket.on("close", () => cleanup([]));
    });
}
