/**
 * runtime.ts — RemoteAgentSessionRuntime
 *
 * Implements the AgentSessionRuntime shape for a remotely-owned pi child
 * managed by pi-controller.  Wraps a RemoteAgentSession and the cwd-bound
 * services that InteractiveMode expects.
 *
 * ─── Kill-on-exit ────────────────────────────────────────────────────────────
 *
 * Default dispose() behaviour: close the UDS connection.  The daemon's child
 * keeps running; the user can re-attach via `rafiki attach`.
 *
 * When constructed with killOnExit=true (set by CLI flag `--kill-on-exit`):
 * dispose() sends ctrl_kill to the daemon first, then closes the connection.
 * This matches native pi exit semantics.
 */

import type {
    AgentSession,
    AgentSessionRuntimeDiagnostic,
    AgentSessionServices,
    ModelRuntime,
    ResourceLoader,
    SessionManager,
    SettingsManager,
} from "@earendil-works/pi-coding-agent";
import {
    buildLocalModelRegistry,
    buildLocalSessionManager,
    buildLocalSettingsManager,
    seedSessionManagerFromFrames,
} from "./local-services.ts";
import type { Api, Model } from "@earendil-works/pi-ai";
import type { ReplacedSessionContext } from "./session.ts";
import { Client } from "./client.ts";
import { RemoteAgentSession } from "./session.ts";
import { controlToken, controlURL, envFlag, envValue } from "./env.ts";

// ─── Public types ─────────────────────────────────────────────────────────────

export interface ConnectOptions {
    socket?: string;
    childId: string;
    killOnExit?: boolean;
}

/** Shape returned by ctrl_get for a single child (spec §6.1). */
export interface ChildMetadata {
    childId: string;
    cwd: string;
    sessionId: string;
    sessionFile: string;
    sessionName?: string;
    model: string;
    thinking?: string;
    /**
     * The daemon's own model catalog answer for `model` (routing.ModelCatalog.ContextWindow,
     * backed by OpenRouter's live model list — the same source pricing already uses).
     * Undefined when the catalog has no entry: no catalog configured, a model
     * it hasn't seen, or a cold/stale cache. Preferred over pi's local static
     * catalog in resolveModelFromRegistry precisely because it covers models
     * pi's own generated catalog doesn't happen to list (e.g. OpenRouter-only
     * ids like "deepseek/deepseek-chat").
     */
    contextWindow?: number;
    maxCompletionTokens?: number;
}

// ─── RemoteAgentSessionRuntime ────────────────────────────────────────────────

/**
 * Owns a RemoteAgentSession plus its cwd-bound services.
 *
 * Conforms to the AgentSessionRuntime shape so it can be handed directly to
 * InteractiveMode's constructor (cast via `as unknown as AgentSessionRuntime`).
 */
export class RemoteAgentSessionRuntime {
    private readonly _client: Client;
    private readonly _session: RemoteAgentSession;
    private readonly _services: AgentSessionServices;
    private readonly _cwd: string;
    private readonly killOnExit: boolean;
    private rebindSession?: (session: AgentSession) => Promise<void>;
    private beforeSessionInvalidate?: () => void;
    private disposed = false;

    // ── Static factory ────────────────────────────────────────────────────────

