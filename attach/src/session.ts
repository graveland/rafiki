/**
 * session.ts — RemoteAgentSession
 *
 * Proxies AgentSession operations over a UDS Client to the pi-controller
 * daemon, which forwards them to its daemon-owned pi child.
 *
 * Usage: cast to AgentSession at the boundary:
 *   `session as unknown as AgentSession`
 *
 * ═══════════════════════════════════════════════════════════════════════════
 * PUBLIC SURFACE ENUMERATION  (from agent-session.d.ts — 597 lines)
 * ═══════════════════════════════════════════════════════════════════════════
 *
 * GETTERS / PUBLIC PROPERTIES  (27 total)
 * ─────────────────────────────────────────────────────────────────────────
 *  [impl]  1. agent: Agent                           — readonly property
 *                                                      stub: abort() + waitForIdle() + signal
 *  [impl]  2. sessionManager: SessionManager         — readonly property
 *  [impl]  3. settingsManager: SettingsManager       — readonly property
 *  [impl]  4. modelRegistry: ModelRegistry           — getter
 *  [impl]  5. state: AgentState                      — getter (proxy to cached fields)
 *  [impl]  6. model: Model<any> | undefined          — getter
 *  [impl]  7. thinkingLevel: ThinkingLevel           — getter
 *  [impl]  8. isStreaming: boolean                   — getter
 *  [stub]  9. systemPrompt: string                   — getter (returns "")
 *  [impl] 10. retryAttempt: number                   — getter
 *  [impl] 11. isCompacting: boolean                  — getter
 *  [impl] 12. messages: AgentMessage[]               — getter
 *  [impl] 13. steeringMode: "all" | "one-at-a-time"  — getter
 *  [impl] 14. followUpMode: "all" | "one-at-a-time"  — getter
 *  [impl] 15. sessionFile: string | undefined        — getter
 *  [impl] 16. sessionId: string                      — getter
 *  [impl] 17. sessionName: string | undefined        — getter
 *  [stub] 18. scopedModels: ReadonlyArray<...>       — getter (returns [])
 *  [stub] 19. promptTemplates: ReadonlyArray<...>    — getter (returns [])
 *  [stub] 20. resourceLoader: ResourceLoader         — getter (stub object)
 *  [impl] 21. pendingMessageCount: number            — getter
 *  [stub] 22. autoCompactionEnabled: boolean         — getter (returns false)
 *  [impl] 23. isRetrying: boolean                    — getter
 *  [stub] 24. autoRetryEnabled: boolean              — getter (returns false)
 *  [stub] 25. isBashRunning: boolean                 — getter (returns false)
 *  [stub] 26. hasPendingBashMessages: boolean        — getter (returns false)
 *  [stub] 27. extensionRunner: ExtensionRunner       — getter (minimal stub)
 *
 * METHODS  (45 total)
 * ─────────────────────────────────────────────────────────────────────────
 *  [impl]  1. subscribe(listener): () => void
 *  [impl]  2. dispose(): void
 *  [stub]  3. getActiveToolNames(): string[]
 *  [stub]  4. getAllTools(): ToolInfo[]
 *  [stub]  5. getToolDefinition(name): ToolDefinition | undefined
 *  [stub]  6. setActiveToolsByName(names): void
 *  [impl]  7. prompt(text, opts?): Promise<void>
 *  [impl]  8. steer(text, images?): Promise<void>
 *  [impl]  9. followUp(text, images?): Promise<void>
 *  [v1-err]10. sendCustomMessage(msg, opts?): Promise<void>
 *  [impl] 11. sendUserMessage(content, opts?): Promise<void>
 *  [impl] 12. clearQueue(): { steering: string[]; followUp: string[] }
 *  [impl] 13. getSteeringMessages(): readonly string[]
 *  [impl] 14. getFollowUpMessages(): readonly string[]
 *  [impl] 15. abort(): Promise<void>
 *  [impl] 16. setModel(model): Promise<void>
 *  [stub] 17. cycleModel(dir?): Promise<ModelCycleResult | undefined>  — returns undefined (TUI shows "only one model")
 *  [impl] 18. setThinkingLevel(level): void
 *  [stub] 19. cycleThinkingLevel(): ThinkingLevel | undefined  — returns undefined (TUI shows "model does not support thinking")
 *  [stub] 20. getAvailableThinkingLevels(): ThinkingLevel[]
 *  [stub] 21. supportsThinking(): boolean
 *  [impl] 22. setSteeringMode(mode): void
 *  [impl] 23. setFollowUpMode(mode): void
 *  [impl] 24. compact(instructions?): Promise<CompactionResult>  — forwards /compact to daemon
 *  [stub] 25. abortCompaction(): void
 *  [stub] 26. abortBranchSummary(): void
 *  [stub] 27. setAutoCompactionEnabled(enabled): void
 *  [impl] 28. bindExtensions(bindings): Promise<void>
 *  [impl] 29. reload(): Promise<void>                             — forwards /reload to daemon
 *  [stub] 30. abortRetry(): void
 *  [stub] 31. setAutoRetryEnabled(enabled): void
 *  [v1-err]32. executeBash(cmd, onChunk?, opts?): Promise<BashResult>
 *  [stub] 33. recordBashResult(cmd, result, opts?): void
 *  [stub] 34. abortBash(): void
 *  [impl] 35. setSessionName(name): void
 *  [v1-err]36. navigateTree(id, opts?): Promise<...>               — /fork not supported
 *  [stub] 37. getUserMessagesForForking(): Array<...>
 *  [impl] 38. getSessionStats(): SessionStats
 *  [stub] 39. getContextUsage(): ContextUsage | undefined
 *  [v1-err]40. exportToHtml(path?): Promise<string>
 *  [v1-err]41. exportToJsonl(path?): string
 *  [impl] 42. getLastAssistantText(): string | undefined
 *  [v1-err]43. createReplacedSessionContext(): ReplacedSessionContext
 *  [stub] 44. hasExtensionHandlers(type): boolean
 *  [stub] 45. setScopedModels(models): void
 *
 * TOTAL: 27 getters/properties, 45 methods = 72 public members
 * IMPLEMENTED: 23 getters + 22 methods = 45
 * STUBBED:      4 getters + 23 methods = 27
 *
 * V1-UNSUPPORTED COMMANDS (throw with actionable message, TUI shows showError):
 *   sendCustomMessage  — no daemon protocol support
 *   executeBash (!)    — no streaming chunk relay; run bash in daemon session
 *   navigateTree       — /fork requires session branching not in v1
 *   exportToHtml       — session data lives in daemon; use daemon export
 *   exportToJsonl      — session data lives in daemon; use daemon export
 *   createReplacedSessionContext — session branching not in v1
 * ═══════════════════════════════════════════════════════════════════════════
 */

