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
    AuthStorage,
    ModelRegistry,
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
 * The `_settings` parameter is accepted to make the dependency order explicit
 * at the call site (registry builds after settings), but ModelRegistry has its
 * own AuthStorage — it doesn't take a SettingsManager directly.
 */
export async function buildLocalModelRegistry(_settings: SettingsManager): Promise<ModelRegistry> {
    const agentDir = getAgentDir();
    const authStorage = AuthStorage.create(join(agentDir, "auth.json"));
    return ModelRegistry.create(authStorage, join(agentDir, "models.json"));
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