    /**
     * Connect to the daemon, fetch child metadata, and return a fully
     * initialised runtime.
     */
    static async connect(opts: ConnectOptions): Promise<RemoteAgentSessionRuntime> {
        const remote = controlURL();
        const client = remote
            ? await Client.dialURL(remote, controlToken() ?? "")
            : await Client.dial({ socket: opts.socket });
        let meta: ChildMetadata;
        try {
            meta = await fetchChildMetadata(client, opts.childId);
        } catch (err) {
            await client.close();
            throw err;
        }

        const settingsManager = await buildLocalSettingsManager();
        const { modelRegistry, modelRuntime } = await buildLocalModelRegistry(settingsManager);
        let sessionManager = await buildLocalSessionManager(meta.sessionFile);

        // Scrollback bound: RAFIKI_ATTACH_TAIL (set by `rafiki attach --tail`),
        // -1 = all. Applied to both the claude seed fetch and primeHistory.
        const tail = resolveTailLimit(envValue("RAFIKI_ATTACH_TAIL", "PIC_ATTACH_TAIL"));

        // For children with no pi-format session file (claude), the manager is
        // empty and pi's renderInitialMessages() — which paints from
        // sessionManager.buildSessionContext() — would show no scrollback. Seed
        // it from the daemon's rendered history so the prior transcript paints.
        if (tail !== 0 && sessionManager.buildSessionContext().messages.length === 0) {
            try {
                const frames = await client.getRecent(opts.childId, tail);
                const seeded = seedSessionManagerFromFrames(meta.cwd, frames);
                if (seeded.buildSessionContext().messages.length > 0) {
                    sessionManager = seeded;
                }
            } catch (err) {
                if (envFlag("RAFIKI_ATTACH_DEBUG", "PIC_ATTACH_DEBUG")) {
                    console.error("[rafiki-attach] scrollback seed failed:", err);
                }
            }
        }

        const model = resolveModelFromRegistry(modelRegistry, meta.model, meta);
        const thinking = (meta.thinking ?? "medium") as "low" | "medium" | "high";

        const session = new RemoteAgentSession({
            client,
            childId: opts.childId,
            cwd: meta.cwd,
            sessionId: meta.sessionId,
            sessionFile: meta.sessionFile,
            sessionName: meta.sessionName,
            model,
            thinkingLevel: thinking,
            sessionManager,
            settingsManager,
            modelRuntime,
        });

        const services: AgentSessionServices = makeServicesStub({
            cwd: meta.cwd,
            settingsManager,
            modelRuntime,
        });

        const runtime = new RemoteAgentSessionRuntime({
            client,
            session,
            services,
            cwd: meta.cwd,
            killOnExit: opts.killOnExit ?? false,
        });

        // Tell the daemon to deliver events for this child to our connection.
        // Without this, the daemon's bus never routes anything to us.
        try {
            const subResp = await client.request({
                type: "ctrl_subscribe",
                childId: opts.childId,
            });
            if (!subResp.success) {
                await client.close();
                throw new Error(`ctrl_subscribe: ${subResp.error?.code ?? "unknown"}`);
            }
        } catch (err) {
            await client.close();
            throw err;
        }

        // Seed prior transcript, then begin consuming live events.
        await session.primeHistory(tail);
        session.start();

        return runtime;
    }

    // ── Constructor ───────────────────────────────────────────────────────────

    private constructor(args: {
        client: Client;
        session: RemoteAgentSession;
        services: AgentSessionServices;
        cwd: string;
        killOnExit: boolean;
    }) {
        this._client = args.client;
        this._session = args.session;
        this._services = args.services;
        this._cwd = args.cwd;
        this.killOnExit = args.killOnExit;
    }

    // ── AgentSessionRuntime getters ───────────────────────────────────────────

    get services(): AgentSessionServices {
        return this._services;
    }

    get session(): AgentSession {
        return this._session as unknown as AgentSession;
    }

    get cwd(): string {
        return this._cwd;
    }

    get diagnostics(): readonly AgentSessionRuntimeDiagnostic[] {
        return [];
    }

    get modelFallbackMessage(): string | undefined {
        return undefined;
    }

    // ── Callback registration (called by InteractiveMode) ─────────────────────

    setRebindSession(fn?: (session: AgentSession) => Promise<void>): void {
        this.rebindSession = fn;
    }

    /**
     * Set a synchronous callback that runs before the current session is
     * invalidated during a session-replacement flow.
     */
    setBeforeSessionInvalidate(fn?: () => void): void {
        this.beforeSessionInvalidate = fn;
    }

    // ── Session-replacement methods ───────────────────────────────────────────

    /**
     * Ask the daemon to switch the child to a different session file.
     *
     * The daemon intercepts the switch_session frame and re-spawns the child
     * against the new session.  Full TUI rebind is partially implemented:
     * the ctrl_send is forwarded but the local RemoteAgentSession object is
     * not rebuilt in v1 — the child state will update via incoming events.
     */
    async switchSession(
        sessionPath: string,
        options?: {
            cwdOverride?: string;
            withSession?: (ctx: ReplacedSessionContext) => Promise<void>;
        }
    ): Promise<{ cancelled: boolean }> {
        const resp = await this._client.request({
            type: "ctrl_send",
            childId: this._session.childId,
            frame: { type: "switch_session", sessionPath, cwdOverride: options?.cwdOverride },
        });
        if (!resp.success) {
            throw new Error(`switch_session: ${resp.error?.code ?? "unknown"}`);
        }
        if (this.rebindSession) {
            // TODO (Task 8): re-fetch metadata and rebuild RemoteAgentSession.
            console.warn("switch_session: rebind not fully implemented in rafiki-attach v1");
        }
        return { cancelled: false };
    }