import type {
    Agent,
    AgentMessage,
    AgentState,
    AgentTool,
    ThinkingLevel,
} from "@earendil-works/pi-agent-core";
import type { ImageContent, Model, TextContent } from "@earendil-works/pi-ai";
import type {
    AgentSession,
    AgentSessionEvent,
    AgentSessionEventListener,
    BranchSummaryEntry,
    CompactionResult,
    ContextUsage,
    ExtensionCommandContext,
    ExtensionContext,
    ExtensionError,
    ExtensionRuntime,
    ExtensionRunner,
    ExtensionUIContext,
    ModelCycleResult,
    ModelRegistry,
    PromptOptions,
    PromptTemplate,
    ResolvedCommand,
    ResourceLoader,
    SessionManager,
    SessionStats,
    SettingsManager,
    ToolDefinition,
    ToolInfo,
} from "@earendil-works/pi-coding-agent";

// ReplacedSessionContext is not re-exported from the main @earendil-works/pi-coding-agent
// index so we mirror the structural minimum here.  The only call-site
// (createReplacedSessionContext) always throws, so this type is never
// materialised at runtime.
export type ReplacedSessionContext = ExtensionCommandContext & {
    sendMessage(message: unknown, options?: unknown): Promise<void>;
    sendUserMessage(content: unknown, options?: unknown): Promise<void>;
};

import type { Client } from "./client.ts";
import { setupTuiAutocomplete } from "../../cmd/pic/picembed/pic-helpers/index.ts";

// ─── Local type definitions (not exported from main package index) ────────────

/** Mirror of BashResult from core/bash-executor.ts */
export interface BashResult {
    output: string;
    exitCode: number | undefined;
    cancelled: boolean;
    truncated: boolean;
    fullOutputPath?: string;
}

/** Mirror of ExtensionBindings from core/agent-session.ts */
export interface ExtensionBindings {
    uiContext?: ExtensionUIContext;
    commandContextActions?: {
        waitForIdle: () => Promise<void>;
        [key: string]: unknown;
    };
    abortHandler?: () => void;
    shutdownHandler?: () => void;
    onError?: (error: ExtensionError) => void;
}

// ─── Init interface ───────────────────────────────────────────────────────────

/** Initialisation data for a RemoteAgentSession, sourced from ctrl_get. */
export interface RemoteSessionInit {
    client: Client;
    childId: string;
    cwd: string;
    sessionId: string;
    sessionFile?: string;
    sessionName?: string;
    model: Model<any>;
    thinkingLevel: ThinkingLevel;
    sessionManager: SessionManager;
    settingsManager: SettingsManager;
    modelRegistry: ModelRegistry;
}

// ─── Stub helpers ─────────────────────────────────────────────────────────────

/**
 * Minimal ResourceLoader stub.  Returns empty collections for everything.
 * Task 5 will wire up the real loader backed by the daemon's session JSONL.
 */
/**
 * Restore the terminal to a sane state on hard exit paths (e.g. when the
 * daemon broadcasts a shutdown event and we bypass pi's normal TUI teardown).
 *
 * Pi's TUI puts stdin into raw mode and switches to the alternate screen
 * buffer with cursor hidden and bracketed-paste/mouse tracking enabled.  A
 * naked process.exit() leaves the terminal in that state (no cursor, no echo,
 * stty sane needed) which is hostile.  This function writes the standard
 * restore sequences and re-cooks stdin.
 *
 * Exported so other entry points (main.ts's signal/error paths) can use it.
 */
