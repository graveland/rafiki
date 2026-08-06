/**
 * local-services.ts — factory functions for Pi's local service objects
 *
 * Constructs SessionManager, SettingsManager, and ModelRegistry pointed at
 * the same files the daemon-spawned pi child uses.  Pass these into
 * RemoteAgentSessionRuntime (which calls them internally).
 *
 * All three factories are intentionally lightweight — they read from the same
 * ~/.pi/agent/ that the daemon's pi child reads, so settings and credentials
 * agree across both processes.
 */

import {
    getAgentDir,
    ModelRegistry,
    ModelRuntime,
    SessionManager,
    SettingsManager,
} from "@earendil-works/pi-coding-agent";

// Re-export SessionManager for tests.
export { SessionManager };
import type { Usage } from "@earendil-works/pi-ai/compat";
import { promises as fs } from "node:fs";
import { join } from "node:path";

/**
 * Default zero-usage sentinel for assistant messages that lack usage data.
 * Pi's FooterComponent iterates getEntries() and unconditionally reads
 * entry.message.usage.input (etc.) for every assistant message — this stub
 * prevents a TypeError when the daemon session file or claude-translated
 * frames contain assistant messages without usage (e.g. pre-usage sessions,
 * some providers, or older pi versions).
 */
const ZERO_USAGE: Usage = {
    input: 0,
    output: 0,
    cacheRead: 0,
    cacheWrite: 0,
    totalTokens: 0,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
};

/**
 * Wrap a SessionManager so that getEntries() guarantees every assistant
 * message carries a Usage object. Pi's FooterComponent.render() and
 * AgentSession.getSessionStats() dereference message.usage without a guard;
 * this proxy prevents crashes on sessions whose assistant messages were
 * saved before usage was tracked or whose provider didn't return usage.
 */
export function sanitizeSessionManager(sm: SessionManager): SessionManager {
    return new Proxy(sm, {
        get(target, prop, receiver) {
            const value = Reflect.get(target, prop, receiver);
            if (prop !== "getEntries" || typeof value !== "function") return value;
            return function (this: unknown, ...args: unknown[]) {
                const entries = (value as () => unknown[]).apply(target, args);
                for (const entry of entries as Array<Record<string, unknown>>) {
                    const msg = entry["message"] as Record<string, unknown> | undefined;
                    if (entry["type"] === "message" && msg?.["role"] === "assistant" && msg["usage"] == null) {
                        msg["usage"] = ZERO_USAGE;
                    }
                }
                return entries;
            };
        },
    }) as SessionManager;
}

/**
 * Construct a local SessionManager pointed at an existing session JSONL file.
 *
 * When the file is missing or the path is empty (e.g. daemon hasn't flushed the
 * session file yet), returns an in-memory SessionManager with zero entries.
 * The returned object is a read-only snapshot; we never call append/save on it.
 */
export async function buildLocalSessionManager(sessionFile: string): Promise<SessionManager> {
    let sm: SessionManager;
    if (!sessionFile || !(await fileExists(sessionFile))) {
        sm = SessionManager.inMemory(process.cwd());
    } else {
        sm = SessionManager.open(sessionFile);
    }
    return sanitizeSessionManager(sm);
}

/**
 * Construct a SettingsManager reading from ~/.pi/agent/settings.json.
 *
 * The daemon's pi child reads the same file (it inherits HOME), so settings
 * stay in sync between our process and the daemon without any extra work.
 */
export async function buildLocalSettingsManager(): Promise<SettingsManager> {
    return SettingsManager.create(process.cwd());
}

/**
 * Construct a ModelRegistry backed by ~/.pi/agent/auth.json and models.json.
 *
 * Returns both the registry (for the session) and the underlying ModelRuntime
 * (for AgentSessionServices). The daemon's pi child reads the same files
 * (it inherits HOME), so settings and credentials agree across processes.
 */
export async function buildLocalModelRegistry(_settings: SettingsManager): Promise<{
    modelRegistry: ModelRegistry;
    modelRuntime: ModelRuntime;
}> {
    const agentDir = getAgentDir();
    const modelRuntime = await ModelRuntime.create({
        authPath: join(agentDir, "auth.json"),
        modelsPath: join(agentDir, "models.json"),
    });
    return {
        modelRegistry: new ModelRegistry(modelRuntime),
        modelRuntime,
    };
}

/**
 * Roles that pi's SessionManager.appendMessage() accepts:
 * Message (user/assistant/toolResult) | CustomMessage (custom) |
 * BashExecutionMessage (bashExecution). The branchSummary/compactionSummary
 * variants must NOT go through appendMessage (pi appends those via dedicated
 * methods as top-level entries), so they are filtered out here.
 */
const APPENDABLE_ROLES = new Set([
    "user",
    "assistant",
    "toolResult",
    "custom",
    "bashExecution",
]);

/**
 * Seed an in-memory SessionManager from the daemon's rendered history frames so
 * pi's renderInitialMessages() (which paints from sessionManager.buildSessionContext())
 * shows prior transcript for children with no pi-format session file (claude).
 *
 * Extracts the latest frame carrying a full `messages` array (the claude
 * translator replays the complete conversation on every agent_end) and appends
 * each appendable message in order. Best-effort: unsupported message variants
 * (branch/compaction summaries) are filtered by role; anything appendMessage
 * still rejects is skipped via try/catch.
 */
export function seedSessionManagerFromFrames(
    cwd: string,
    frames: Record<string, unknown>[]
): SessionManager {
    const sm = SessionManager.inMemory(cwd);
    // Find the last frame with a non-empty messages array — the claude
    // translator replays the full conversation on every agent_end, so the
    // latest such frame is the complete transcript.
    let messages: unknown[] | undefined;
    for (const f of frames) {
        const m = f["messages"];
        if (Array.isArray(m) && m.length > 0) messages = m;
    }
    if (!messages) return sanitizeSessionManager(sm);
    for (const msg of messages) {
        const role = (msg as { role?: unknown } | null)?.role;
        if (typeof role !== "string" || !APPENDABLE_ROLES.has(role)) continue;
        try {
            sm.appendMessage(msg as Parameters<SessionManager["appendMessage"]>[0]);
        } catch {
            // Skip anything appendMessage still rejects (best-effort seeding).
        }
    }
    return sanitizeSessionManager(sm);
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

async function fileExists(path: string): Promise<boolean> {
    try {
        await fs.access(path);
        return true;
    } catch {
        return false;
    }
}