    /**
     * Ask the daemon to start a fresh session for the child.
     */
    async newSession(
        _options?: {
            parentSession?: string;
            setup?: (sessionManager: SessionManager) => Promise<void>;
            withSession?: (ctx: ReplacedSessionContext) => Promise<void>;
        }
    ): Promise<{ cancelled: boolean }> {
        const resp = await this._client.request({
            type: "ctrl_send",
            childId: this._session.childId,
            frame: { type: "new_session" },
        });
        if (!resp.success) {
            throw new Error(`new_session: ${resp.error?.code ?? "unknown"}`);
        }
        return { cancelled: false };
    }

    /**
     * Fork the session at a given conversation entry.
     */
    async fork(
        entryId: string,
        options?: {
            position?: "before" | "at";
            withSession?: (ctx: ReplacedSessionContext) => Promise<void>;
        }
    ): Promise<{ cancelled: boolean; selectedText?: string }> {
        const resp = await this._client.request({
            type: "ctrl_send",
            childId: this._session.childId,
            frame: { type: "fork", entryId, position: options?.position ?? "before" },
        });
        if (!resp.success) {
            throw new Error(`fork: ${resp.error?.code ?? "unknown"}`);
        }
        return { cancelled: false };
    }

    /**
     * Not supported in rafiki-attach v1.  Use `rafiki create --resume <file> --detached` instead.
     */
    async importFromJsonl(
        _inputPath: string,
        _cwdOverride?: string
    ): Promise<{ cancelled: boolean }> {
        throw new Error(
            "importFromJsonl: not supported in rafiki-attach v1 — use `rafiki create --resume <file> --detached` instead"
        );
    }

    // ── Lifecycle ─────────────────────────────────────────────────────────────

    /**
     * Send ctrl_kill to the daemon for this child. Used by rafiki-attach when the
     * user opts to terminate the session at TUI exit. Does NOT close the
     * connection — call dispose() after.
     */
    async killChild(): Promise<void> {
        try {
            await this._client.request({
                type: "ctrl_kill",
                childId: this._session.childId,
            });
        } catch (err) {
            console.error("[rafiki-attach] kill-child failed:", err);
        }
    }