export function restoreTerminal(): void {
    try {
        if (process.stdin.isTTY) {
            process.stdin.setRawMode(false);
        }
    } catch {
        // ignore — stdin may not be a TTY
    }
    if (process.stdout.isTTY) {
        // Show cursor, exit alternate screen, disable bracketed paste, disable
        // mouse tracking (1000/1002/1003/1006), pop kitty keyboard protocol
        // stack, disable xterm modifyOtherKeys, reset attributes.
        process.stdout.write(
            "\x1b[?25h" +    // DECTCEM show cursor
            "\x1b[?1049l" +  // exit alternate screen buffer
            "\x1b[?2004l" +  // disable bracketed paste
            "\x1b[?1000l" +  // disable X11 mouse
            "\x1b[?1002l" +  // disable cell motion mouse tracking
            "\x1b[?1003l" +  // disable all-motion mouse tracking
            "\x1b[?1006l" +  // disable SGR-encoded mouse
            "\x1b[<u" +      // pop kitty keyboard protocol stack
            "\x1b[>4;0m" +   // disable xterm modifyOtherKeys
            "\x1b[0m"        // reset SGR attributes
        );
    }
}

function makeResourceLoaderStub(): ResourceLoader {
    // Stub ExtensionRuntime: all action handlers throw until runner binds.
    const stubRuntime = {} as unknown as ExtensionRuntime;
    return {
        getExtensions: () => ({ extensions: [], errors: [], runtime: stubRuntime }),
        getSkills: () => ({ skills: [], diagnostics: [] }),
        getPrompts: () => ({ prompts: [], diagnostics: [] }),
        getThemes: () => ({ themes: [], diagnostics: [] }),
        getAgentsFiles: () => ({ agentsFiles: [] }),
        getSystemPrompt: () => undefined,
        getAppendSystemPrompt: () => [],
        extendResources: () => {},
        reload: () => Promise.resolve(),
    } as unknown as ResourceLoader;
}

/**
 * Minimal ExtensionRunner stub.
 *
 * Returns safe empty defaults for every method the TUI calls on startup so
 * InteractiveMode doesn't crash on bind.  Methods that cannot meaningfully
 * no-op throw with a clear message.
 */
function makeExtensionRunnerStub(): ExtensionRunner {
    const stub: Record<string, unknown> = {
        hasHandlers: () => false,
        hasUI: () => false,
        getExtensionPaths: () => [],
        getAllRegisteredTools: () => [],
        getToolDefinition: () => undefined,
        getFlags: () => new Map(),
        getFlagValues: () => new Map(),
        setFlagValue: () => {},
        getShortcuts: () => new Map(),
        getShortcutDiagnostics: () => [],
        getRegisteredCommands: () => [] as ResolvedCommand[],
        getCommandDiagnostics: () => [],
        getCommand: () => undefined,
        getMessageRenderer: () => undefined,
        onError: () => () => {},
        emitError: () => {},
        invalidate: () => {},
        // setUIContext is a no-op — we have no terminal UI in the daemon
        setUIContext: () => {},
        getUIContext: (): ExtensionUIContext => {
            throw new Error("extensionRunner.getUIContext: not available in pic-attach v1");
        },
        bindCore: () => {},
        bindCommandContext: () => {},
        shutdown: () => {},
        createContext: (): ExtensionContext => {
            throw new Error("extensionRunner.createContext: not available in pic-attach v1");
        },
        createCommandContext: (): ExtensionCommandContext => {
            throw new Error("extensionRunner.createCommandContext: not available in pic-attach v1");
        },
        emit: () => Promise.resolve(undefined),
        emitMessageEnd: () => Promise.resolve(undefined),
        emitToolResult: () => Promise.resolve(undefined),
        emitToolCall: () => Promise.resolve(undefined),
        emitUserBash: () => Promise.resolve(undefined),
        emitContext: (messages: AgentMessage[]) => Promise.resolve(messages),
        emitBeforeProviderRequest: (payload: unknown) => Promise.resolve(payload),
        emitBeforeAgentStart: () => Promise.resolve(undefined),
        emitResourcesDiscover: () =>
            Promise.resolve({ skillPaths: [], promptPaths: [], themePaths: [] }),
        emitInput: () => Promise.resolve({ action: "continue" as const }),
    };
    return stub as unknown as ExtensionRunner;
}

// ─── RemoteAgentSession ───────────────────────────────────────────────────────

export class RemoteAgentSession {
    // ── Public readonly properties (mirroring AgentSession) ─────────────────
    public readonly agent: Agent;
    public readonly sessionManager: SessionManager;
    public readonly settingsManager: SettingsManager;

    // ── Private infrastructure ───────────────────────────────────────────────
    private readonly _client: Client;
    private readonly _childId: string;
    private readonly _listeners = new Set<AgentSessionEventListener>();
    private _eventIter?: AsyncIterableIterator<Record<string, unknown>>;
    private _disposed = false;

    // ── Cached state (updated from incoming events) ──────────────────────────
    private _model: Model<any> | undefined;
    private _thinkingLevel: ThinkingLevel;
    private _isStreaming = false;
    private _isCompacting = false;
    private _isRetrying = false;
    private _retryAttempt = 0;
    private _messages: AgentMessage[] = [];
    private _sessionFile: string | undefined;
    private _sessionId: string;
    private _sessionName: string | undefined;
    private _steeringMessages: string[] = [];
    private _followUpMessages: string[] = [];
    private _steeringMode: "all" | "one-at-a-time" = "one-at-a-time";
    private _followUpMode: "all" | "one-at-a-time" = "one-at-a-time";
    private _streamingMessage: AgentMessage | undefined = undefined;
    private _errorMessage: string | undefined = undefined;

    // ── Stubs ────────────────────────────────────────────────────────────────
    private readonly _modelRegistry: ModelRegistry;
    private readonly _resourceLoader: ResourceLoader;
    private readonly _extensionRunner: ExtensionRunner;

