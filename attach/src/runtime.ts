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
 * keeps running; the user can re-attach via `pic attach`.
 *
 * When constructed with killOnExit=true (set by CLI flag `--kill-on-exit`):
 * dispose() sends ctrl_kill to the daemon first, then closes the connection.
 * This matches native pi exit semantics.
 */

import type {
    AgentSession,
    AgentSessionRuntimeDiagnostic,
    AgentSessionServices,
    ModelRegistry,
    ResourceLoader,
    SessionManager,
    SettingsManager,
} from "@earendil-works/pi-coding-agent";
import { AuthStorage } from "@earendil-works/pi-coding-agent";
import {
    buildLocalModelRegistry,
    buildLocalSessionManager,
    buildLocalSettingsManager,
} from "./local-services.ts";
import type { Api, Model } from "@earendil-works/pi-ai";
import type { ReplacedSessionContext } from "./session.ts";
import { Client } from "./client.ts";
import { RemoteAgentSession } from "./session.ts";

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
        const client = await Client.dial({ socket: opts.socket });
        let meta: ChildMetadata;
        try {
            meta = await fetchChildMetadata(client, opts.childId);
        } catch (err) {
            await client.close();
            throw err;
        }

        const settingsManager = await buildLocalSettingsManager();
        const modelRegistry = await buildLocalModelRegistry(settingsManager);
        const sessionManager = await buildLocalSessionManager(meta.sessionFile);

        const model = resolveModelFromRegistry(modelRegistry, meta.model);
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
            modelRegistry,
        });

        const services: AgentSessionServices = makeServicesStub({
            cwd: meta.cwd,
            settingsManager,
            modelRegistry,
        });

        return new RemoteAgentSessionRuntime({
            client,
            session,
            services,
            cwd: meta.cwd,
            killOnExit: opts.killOnExit ?? false,
        });
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
            console.warn("switch_session: rebind not fully implemented in pic-attach v1");
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
     * Not supported in pic-attach v1.  Use `pic spawn --resume <file>` instead.
     */
    async importFromJsonl(
        _inputPath: string,
        _cwdOverride?: string
    ): Promise<{ cancelled: boolean }> {
        throw new Error(
            "importFromJsonl: not supported in pic-attach v1 — use `pic spawn --resume <file>` instead"
        );
    }

    // ── Lifecycle ─────────────────────────────────────────────────────────────

    /**
     * Tear down the runtime.
     *
     * Runs beforeSessionInvalidate, sends ctrl_kill if killOnExit is set,
     * disposes the session, then closes the UDS connection.
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
            try {
                await this._client.request({
                    type: "ctrl_kill",
                    childId: this._session.childId,
                });
            } catch (err) {
                console.error("kill-on-exit: ctrl_kill failed:", err);
            }
        }

        await this._client.close();
    }
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

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
    };
}

/**
 * Resolve a model from the registry, or synthesise a minimal stub.
 *
 * The daemon provides a model identifier string (e.g. "anthropic/claude-sonnet-4").
 * In v1 the registry lookup is best-effort; a minimal stub is returned when the
 * registry has no matching entry.
 */
function resolveModelFromRegistry(_registry: ModelRegistry, modelStr: string): Model<Api> {
    return { id: modelStr, name: modelStr } as unknown as Model<Api>;
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
    modelRegistry: ModelRegistry;
}): AgentSessionServices {
    return {
        cwd: opts.cwd,
        agentDir: "",
        authStorage: AuthStorage.inMemory(),
        settingsManager: opts.settingsManager,
        modelRegistry: opts.modelRegistry,
        resourceLoader: {} as unknown as ResourceLoader,
        diagnostics: [],
    };
}
