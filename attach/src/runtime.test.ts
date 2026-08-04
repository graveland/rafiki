/**
 * Tests for runtime.ts — RemoteAgentSessionRuntime.
 *
 * Each test spins up a minimal UDS server that:
 *   1. Responds to ctrl_get (required by connect()) with a fixed ChildSummary.
 *   2. Handles the specific command under test and records what it received.
 *
 * This mirrors the pattern in client.test.ts.
 */

import { describe, expect, it, afterEach } from "bun:test";
import * as net from "node:net";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import type { Api, Model } from "@earendil-works/pi-ai";
import {
    DEFAULT_TAIL_LIMIT,
    type ModelFinder,
    RemoteAgentSessionRuntime,
    resolveModelFromRegistry,
    resolveTailLimit,
} from "./runtime.ts";

// ─── Server harness (mirrors client.test.ts) ──────────────────────────────────

function tempSock(): string {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "rafiki-rt-test-"));
    return path.join(dir, "ctrl.sock");
}

interface ServerHandle {
    sockPath: string;
    server: net.Server;
    close: () => Promise<void>;
}

async function startServer(
    handler: (conn: net.Socket) => void
): Promise<ServerHandle> {
    const sockPath = tempSock();
    const server = net.createServer(handler);

    await new Promise<void>((resolve, reject) => {
        server.once("error", reject);
        server.listen(sockPath, resolve);
    });

    return {
        sockPath,
        server,
        close(): Promise<void> {
            return new Promise((resolve) => server.close(() => resolve()));
        },
    };
}

/** Read one JSONL line from a socket (LF-delimited). */
function readLine(sock: net.Socket): Promise<string> {
    return new Promise((resolve, reject) => {
        let buf = "";
        const onData = (chunk: Buffer) => {
            buf += chunk.toString("utf8");
            const nl = buf.indexOf("\n");
            if (nl !== -1) {
                sock.off("data", onData);
                sock.off("error", onError);
                resolve(buf.slice(0, nl));
            }
        };
        const onError = (err: Error) => {
            sock.off("data", onData);
            reject(err);
        };
        sock.on("data", onData);
        sock.once("error", onError);
    });
}

/** Write a JSONL frame to a socket. */
function writeLine(sock: net.Socket, obj: unknown): Promise<void> {
    return new Promise((resolve, reject) => {
        sock.write(JSON.stringify(obj) + "\n", "utf8", (err) => {
            if (err) reject(err);
            else resolve();
        });
    });
}

// ─── Shared test fixtures ─────────────────────────────────────────────────────

const CHILD_ID = "child-abc123";
const CHILD_SUMMARY = {
    childId: CHILD_ID,
    cwd: "/home/user/project",
    sessionId: "sess-001",
    sessionFile: "/home/user/.pi/sessions/sess-001.jsonl",
    name: "my-session",
    model: "anthropic/claude-sonnet-4",
    thinking: "medium",
};

/**
 * Build a server handler that:
 *   - Responds to the initial ctrl_get from connect()
 *   - Records all subsequent requests and responds with success=true
 *
 * `captured` is populated with every request object the server receives after
 * the initial ctrl_get handshake.
 */
function makeHandler(captured: Array<Record<string, unknown>>) {
    return (conn: net.Socket) => {
        let ctrlGetDone = false;

        conn.on("data", async (chunk: Buffer) => {
            // We may receive partial data; split on newlines.
            const lines = chunk.toString("utf8").split("\n").filter(Boolean);
            for (const line of lines) {
                let req: Record<string, unknown>;
                try {
                    req = JSON.parse(line) as Record<string, unknown>;
                } catch {
                    continue;
                }

                if (!ctrlGetDone) {
                    // First request must be ctrl_get.
                    ctrlGetDone = true;
                    await writeLine(conn, {
                        type: "ctrl_response",
                        command: "ctrl_get",
                        id: req["id"],
                        success: true,
                        data: CHILD_SUMMARY,
                    });
                } else {
                    // Subsequent requests: record and acknowledge.
                    captured.push(req);
                    await writeLine(conn, {
                        type: "ctrl_response",
                        command: String(req["type"] ?? ""),
                        id: req["id"],
                        success: true,
                    });
                }
            }
        });

        conn.on("error", () => {}); // ignore client-side disconnects
    };
}

// ─── Cleanup ──────────────────────────────────────────────────────────────────