    // Refresh handle from pic-helpers; called after /reload so the autocomplete
    // cache picks up newly-added skills/extensions/prompts.
    private _refreshAutocomplete?: () => Promise<void>;

    constructor(init: RemoteSessionInit) {
        this._client = init.client;
        this._childId = init.childId;
        this._sessionId = init.sessionId;
        this._sessionFile = init.sessionFile;
        this._sessionName = init.sessionName;
        this._model = init.model as Model<any>;
        this._thinkingLevel = init.thinkingLevel;

        this.sessionManager = init.sessionManager;
        this.settingsManager = init.settingsManager;
        this._modelRegistry = init.modelRegistry;
        this._resourceLoader = makeResourceLoaderStub();
        this._extensionRunner = makeExtensionRunnerStub();

        // Minimal Agent stub.  The TUI reads most state via AgentSession
        // getters (which we implement), but a few hot paths reach through to
        // session.agent directly:
        //   abort()       — Escape handler forwards abort to daemon
        //   waitForIdle() — extension commandContextActions call site
        //   signal        — extension shortcut context (reads, doesn't call)
        //   transport     — settings modal assigns on transport change (JS sets silently)
        const self = this;
        this.agent = {
            // ── abort ─────────────────────────────────────────────────────────
            // Fire-and-forget; the TUI's caller is synchronous.  Errors
            // surface via the unhandled-rejection path which we log.
            abort(): void {
                void self.abort().catch((err) => {
                    if (process.env.PIC_ATTACH_DEBUG === "1") {
                        console.error("[pic-attach] agent.abort forward failed:", err);
                    }
                });
            },

            // ── waitForIdle ───────────────────────────────────────────────────
            // Waits until the daemon's pi child is no longer actively streaming.
            // Extension command handlers (commandContextActions.waitForIdle) call
            // this to defer work until the agent finishes its current turn.
            waitForIdle(): Promise<void> {
                if (!self.isStreaming) {
                    return Promise.resolve();
                }
                return new Promise<void>((resolve) => {
                    const unsub = self.subscribe((ev) => {
                        if (ev.type === "agent_end") {
                            unsub();
                            resolve();
                        }
                    });
                });
            },

            // ── signal ────────────────────────────────────────────────────────
            // The active abort signal for the current run, or undefined when
            // idle. We cannot surface the daemon-side AbortSignal, so callers
            // always see undefined. Extension shortcuts treat undefined signal
            // as "no cancellation possible" and work correctly.
            get signal(): AbortSignal | undefined {
                return undefined;
            },
        } as unknown as Agent;

        // Start consuming events from the daemon.
        void this.consumeEvents();
    }

    // ── Event loop ────────────────────────────────────────────────────────────

    private async consumeEvents(): Promise<void> {
        this._eventIter = this._client.subscribe();
        const debug = process.env.PIC_ATTACH_DEBUG === "1";

        try {
            for await (const frame of this._eventIter) {
                try {
                    // Daemon-level broadcast — not tied to a specific child.
                    // Check before the per-child filter so it's never skipped.
                    if (frame["type"] === "ctrl_daemon_shutdown") {
                        const reason = (frame as { reason?: string }).reason ?? "unknown";
                        console.error(`[pic-attach] daemon shutting down (reason: ${reason})`);
                        // Self-signal SIGTERM instead of process.exit().  Signal
                        // delivery preempts the event loop — important here
                        // because pi's TUI is typically blocked on stdin and
                        // process.exit() from inside an async iterator can sit
                        // behind the stdin wait until the user hits a key.  Both
                        // pi's own SIGTERM handler (which cleanly tears down the
                        // TUI) and main.ts's gracefulExit will fire.
                        process.kill(process.pid, "SIGTERM");
                        return;
                    }

                    if (frame["type"] !== "ctrl_event") continue;
                    if (frame["childId"] !== this._childId) continue;
                    const inner = frame["event"] as Record<string, unknown>;
                    if (!inner || typeof inner !== "object") continue;

                    const ev = this.translate(inner);
                    if (ev === null) continue;

                    if (debug) {
                        console.error(
                            `[pic-attach] event: type=${ev.type} listeners=${this._listeners.size}`
                        );
                    }

                    this.updateCacheFromEvent(ev);
                    this.emit(ev);
                } catch (err) {
                    // Per-event error: log and continue. One bad event must not kill the loop.
                    console.error(
                        "[pic-attach] event processing error:",
                        err instanceof Error ? err.message : String(err),
                        "\n  frame type:", frame["type"],
                        "\n  childId:", frame["childId"],
                        "\n  event type:",
                        (frame["event"] as Record<string, unknown> | undefined)?.["type"],
                    );
                    if (debug && err instanceof Error && err.stack) {
                        console.error(err.stack);
                    }
                }
            }
        } catch (err) {
            // Iterator-level error: fires on connection close, expected during dispose().
            // Log regardless — even a noisy shutdown log beats silent data loss.
            if (!this._disposed) {
                console.error(
                    "[pic-attach] event iterator terminated unexpectedly:",
                    err instanceof Error ? err.message : String(err)
                );
            }
        }
    }

