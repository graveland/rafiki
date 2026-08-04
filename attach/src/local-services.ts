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
import { promises as fs } from "node:fs";
import { join } from "node:path";

/**
 * Construct a local SessionManager pointed at an existing session JSONL file.
 *
 * When the file is missing or the path is empty (e.g. daemon hasn't flushed the
 * session file yet), returns an in-memory SessionManager with zero entries.
 * The returned object is a read-only snapshot; we never call append/save on it.
 */
export async function buildLocalSessionManager(sessionFile: string): Promise<SessionManager> {
    if (!sessionFile || !(await fileExists(sessionFile))) {
        return SessionManager.inMemory(process.cwd());
    }
    return SessionManager.open(sessionFile);
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
    if (!messages) return sm;
    for (const msg of messages) {
        const role = (msg as { role?: unknown } | null)?.role;
        if (typeof role !== "string" || !APPENDABLE_ROLES.has(role)) continue;
        try {
            sm.appendMessage(msg as Parameters<SessionManager["appendMessage"]>[0]);
        } catch {
            // Skip anything appendMessage still rejects (best-effort seeding).
        }
    }
    return sm;
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