const servers: ServerHandle[] = [];
afterEach(async () => {
    for (const s of servers.splice(0)) {
        await s.close();
    }
});

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("RemoteAgentSessionRuntime", () => {
    it("connect — sends ctrl_subscribe for the childId", async () => {
        const captured: Array<Record<string, unknown>> = [];
        const srv = await startServer(makeHandler(captured));
        servers.push(srv);

        const runtime = await RemoteAgentSessionRuntime.connect({
            socket: srv.sockPath,
            childId: CHILD_ID,
        });

        const subReqs = captured.filter((r) => r["type"] === "ctrl_subscribe");
        expect(subReqs).toHaveLength(1);
        expect(subReqs[0]!["childId"]).toBe(CHILD_ID);

        await runtime.dispose();
    });

    it("connect with fake server — fetches metadata and exposes correct cwd", async () => {
        const captured: Array<Record<string, unknown>> = [];
        const srv = await startServer(makeHandler(captured));
        servers.push(srv);

        const runtime = await RemoteAgentSessionRuntime.connect({
            socket: srv.sockPath,
            childId: CHILD_ID,
        });

        expect(runtime.cwd).toBe(CHILD_SUMMARY.cwd);
        expect(runtime.diagnostics).toEqual([]);
        expect(runtime.modelFallbackMessage).toBeUndefined();

        await runtime.dispose();
    });

    it("dispose default — ctrl_kill is NOT sent", async () => {
        const captured: Array<Record<string, unknown>> = [];
        const srv = await startServer(makeHandler(captured));
        servers.push(srv);

        const runtime = await RemoteAgentSessionRuntime.connect({
            socket: srv.sockPath,
            childId: CHILD_ID,
        });

        await runtime.dispose();

        const killReqs = captured.filter((r) => r["type"] === "ctrl_kill");
        expect(killReqs).toHaveLength(0);
    });

    it("dispose with killOnExit=true — ctrl_kill is sent before close", async () => {
        const captured: Array<Record<string, unknown>> = [];
        const srv = await startServer(makeHandler(captured));
        servers.push(srv);

        const runtime = await RemoteAgentSessionRuntime.connect({
            socket: srv.sockPath,
            childId: CHILD_ID,
            killOnExit: true,
        });

        await runtime.dispose();

        const killReqs = captured.filter((r) => r["type"] === "ctrl_kill");
        expect(killReqs).toHaveLength(1);
        expect(killReqs[0]!["childId"]).toBe(CHILD_ID);
    });

    it("switchSession — sends ctrl_send with switch_session frame", async () => {
        const captured: Array<Record<string, unknown>> = [];
        const srv = await startServer(makeHandler(captured));
        servers.push(srv);

        const runtime = await RemoteAgentSessionRuntime.connect({
            socket: srv.sockPath,
            childId: CHILD_ID,
        });

        const result = await runtime.switchSession("/home/user/.pi/sessions/other.jsonl");
        expect(result.cancelled).toBe(false);

        const sendReqs = captured.filter((r) => r["type"] === "ctrl_send");
        expect(sendReqs).toHaveLength(1);
        const frame = sendReqs[0]!["frame"] as Record<string, unknown>;
        expect(frame["type"]).toBe("switch_session");
        expect(frame["sessionPath"]).toBe("/home/user/.pi/sessions/other.jsonl");
        expect(sendReqs[0]!["childId"]).toBe(CHILD_ID);

        await runtime.dispose();
    });

    it("killChild — sends ctrl_kill without closing connection", async () => {
        const captured: Array<Record<string, unknown>> = [];
        const srv = await startServer(makeHandler(captured));
        servers.push(srv);

        const runtime = await RemoteAgentSessionRuntime.connect({
            socket: srv.sockPath,
            childId: CHILD_ID,
        });

        // Call killChild explicitly — the connection must stay open afterwards
        // so we can still dispose() cleanly.
        await runtime.killChild();

        // ctrl_kill should have been sent.
        const killReqs = captured.filter((r) => r["type"] === "ctrl_kill");
        expect(killReqs).toHaveLength(1);
        expect(killReqs[0]!["childId"]).toBe(CHILD_ID);

        // dispose() should not send a second ctrl_kill (killOnExit=false by default).
        await runtime.dispose();
        const killReqsAfter = captured.filter((r) => r["type"] === "ctrl_kill");
        expect(killReqsAfter).toHaveLength(1);
    });

    it("newSession — sends ctrl_send with new_session frame", async () => {
        const captured: Array<Record<string, unknown>> = [];
        const srv = await startServer(makeHandler(captured));
        servers.push(srv);

        const runtime = await RemoteAgentSessionRuntime.connect({
            socket: srv.sockPath,
            childId: CHILD_ID,
        });

        const result = await runtime.newSession();
        expect(result.cancelled).toBe(false);

        const sendReqs = captured.filter((r) => r["type"] === "ctrl_send");
        expect(sendReqs).toHaveLength(1);
        const frame = sendReqs[0]!["frame"] as Record<string, unknown>;
        expect(frame["type"]).toBe("new_session");
        expect(sendReqs[0]!["childId"]).toBe(CHILD_ID);

        await runtime.dispose();
    });
});