    /**
     * Translate a raw daemon event payload into an AgentSessionEvent.
     * The daemon forwards the pi child's RPC events verbatim inside
     * `ctrl_event.event`.  Most pass through unchanged; `agent_end` gains
     * the AgentSession-required `willRetry` field.
     */
    private translate(inner: Record<string, unknown>): AgentSessionEvent | null {
        const t = inner["type"] as string | undefined;
        if (!t) return null;

        switch (t) {
            case "agent_end": {
                // AgentSessionEvent's agent_end adds willRetry:boolean.
                const willRetry =
                    typeof inner["willRetry"] === "boolean" ? inner["willRetry"] : false;
                return {
                    ...inner,
                    type: "agent_end",
                    messages: (inner["messages"] as AgentMessage[] | undefined) ?? this._messages,
                    willRetry,
                } as AgentSessionEvent;
            }
            default:
                // Pass everything else through verbatim.  Extra fields are
                // harmless and the JSON shapes should match AgentSessionEvent members.
                return inner as unknown as AgentSessionEvent;
        }
    }

    /**
     * Update cached session state from an event.
     * Keeps local fields consistent so getters reflect real-time daemon state.
     */
    private updateCacheFromEvent(ev: AgentSessionEvent): void {
        switch (ev.type) {
            case "agent_start":
                this._isStreaming = true;
                this._streamingMessage = undefined;
                this._errorMessage = undefined;
                break;

            case "agent_end": {
                this._isStreaming = false;
                this._streamingMessage = undefined;
                if (ev.messages) {
                    this._messages = ev.messages;
                }
                this._isRetrying = (ev as { willRetry?: boolean }).willRetry === true;
                if (!this._isRetrying) {
                    this._retryAttempt = 0;
                }
                break;
            }

            case "message_start":
            case "message_update":
                if ("message" in ev) {
                    this._streamingMessage = ev.message;
                }
                break;

            case "message_end":
                if ("message" in ev) {
                    this._streamingMessage = undefined;
                    // Append/replace message in local cache
                    const msg = ev.message;
                    const existing = this._messages.findIndex(
                        (m) =>
                            (m as unknown as Record<string, unknown>)["id"] ===
                            (msg as unknown as Record<string, unknown>)["id"]
                    );
                    if (existing >= 0) {
                        this._messages = [
                            ...this._messages.slice(0, existing),
                            msg,
                            ...this._messages.slice(existing + 1),
                        ];
                    } else {
                        this._messages = [...this._messages, msg];
                    }
                }
                break;

            case "compaction_start":
                this._isCompacting = true;
                break;

            case "compaction_end":
                this._isCompacting = false;
                break;

            case "session_info_changed":
                if ("name" in ev) {
                    this._sessionName = (ev as { name?: string }).name;
                }
                break;

            case "thinking_level_changed":
                if ("level" in ev) {
                    this._thinkingLevel = (ev as { level: ThinkingLevel }).level;
                }
                break;

            case "queue_update":
                if ("steering" in ev && "followUp" in ev) {
                    const q = ev as { steering: readonly string[]; followUp: readonly string[] };
                    this._steeringMessages = [...q.steering];
                    this._followUpMessages = [...q.followUp];
                }
                break;

            case "auto_retry_start":
                this._isRetrying = true;
                if ("attempt" in ev) {
                    this._retryAttempt = (ev as { attempt: number }).attempt;
                }
                break;

            case "auto_retry_end":
                this._isRetrying = false;
                break;
        }
    }

    private emit(ev: AgentSessionEvent): void {
        for (const listener of this._listeners) {
            try {
                listener(ev);
            } catch (err) {
                console.error("[RemoteAgentSession] listener error:", err);
            }
        }
    }

    // ── Public getter: childId ──────────────────────────────────────────────

    /** Exposes the daemon child-id so RemoteAgentSessionRuntime can compose ctrl_ requests. */
    get childId(): string {
        return this._childId;
    }

    // ── Implemented: subscribe / dispose ─────────────────────────────────────

    /**
     * Subscribe to agent session events.
     * Returns an unsubscribe function for this listener.
     */
    subscribe(listener: AgentSessionEventListener): () => void {
        this._listeners.add(listener);
        return () => {
            this._listeners.delete(listener);
        };
    }

    /**
     * Remove all listeners and stop consuming events.
     * The UDS connection itself is closed by the runtime (Task 4).
     */
    dispose(): void {
        this._disposed = true;
        this._listeners.clear();
        void this._eventIter?.return?.();
    }

    // ── Implemented: prompting ────────────────────────────────────────────────

    /** Send a prompt to the daemon-owned pi child. */
    async prompt(text: string, options?: PromptOptions): Promise<void> {
        const resp = await this._client.request({
            type: "ctrl_send",
            childId: this._childId,
            frame: {
                type: "prompt",
                message: text,
                images: options?.images,
                streamingBehavior: options?.streamingBehavior,
                source: options?.source,
            },
        });
        if (!resp.success) {
            throw new Error(`prompt: ${resp.error?.code ?? "unknown error"}`);
        }
    }

    /** Queue a steering message to the daemon-owned pi child. */
    async steer(text: string, images?: ImageContent[]): Promise<void> {
        const resp = await this._client.request({
            type: "ctrl_send",
            childId: this._childId,
            frame: { type: "steer", message: text, images },
        });
        if (!resp.success) {
            throw new Error(`steer: ${resp.error?.code ?? "unknown error"}`);
        }
    }

    /** Queue a follow-up message to the daemon-owned pi child. */
    async followUp(text: string, images?: ImageContent[]): Promise<void> {
        const resp = await this._client.request({
            type: "ctrl_send",
            childId: this._childId,
            frame: { type: "follow_up", message: text, images },
        });
        if (!resp.success) {
            throw new Error(`followUp: ${resp.error?.code ?? "unknown error"}`);
        }
    }

