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
import type { ModelRegistry, SessionManager, SettingsManager } from "@earendil-works/pi-coding-agent";
import { RemoteAgentSessionRuntime } from "./runtime.ts";

// ─── Server harness (mirrors client.test.ts) ──────────────────────────────────

function tempSock(): string {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pic-rt-test-"));
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

/** Fake local services (not exercised by runtime.ts itself in v1). */
const fakeServices = {
    sessionManager: {} as unknown as SessionManager,
    settingsManager: {} as unknown as SettingsManager,
    modelRegistry: {} as unknown as ModelRegistry,
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
    it("connect with fake server — fetches metadata and exposes correct cwd", async () => {
        const captured: Array<Record<string, unknown>> = [];
        const srv = await startServer(makeHandler(captured));
        servers.push(srv);

        const runtime = await RemoteAgentSessionRuntime.connect({
            socket: srv.sockPath,
            childId: CHILD_ID,
            ...fakeServices,
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
            ...fakeServices,
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
            ...fakeServices,
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
            ...fakeServices,
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

    it("newSession — sends ctrl_send with new_session frame", async () => {
        const captured: Array<Record<string, unknown>> = [];
        const srv = await startServer(makeHandler(captured));
        servers.push(srv);

        const runtime = await RemoteAgentSessionRuntime.connect({
            socket: srv.sockPath,
            childId: CHILD_ID,
            ...fakeServices,
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