    /**
     * Tear down the runtime.
     *
     * Runs beforeSessionInvalidate, sends ctrl_kill if killOnExit is set
     * (backward-compat path), disposes the session, then closes the UDS
     * connection.
     */
    async dispose(): Promise<void> {
        if (this.disposed) return;
        this.disposed = true;

        if (this.beforeSessionInvalidate) {
            try {
                this.beforeSessionInvalidate();
            } catch (err) {
                console.error("beforeSessionInvalidate:", err);
            }
        }

        this._session.dispose();

        if (this.killOnExit) {
            await this.killChild();
        }

        // Best-effort unsubscribe before closing — the daemon cleans up on
        // connection close anyway, but being explicit is tidy.
        try {
            await this._client.request({
                type: "ctrl_unsubscribe",
                childId: this._session.childId,
            });
        } catch {
            // Ignore — we're tearing down regardless.
        }

        await this._client.close();
    }
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

/** Default scrollback replay when RAFIKI_ATTACH_TAIL is unset (direct rafiki-attach
 * invocation). Bounded — an unbounded fetch of a large history produced a
 * ctrl_get_recent response past the 16 MiB frame cap and killed the connect. */
export const DEFAULT_TAIL_LIMIT = 500;

/**
 * Parse a RAFIKI_ATTACH_TAIL value: N > 0 = last N events, 0 = none, -1 = all.
 * Unset/empty/garbage falls back to DEFAULT_TAIL_LIMIT.
 */
export function resolveTailLimit(raw: string | undefined): number {
    if (raw === undefined || raw === "") return DEFAULT_TAIL_LIMIT;
    const n = Number(raw);
    if (!Number.isFinite(n)) return DEFAULT_TAIL_LIMIT;
    return Math.trunc(n);
}

/** Fetch a child's metadata from the daemon via ctrl_get. */
async function fetchChildMetadata(client: Client, childId: string): Promise<ChildMetadata> {
    const resp = await client.request({ type: "ctrl_get", childId });
    if (!resp.success) {
        throw new Error(`ctrl_get ${childId}: ${resp.error?.code ?? "unknown"}`);
    }
    const data = resp.data as Record<string, unknown>;
    return {
        childId: data["childId"] as string,
        cwd: data["cwd"] as string,
        sessionId: data["sessionId"] as string,
        sessionFile: data["sessionFile"] as string,
        sessionName: data["name"] as string | undefined,
        model: data["model"] as string,
        thinking: data["thinking"] as string | undefined,
        contextWindow: data["contextWindow"] as number | undefined,
        maxCompletionTokens: data["maxCompletionTokens"] as number | undefined,
    };
}

/** The one ModelRegistry method resolveModelFromRegistry actually needs — narrowed so it's trivial to fake in a test. */
export interface ModelFinder {
    find(provider: string, modelId: string): Model<Api> | undefined;
}

/** The subset of ChildMetadata's daemon-catalog fields resolveModelFromRegistry consumes. */
export interface DaemonContextWindow {
    contextWindow?: number;
    maxCompletionTokens?: number;
}

/**
 * Resolve a model from the registry, splitting the daemon's provider-qualified
 * identifier string (e.g. "anthropic/claude-sonnet-4", "deepseek/deepseek-v4-pro")
 * on its FIRST "/" — mirroring rafiki's own convention exactly (see
 * pkg/fundi/config.go's splitModel: the id half may itself contain further
 * slashes, e.g. an OpenRouter "org/sub/model" id).
 *
 * A registry hit returns the real Model (contextWindow, reasoning, etc. all
 * populated); a miss (an OpenRouter-routed id pi's own catalog doesn't carry)
 * still returns provider+id split apart rather than the previous "v1" stub,
 * which packed the whole string into `id` and left `provider` unset —
 * InteractiveMode's footer reads `state.model.provider` directly and rendered
 * "(undefined)" for every fundi/claude child, with the 0% / 0-token context
 * stats that come from contextWindow being equally unset.
 *
 * daemon, when given, OVERRIDES contextWindow/maxTokens on top of whichever
 * of the above applied — the daemon's own model catalog (live OpenRouter
 * data, the same source pricing uses) is fresher and more complete than
 * pi's checked-in generated one, which is missing entries for real,
 * currently-served models (e.g. "deepseek/deepseek-chat": pi's catalog only
 * lists "deepseek-v4-flash"/"deepseek-v4-pro"). This is deliberately an
 * override, not just a miss-fallback: even a registry HIT should defer to
 * the daemon's fresher number when the daemon has one.
 */
export function resolveModelFromRegistry(registry: ModelFinder, modelStr: string, daemon?: DaemonContextWindow): Model<Api> {
    const slash = modelStr.indexOf("/");
    let model: Model<Api>;
    if (slash < 0) {
        model = { id: modelStr, name: modelStr } as unknown as Model<Api>;
    } else {
        const provider = modelStr.slice(0, slash);
        const id = modelStr.slice(slash + 1);
        model = registry.find(provider, id) ?? ({ id, provider, name: modelStr } as unknown as Model<Api>);
    }
    if (daemon?.contextWindow) {
        model = {
            ...model,
            contextWindow: daemon.contextWindow,
            ...(daemon.maxCompletionTokens ? { maxTokens: daemon.maxCompletionTokens } : {}),
        } as Model<Api>;
    }
    return model;
}

/**
 * Build the AgentSessionServices struct expected by InteractiveMode.
 *
 * The session itself owns real stubs for resourceLoader and extensionRunner;
 * the services object here satisfies the constructor contract.  The TUI reads
 * per-session state from the session getters, not from services.resourceLoader.
 */
function makeServicesStub(opts: {
    cwd: string;
    settingsManager: SettingsManager;
    modelRuntime: ModelRuntime;
}): AgentSessionServices {
    return {
        cwd: opts.cwd,
        agentDir: "",
        modelRuntime: opts.modelRuntime,
        settingsManager: opts.settingsManager,
        resourceLoader: {} as unknown as ResourceLoader,
        diagnostics: [],
    };
}