    /**
     * Send a user message.  The deliverAs option maps to steer/followUp when
     * the child is streaming.
     */
    async sendUserMessage(
        content: string | (TextContent | ImageContent)[],
        options?: { deliverAs?: "steer" | "followUp" }
    ): Promise<void> {
        const resp = await this._client.request({
            type: "ctrl_send",
            childId: this._childId,
            frame: {
                type: "send_user_message",
                content,
                deliverAs: options?.deliverAs,
            },
        });
        if (!resp.success) {
            throw new Error(`sendUserMessage: ${resp.error?.code ?? "unknown error"}`);
        }
    }

    /** Abort the current agent operation. */
    async abort(): Promise<void> {
        const resp = await this._client.request({
            type: "ctrl_send",
            childId: this._childId,
            frame: { type: "abort" },
        });
        if (!resp.success) {
            throw new Error(`abort: ${resp.error?.code ?? "unknown error"}`);
        }
    }

    // ── Implemented: model / thinking ─────────────────────────────────────────

    /** Set the active model.  Sends to daemon; updates local cache on success. */
    async setModel(model: Model<any>): Promise<void> {
        const resp = await this._client.request({
            type: "ctrl_send",
            childId: this._childId,
            frame: {
                type: "set_model",
                provider: (model as unknown as Record<string, unknown>)["provider"],
                modelId: (model as unknown as Record<string, unknown>)["id"],
            },
        });
        if (!resp.success) {
            throw new Error(`setModel: ${resp.error?.code ?? "unknown error"}`);
        }
        this._model = model;
    }

    /**
     * Set thinking level.
     * Optimistic local update; daemon confirms via thinking_level_changed event.
     */
    setThinkingLevel(level: ThinkingLevel): void {
        this._thinkingLevel = level;
        void this._client.request({
            type: "ctrl_send",
            childId: this._childId,
            frame: { type: "set_thinking_level", level },
        });
    }

    // ── Implemented: queue controls ───────────────────────────────────────────

    clearQueue(): { steering: string[]; followUp: string[] } {
        const result = { steering: [...this._steeringMessages], followUp: [...this._followUpMessages] };
        this._steeringMessages = [];
        this._followUpMessages = [];
        return result;
    }

    getSteeringMessages(): readonly string[] {
        return this._steeringMessages;
    }

    getFollowUpMessages(): readonly string[] {
        return this._followUpMessages;
    }

    // ── Implemented: mode setters ─────────────────────────────────────────────

    setSteeringMode(mode: "all" | "one-at-a-time"): void {
        this._steeringMode = mode;
        void this._client.request({
            type: "ctrl_send",
            childId: this._childId,
            frame: { type: "set_steering_mode", mode },
        });
    }

    setFollowUpMode(mode: "all" | "one-at-a-time"): void {
        this._followUpMode = mode;
        void this._client.request({
            type: "ctrl_send",
            childId: this._childId,
            frame: { type: "set_follow_up_mode", mode },
        });
    }

    // ── Implemented: session info ─────────────────────────────────────────────

    setSessionName(name: string): void {
        this._sessionName = name;
        void this._client.request({
            type: "ctrl_send",
            childId: this._childId,
            frame: { type: "set_session_name", name },
        });
    }

    // ── Implemented: stats / text ─────────────────────────────────────────────

    getSessionStats(): SessionStats {
        const msgs = this._messages;
        const isRole = (m: unknown, role: string): boolean =>
            typeof (m as unknown as Record<string, unknown>)?.["role"] === "string" &&
            (m as unknown as Record<string, unknown>)["role"] === role;
        return {
            sessionFile: this._sessionFile,
            sessionId: this._sessionId,
            userMessages: msgs.filter((m) => isRole(m, "user")).length,
            assistantMessages: msgs.filter((m) => isRole(m, "assistant")).length,
            toolCalls: 0,
            toolResults: 0,
            totalMessages: msgs.length,
            tokens: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
            cost: 0,
        };
    }

    /**
     * Return text of the last assistant message.
     * Scans message cache in reverse; returns undefined if none found.
     */
    getLastAssistantText(): string | undefined {
        for (let i = this._messages.length - 1; i >= 0; i--) {
            const msg = this._messages[i] as unknown as Record<string, unknown>;
            if (msg?.["role"] !== "assistant") continue;
            const content = msg["content"];
            if (!Array.isArray(content)) continue;
            const text = (content as Array<Record<string, unknown>>)
                .filter((c) => c["type"] === "text")
                .map((c) => String(c["text"] ?? ""))
                .join("");
            if (text) return text;
        }
        return undefined;
    }

    // ── Getters ───────────────────────────────────────────────────────────────

    get modelRegistry(): ModelRegistry {
        return this._modelRegistry;
    }

