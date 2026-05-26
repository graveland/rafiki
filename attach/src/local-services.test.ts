/**
 * Tests for local-services.ts — factory functions for Pi local services.
 */

import { describe, expect, it } from "bun:test";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
    buildLocalModelRegistry,
    buildLocalSessionManager,
    buildLocalSettingsManager,
} from "./local-services.ts";

// ─── Helpers ──────────────────────────────────────────────────────────────────

/** Write a minimal valid pi session JSONL to a temp file and return its path. */
function writeTempSession(entries: number): string {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pic-svc-test-"));
    const filePath = path.join(dir, "session.jsonl");

    const lines: string[] = [
        // Session header (not counted by getEntries())
        JSON.stringify({
            type: "session",
            version: 3,
            id: "test-sess-001",
            timestamp: "2026-01-01T00:00:00.000Z",
            cwd: "/tmp",
        }),
    ];

    for (let i = 0; i < entries; i++) {
        lines.push(
            JSON.stringify({
                type: "model_change",
                id: `entry-${i}`,
                parentId: i === 0 ? null : `entry-${i - 1}`,
                timestamp: `2026-01-01T00:00:${String(i + 1).padStart(2, "0")}.000Z`,
                provider: "anthropic",
                modelId: "claude-sonnet-4",
            })
        );
    }

    fs.writeFileSync(filePath, lines.join("\n") + "\n");
    return filePath;
}

// ─── buildLocalSessionManager ─────────────────────────────────────────────────

describe("buildLocalSessionManager", () => {
    it("empty path → in-memory SessionManager with zero entries", async () => {
        const sm = await buildLocalSessionManager("");
        expect(sm.getEntries()).toHaveLength(0);
    });

    it("missing file path → in-memory SessionManager with zero entries", async () => {
        const sm = await buildLocalSessionManager("/nonexistent/path/session.jsonl");
        expect(sm.getEntries()).toHaveLength(0);
    });

    it("existing JSONL with two entries → SessionManager reports both entries", async () => {
        const filePath = writeTempSession(2);
        const sm = await buildLocalSessionManager(filePath);
        expect(sm.getEntries()).toHaveLength(2);
    });

    it("existing JSONL with one entry → SessionManager reports that entry", async () => {
        const filePath = writeTempSession(1);
        const sm = await buildLocalSessionManager(filePath);
        expect(sm.getEntries()).toHaveLength(1);
        expect(sm.getEntries()[0]!.type).toBe("model_change");
    });
});

// ─── buildLocalSettingsManager + buildLocalModelRegistry ─────────────────────

describe("buildLocalSettingsManager", () => {
    it("smoke test — returns a SettingsManager without throwing", async () => {
        const settings = await buildLocalSettingsManager();
        // SettingsManager is a live object; we just check it constructed.
        expect(settings).toBeDefined();
        expect(typeof settings.getPackages).toBe("function");
    });
});

describe("buildLocalModelRegistry", () => {
    it("smoke test — returns a ModelRegistry without throwing", async () => {
        const settings = await buildLocalSettingsManager();
        const registry = await buildLocalModelRegistry(settings);
        expect(registry).toBeDefined();
        // getAll() returns the built-in model list even without configured auth.
        expect(Array.isArray(registry.getAll())).toBe(true);
        expect(registry.getAll().length).toBeGreaterThan(0);
    });
});