// ─── Tail limit ───────────────────────────────────────────────────────────────

describe("resolveTailLimit", () => {
    it("defaults to a bounded tail when unset or garbage", () => {
        expect(resolveTailLimit(undefined)).toBe(DEFAULT_TAIL_LIMIT);
        expect(resolveTailLimit("")).toBe(DEFAULT_TAIL_LIMIT);
        expect(resolveTailLimit("nope")).toBe(DEFAULT_TAIL_LIMIT);
    });

    it("passes through explicit values", () => {
        expect(resolveTailLimit("100")).toBe(100);
        expect(resolveTailLimit("0")).toBe(0);
        expect(resolveTailLimit("-1")).toBe(-1);
    });
});

// ─── Model resolution ───────────────────────────────────────────────────────
//
// Regression coverage for the footer showing "(undefined)" and "0.0%/0" for
// every fundi/claude child: resolveModelFromRegistry used to ignore its
// registry argument entirely and return { id: modelStr, name: modelStr },
// packing the whole "provider/id" string into `id` and leaving `provider`
// (and contextWindow, reasoning, ...) unset. InteractiveMode's footer reads
// state.model.provider directly.

describe("resolveModelFromRegistry", () => {
    it("returns the registry's real Model on a hit — provider/id split on the FIRST slash", () => {
        const found: Model<Api> = {
            provider: "deepseek",
            id: "deepseek-v4-pro",
            contextWindow: 128000,
        } as unknown as Model<Api>;
        const registry: ModelFinder = {
            find: (provider, modelId) => (provider === "deepseek" && modelId === "deepseek-v4-pro" ? found : undefined),
        };

        const model = resolveModelFromRegistry(registry, "deepseek/deepseek-v4-pro");

        expect(model).toBe(found);
    });

    it("on a registry miss, still splits provider from id instead of packing the whole string into id", () => {
        const registry: ModelFinder = { find: () => undefined };

        const model = resolveModelFromRegistry(registry, "moonshotai/kimi-k3");

        expect((model as unknown as { provider: string; id: string }).provider).toBe("moonshotai");
        expect((model as unknown as { provider: string; id: string }).id).toBe("kimi-k3");
    });

    it("splits on the FIRST slash only — an id with its own slashes stays whole", () => {
        const registry: ModelFinder = { find: () => undefined };

        const model = resolveModelFromRegistry(registry, "openrouter/org/sub-model");

        expect((model as unknown as { provider: string; id: string }).provider).toBe("openrouter");
        expect((model as unknown as { provider: string; id: string }).id).toBe("org/sub-model");
    });

    it("a bare id with no slash falls back to the old stub shape (no provider to split out)", () => {
        const registry: ModelFinder = { find: () => undefined };

        const model = resolveModelFromRegistry(registry, "no-provider-here");

        expect((model as unknown as { id: string }).id).toBe("no-provider-here");
        expect((model as unknown as { provider?: string }).provider).toBeUndefined();
    });

    // Regression for "deepseek-v4-pro gets 1M properly, deepseek-chat gets
    // 0.0%/0" — pi's checked-in generated catalog only lists two deepseek
    // models (v4-flash, v4-pro), so a registry miss for a real, currently
    // served model like "deepseek-chat" left contextWindow permanently
    // unset. The daemon's own catalog (live OpenRouter data) covers it.

    it("fills contextWindow/maxTokens from the daemon on a registry miss", () => {
        const registry: ModelFinder = { find: () => undefined };

        const model = resolveModelFromRegistry(registry, "deepseek/deepseek-chat", {
            contextWindow: 163840,
            maxCompletionTokens: 8192,
        });

        const m = model as unknown as { provider: string; id: string; contextWindow: number; maxTokens: number };
        expect(m.provider).toBe("deepseek");
        expect(m.id).toBe("deepseek-chat");
        expect(m.contextWindow).toBe(163840);
        expect(m.maxTokens).toBe(8192);
    });

    it("the daemon's contextWindow overrides a registry hit's own value — the daemon is fresher", () => {
        const found: Model<Api> = {
            provider: "deepseek", id: "deepseek-v4-pro", contextWindow: 999, name: "stale",
        } as unknown as Model<Api>;
        const registry: ModelFinder = { find: () => found };

        const model = resolveModelFromRegistry(registry, "deepseek/deepseek-v4-pro", { contextWindow: 1000000 });

        const m = model as unknown as { contextWindow: number; name: string };
        expect(m.contextWindow).toBe(1000000);
        expect(m.name).toBe("stale"); // everything else from the registry hit is preserved
    });

    it("no daemon data (undefined, or a response with contextWindow absent) leaves the registry/stub result untouched", () => {
        const registry: ModelFinder = { find: () => undefined };

        const withoutArg = resolveModelFromRegistry(registry, "moonshotai/kimi-k3");
        const withEmptyDaemon = resolveModelFromRegistry(registry, "moonshotai/kimi-k3", {});

        for (const model of [withoutArg, withEmptyDaemon]) {
            const m = model as unknown as { contextWindow?: number };
            expect(m.contextWindow).toBeUndefined();
        }
    });
});