    /**
     * Proxy AgentState built from our cached fields.
     * The TUI reads state.messages, state.isStreaming, etc.
     */
    get state(): AgentState {
        // eslint-disable-next-line @typescript-eslint/no-this-alias
        const self = this;
        let _tools: AgentTool<any>[] = [];
        return {
            get systemPrompt() { return ""; },
            get model() { return self._model as Model<any>; },
            set model(v: Model<any>) { self._model = v; },
            get thinkingLevel() { return self._thinkingLevel; },
            set thinkingLevel(v: ThinkingLevel) { self._thinkingLevel = v; },
            get tools() { return _tools; },
            set tools(v: AgentTool<any>[]) { _tools = [...v]; },
            get messages() { return self._messages; },
            set messages(v: AgentMessage[]) { self._messages = [...v]; },
            get isStreaming() { return self._isStreaming; },
            get streamingMessage() { return self._streamingMessage; },
            get pendingToolCalls(): ReadonlySet<string> { return new Set(); },
            get errorMessage() { return self._errorMessage; },
        } as AgentState;
    }

    get model(): Model<any> | undefined {
        return this._model;
    }

    get thinkingLevel(): ThinkingLevel {
        return this._thinkingLevel;
    }

    get isStreaming(): boolean {
        return this._isStreaming;
    }

    /** Stub: system prompt is not available client-side. */
    get systemPrompt(): string {
        return "";
    }

    get retryAttempt(): number {
        return this._retryAttempt;
    }

    get isCompacting(): boolean {
        return this._isCompacting;
    }

    get messages(): AgentMessage[] {
        return this._messages;
    }

    get steeringMode(): "all" | "one-at-a-time" {
        return this._steeringMode;
    }

    get followUpMode(): "all" | "one-at-a-time" {
        return this._followUpMode;
    }

    get sessionFile(): string | undefined {
        return this._sessionFile;
    }

    get sessionId(): string {
        return this._sessionId;
    }

    get sessionName(): string | undefined {
        return this._sessionName;
    }

    /** Stub: scoped models (--models flag) are not forwarded in v1. */
    get scopedModels(): ReadonlyArray<{ model: Model<any>; thinkingLevel?: ThinkingLevel }> {
        return [];
    }

    /** Stub: prompt templates are not loaded client-side in v1. */
    get promptTemplates(): ReadonlyArray<PromptTemplate> {
        return [];
    }

    get resourceLoader(): ResourceLoader {
        return this._resourceLoader;
    }

    get pendingMessageCount(): number {
        return this._steeringMessages.length + this._followUpMessages.length;
    }

    /** Stub: auto-compaction is managed by the daemon-owned pi child. */
    get autoCompactionEnabled(): boolean {
        return false;
    }

    get isRetrying(): boolean {
        return this._isRetrying;
    }

    /** Stub: auto-retry is managed by the daemon-owned pi child. */
    get autoRetryEnabled(): boolean {
        return false;
    }

    /** Stub: bash execution happens in the daemon-owned pi child. */
    get isBashRunning(): boolean {
        return false;
    }

    /** Stub: bash pending messages are in the daemon-owned pi child. */
    get hasPendingBashMessages(): boolean {
        return false;
    }

    get extensionRunner(): ExtensionRunner {
        return this._extensionRunner;
    }

    // ── Stubs ─────────────────────────────────────────────────────────────────

    /** Stub: tool list is managed by the daemon-owned pi child. */
    getActiveToolNames(): string[] {
        return [];
    }

    /** Stub: tool list is managed by the daemon-owned pi child. */
    getAllTools(): ToolInfo[] {
        return [];
    }

    /** Stub: tool definitions live in the daemon-owned pi child. */
    getToolDefinition(_name: string): ToolDefinition | undefined {
        return undefined;
    }

    /** Stub: active tool set is managed by the daemon-owned pi child. */
    setActiveToolsByName(_toolNames: string[]): void {
        // no-op: daemon-side concern
    }

    /** sendCustomMessage has no daemon protocol support in v1. */
    async sendCustomMessage(
        _message: unknown,
        _options?: unknown
    ): Promise<void> {
        throw new Error("sendCustomMessage is not supported in pic-attach v1.");
    }

    /**
     * Returns undefined — model cycling requires navigating the local model
     * registry, which pic-attach v1 does not have.  The TUI handles undefined
     * by showing "Only one model available" status, which is benign.
     * Use the model selector (Ctrl+M) or setModel() to change models.
     */
    async cycleModel(
        _direction?: "forward" | "backward"
    ): Promise<ModelCycleResult | undefined> {
        return undefined;
    }

    /**
     * Returns undefined — thinking-level cycling requires model metadata not
     * available client-side in v1.  The TUI handles undefined by showing
     * "Current model does not support thinking" status, which is benign.
     */
    cycleThinkingLevel(): ThinkingLevel | undefined {
        return undefined;
    }

    /** Stub: thinking-level list requires model metadata. */
    getAvailableThinkingLevels(): ThinkingLevel[] {
        return [];
    }

    /** Stub: thinking support requires model metadata. */
    supportsThinking(): boolean {
        return false;
    }

    /**
     * Forward /compact to the daemon's pi child as a prompt frame.
     * The child runs its own builtin handler; results flow back as
     * compaction_start / compaction_end events through the normal pipeline.
     * The TUI ignores the return value and catches errors, relying on events.
     */
    async compact(customInstructions?: string): Promise<CompactionResult> {
        const message = customInstructions ? `/compact ${customInstructions}` : "/compact";
        const resp = await this._client.request({
            type: "ctrl_send",
            childId: this._childId,
            frame: { type: "prompt", message },
        });
        if (!resp.success) {
            throw new Error(`compact: ${resp.error?.code ?? "unknown error"}`);
        }
        // Return value is ignored by handleCompactCommand; events carry the result.
        return {} as CompactionResult;
    }

