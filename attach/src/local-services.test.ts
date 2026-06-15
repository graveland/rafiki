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
    seedSessionManagerFromFrames,
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

// ─── seedSessionManagerFromFrames ─────────────────────────────────────────────

describe("seedSessionManagerFromFrames", () => {
    // Realistic UserMessage / AssistantMessage shapes per pi-ai types.ts.
    const userMsg = {
        role: "user",
        content: [{ type: "text", text: "hi" }],
        timestamp: 1,
    };
    const assistantMsg = {
        role: "assistant",
        content: [{ type: "text", text: "yo" }],
        api: "anthropic",
        provider: "anthropic",
        model: "claude-sonnet-4",
        usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 },
        stopReason: "stop",
        timestamp: 2,
    };

    it("round-trips messages through appendMessage → buildSessionContext", () => {
        const frames = [{ type: "agent_end", messages: [userMsg, assistantMsg] }];
        const sm = seedSessionManagerFromFrames("/tmp/test", frames);

        const ctx = sm.buildSessionContext();
        expect(ctx.messages).toHaveLength(2);
        expect((ctx.messages[0] as { role: string }).role).toBe("user");
        expect((ctx.messages[1] as { role: string }).role).toBe("assistant");
        expect((ctx.messages[0] as { content: { text: string }[] }).content[0]!.text).toBe("hi");
        expect((ctx.messages[1] as { content: { text: string }[] }).content[0]!.text).toBe("yo");
    });

    it("uses the LAST frame carrying a messages array (full replay)", () => {
        const frames = [
            { type: "agent_end", messages: [userMsg] },
            { type: "agent_start" },
            { type: "agent_end", messages: [userMsg, assistantMsg] },
        ];
        const sm = seedSessionManagerFromFrames("/tmp/test", frames);
        expect(sm.buildSessionContext().messages).toHaveLength(2);
    });

    it("preserves appendable variants (bashExecution + custom)", () => {
        // Real BashExecutionMessage / CustomMessage shapes per pi messages.ts.
        const bashMsg = {
            role: "bashExecution",
            command: "ls",
            output: "a\nb",
            exitCode: 0,
            cancelled: false,
            truncated: false,
            timestamp: 4,
        };
        const customMsg = {
            role: "custom",
            customType: "note",
            content: "remember this",
            display: true,
            timestamp: 5,
        };
        const frames = [{ type: "agent_end", messages: [bashMsg, customMsg] }];
        const sm = seedSessionManagerFromFrames("/tmp/test", frames);

        const ctx = sm.buildSessionContext();
        expect(ctx.messages).toHaveLength(2);
        expect((ctx.messages[0] as { role: string }).role).toBe("bashExecution");
        expect((ctx.messages[1] as { role: string }).role).toBe("custom");
    });

    it("filters out non-appendable variants (branch/compaction summaries)", () => {
        const branchSummary = { role: "branchSummary", summary: "x", fromId: "f", timestamp: 3 };
        const frames = [{ type: "agent_end", messages: [userMsg, branchSummary, assistantMsg] }];
        const sm = seedSessionManagerFromFrames("/tmp/test", frames);

        const ctx = sm.buildSessionContext();
        expect(ctx.messages).toHaveLength(2);
        expect(ctx.messages.every((m) => (m as { role: string }).role !== "branchSummary")).toBe(
            true
        );
    });

    it("no frame with messages → empty SessionManager", () => {
        const frames = [{ type: "agent_start" }, { type: "message_update", delta: "x" }];
        const sm = seedSessionManagerFromFrames("/tmp/test", frames);
        expect(sm.buildSessionContext().messages).toHaveLength(0);
    });

    it("empty frames → empty SessionManager", () => {
        const sm = seedSessionManagerFromFrames("/tmp/test", []);
        expect(sm.buildSessionContext().messages).toHaveLength(0);
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