describe("connect scrollback bound", () => {
    function withTailEnv(value: string | undefined, fn: () => Promise<void>): Promise<void> {
        const prev = process.env["RAFIKI_ATTACH_TAIL"];
        if (value === undefined) delete process.env["RAFIKI_ATTACH_TAIL"];
        else process.env["RAFIKI_ATTACH_TAIL"] = value;
        return fn().finally(() => {
            if (prev === undefined) delete process.env["RAFIKI_ATTACH_TAIL"];
            else process.env["RAFIKI_ATTACH_TAIL"] = prev;
        });
    }

    it("connect with RAFIKI_ATTACH_TAIL unset — every ctrl_get_recent carries the default limit", async () => {
        await withTailEnv(undefined, async () => {
            const captured: Array<Record<string, unknown>> = [];
            const srv = await startServer(makeHandler(captured));
            servers.push(srv);

            const runtime = await RemoteAgentSessionRuntime.connect({
                socket: srv.sockPath,
                childId: CHILD_ID,
            });

            const recentReqs = captured.filter((r) => r["type"] === "ctrl_get_recent");
            expect(recentReqs.length).toBeGreaterThan(0);
            for (const r of recentReqs) {
                expect(r["limit"]).toBe(DEFAULT_TAIL_LIMIT);
            }

            await runtime.dispose();
        });
    });

    it("connect with RAFIKI_ATTACH_TAIL=25 — the claude seed fetch honors it", async () => {
        await withTailEnv("25", async () => {
            const captured: Array<Record<string, unknown>> = [];
            const srv = await startServer(makeHandler(captured));
            servers.push(srv);

            const runtime = await RemoteAgentSessionRuntime.connect({
                socket: srv.sockPath,
                childId: CHILD_ID,
            });

            const recentReqs = captured.filter((r) => r["type"] === "ctrl_get_recent");
            expect(recentReqs.length).toBeGreaterThan(0);
            for (const r of recentReqs) {
                expect(r["limit"]).toBe(25);
            }

            await runtime.dispose();
        });
    });

    it("connect with RAFIKI_ATTACH_TAIL=0 — no history is fetched at all", async () => {
        await withTailEnv("0", async () => {
            const captured: Array<Record<string, unknown>> = [];
            const srv = await startServer(makeHandler(captured));
            servers.push(srv);

            const runtime = await RemoteAgentSessionRuntime.connect({
                socket: srv.sockPath,
                childId: CHILD_ID,
            });

            const recentReqs = captured.filter((r) => r["type"] === "ctrl_get_recent");
            expect(recentReqs).toHaveLength(0);

            await runtime.dispose();
        });
    });
});