    /** Stub: compaction abort is daemon-side. */
    abortCompaction(): void {
        // no-op in v1
    }

    /** Stub: branch-summary abort is daemon-side. */
    abortBranchSummary(): void {
        // no-op in v1
    }

    /** Stub: auto-compaction toggle is daemon-side. */
    setAutoCompactionEnabled(_enabled: boolean): void {
        // no-op in v1
    }

    /**
     * Bind TUI-side extensions.
     *
     * When uiContext is present (interactive mode), calls setupTuiAutocomplete
     * from pic-helpers to register the slash-command completion provider. This
     * is the only local extension we load; all other extension work happens in
     * the daemon's pi child.
     */
    async bindExtensions(bindings: ExtensionBindings): Promise<void> {
        if (bindings.uiContext) {
            // setupTuiAutocomplete reads PIC_ATTACH_CHILD_ID and PI_CONTROLLER_SOCKET
            // from the environment (set by main.ts before TUI construction).
            const { refresh } = setupTuiAutocomplete(
                // Cast to satisfy ExtensionUIContext.addAutocompleteProvider which expects
                // @earendil-works/pi-tui's AutocompleteProviderFactory. Our locally-typed
                // factory is structurally identical; runtime behaviour is correct.
                bindings.uiContext.addAutocompleteProvider.bind(bindings.uiContext) as (
                    factory: (current: unknown) => unknown
                ) => void
            );
            this._refreshAutocomplete = refresh;
        }
    }

    /**
     * Forward /reload to the daemon's pi child as a prompt frame.
     * The child runs its own reload handler (extensions, keybindings, etc.);
     * results flow back through the normal event pipeline.
     */
    async reload(): Promise<void> {
        const resp = await this._client.request({
            type: "ctrl_send",
            childId: this._childId,
            frame: { type: "prompt", message: "/reload" },
        });
        if (!resp.success) {
            throw new Error(`reload: ${resp.error?.code ?? "unknown error"}`);
        }
        // Re-fetch the autocomplete command list after a brief delay so the
        // daemon has time to finish its reload before we query get_commands.
        // Best-effort — a failure here only stales the autocomplete cache.
        if (this._refreshAutocomplete) {
            const refresh = this._refreshAutocomplete;
            setTimeout(() => {
                void refresh();
            }, 500);
        }
    }

    /** Stub: retry abort is daemon-side. */
    abortRetry(): void {
        // no-op in v1
    }

    /** Stub: auto-retry toggle is daemon-side. */
    setAutoRetryEnabled(_enabled: boolean): void {
        // no-op in v1
    }

    /**
     * Bash execution (! prefix) is not supported in pic-attach v1.  The
     * daemon executes bash in its own session; there is no streaming chunk
     * relay from daemon to client.  The TUI catches this error and shows it
     * via showError(), so the user sees an actionable message rather than a
     * crash.
     */
    async executeBash(
        _command: string,
        _onChunk?: (chunk: string) => void,
        _options?: unknown
    ): Promise<BashResult> {
        throw new Error(
            "Bash execution (! prefix) is not supported in pic-attach v1. Run bash commands in the daemon session instead."
        );
    }

    /** Stub: bash result recording is daemon-side. */
    recordBashResult(
        _command: string,
        _result: BashResult,
        _options?: unknown
    ): void {
        // no-op in v1
    }

    /** Stub: bash abort is daemon-side. */
    abortBash(): void {
        // no-op in v1
    }

    /**
     * Not supported in v1: /tree and /fork tree navigation require session
     * branching support that pic-attach does not yet implement.
     * Use `pic` shell commands to manage sessions.
     */
    async navigateTree(
        _targetId: string,
        _options?: unknown
    ): Promise<{
        editorText?: string;
        cancelled: boolean;
        aborted?: boolean;
        summaryEntry?: BranchSummaryEntry;
    }> {
        throw new Error("/tree and /fork navigation are not supported in pic-attach v1. Use `pic` shell commands to manage sessions.");
    }

    /** Stub: fork selector requires session JSONL tailing (Task 5). */
    getUserMessagesForForking(): Array<{ entryId: string; text: string }> {
        return [];
    }

    /** Stub: context usage is tracked by the daemon-owned pi child. */
    getContextUsage(): ContextUsage | undefined {
        return undefined;
    }

    /** /export and /share are not supported in pic-attach v1 — the session data lives in the daemon. */
    async exportToHtml(_outputPath?: string): Promise<string> {
        throw new Error("/export and /share are not supported in pic-attach v1. Export directly from the daemon session.");
    }

    /** /export is not supported in pic-attach v1 — the session data lives in the daemon. */
    exportToJsonl(_outputPath?: string): string {
        throw new Error("/export is not supported in pic-attach v1. Export directly from the daemon session.");
    }

    /** Not supported in v1: session branching is managed by the daemon. */
    createReplacedSessionContext(): ReplacedSessionContext {
        throw new Error("createReplacedSessionContext is not supported in pic-attach v1. Use `pic` shell commands to manage sessions.");
    }

    /**
     * Returns false — extensions run in the daemon's pi child, not here.
     */
    hasExtensionHandlers(_eventType: string): boolean {
        return false;
    }

    /** Stub: scoped models are not forwarded in v1. */
    setScopedModels(
        _scopedModels: Array<{ model: Model<any>; thinkingLevel?: ThinkingLevel }>
    ): void {
        // no-op in v1
    }
}
