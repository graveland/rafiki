/**
 * Tests for client.ts: UDS JSONL client for the pi-controller daemon.
 *
 * Each test spins up a small in-process UDS server, exercises the Client,
 * then tears down cleanly.
 */

import { describe, expect, it, afterEach } from "bun:test";
import * as net from "node:net";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { Client, FrameSplitter } from "./client.ts";

// ─── Helpers ──────────────────────────────────────────────────────────────────

/** Creates a temp socket path that doesn't exist yet. */
function tempSock(): string {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pic-test-"));
    return path.join(dir, "ctrl.sock");
}

interface ServerHandle {
    sockPath: string;
    server: net.Server;
    close: () => Promise<void>;
}

/**
 * Starts a UDS server at a temp path. The handler is called for each
 * incoming connection. Returns control handles.
 */
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

/** Reads a complete JSONL line from a socket. */
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

/** Writes a JSONL frame to a socket. */
function writeLine(sock: net.Socket, obj: unknown): Promise<void> {
    return new Promise((resolve, reject) => {
        sock.write(JSON.stringify(obj) + "\n", "utf8", (err) => {
            if (err) reject(err);
            else resolve();
        });
    });
}

// ─── FrameSplitter unit tests ─────────────────────────────────────────────────

describe("FrameSplitter", () => {
    it("splits on LF", () => {
        const s = new FrameSplitter(1024);
        const frames = s.push(Buffer.from('{"a":1}\n{"b":2}\n'));
        expect(frames).toHaveLength(2);
        expect(frames[0]!.toString()).toBe('{"a":1}');
        expect(frames[1]!.toString()).toBe('{"b":2}');
    });

    it("strips trailing CR", () => {
        const s = new FrameSplitter(1024);
        const frames = s.push(Buffer.from('{"x":1}\r\n'));
        expect(frames).toHaveLength(1);
        expect(frames[0]!.toString()).toBe('{"x":1}');
    });

    it("buffers partial frames across chunks", () => {
        const s = new FrameSplitter(1024);
        const f1 = s.push(Buffer.from('{"a"'));
        expect(f1).toHaveLength(0);
        const f2 = s.push(Buffer.from(':1}\n'));
        expect(f2).toHaveLength(1);
        expect(f2[0]!.toString()).toBe('{"a":1}');
    });

    it("throws when buffer exceeds maxBytes", () => {
        const s = new FrameSplitter(10);
        // Push 11 bytes without a newline.
        expect(() => s.push(Buffer.from("12345678901"))).toThrow(
            /frame too large/
        );
    });

    it("does NOT split on \\r alone", () => {
        const s = new FrameSplitter(1024);
        const frames = s.push(Buffer.from("hello\rworld\n"));
        expect(frames).toHaveLength(1);
        // The \r is mid-frame, not a split point; only the trailing \r is stripped.
        expect(frames[0]!.toString()).toBe("hello\rworld");
    });
});

// ─── Client integration tests ─────────────────────────────────────────────────

describe("Client", () => {
    const clients: Client[] = [];

    afterEach(async () => {
        for (const c of clients.splice(0)) {
            await c.close().catch(() => {});
        }
    });

    // 1. request_round_trip
    it("request_round_trip", async () => {
        const srv = await startServer((conn) => {
            // Echo back every request as a ctrl_response with the same id.
            const onData = async (chunk: Buffer) => {
                const line = chunk.toString("utf8").trim();
                if (!line) return;
                const req = JSON.parse(line) as Record<string, unknown>;
                const resp = {
                    type: "ctrl_response",
                    command: req["type"],
                    id: req["id"],
                    success: true,
                    data: { echo: true },
                };
                await writeLine(conn, resp);
            };
            conn.on("data", onData);
        });

        const client = await Client.dial({ socket: srv.sockPath });
        clients.push(client);

        const resp = await client.request({ type: "ctrl_status" });
        expect(resp.type).toBe("ctrl_response");
        expect(resp.success).toBe(true);

        await client.close();
        await srv.close();
    });

    // 2. dial_failure_throws
    it("dial_failure_throws", async () => {
        const badPath = "/tmp/pic-test-nonexistent-" + Date.now() + ".sock";
        await expect(Client.dial({ socket: badPath })).rejects.toThrow();
    });

    // 3. subscribe_receives_events
    it("subscribe_receives_events", async () => {
        const events = [
            { type: "ctrl_child_spawned", childId: "a", pid: 1, cwd: "/", at: 0 },
            { type: "ctrl_child_status", childId: "a", status: "idle", previous: "spawning", at: 1 },
            { type: "ctrl_child_exited", childId: "a", exitCode: 0, lastStatus: "idle", duration: 1, at: 2 },
        ];

        const srv = await startServer(async (conn) => {
            // Wait a tick so client subscribe() is registered before frames arrive.
            await new Promise((r) => setTimeout(r, 20));
            for (const ev of events) {
                await writeLine(conn, ev);
            }
        });

        const client = await Client.dial({ socket: srv.sockPath });
        clients.push(client);

        const iter = client.subscribe();
        const received: Record<string, unknown>[] = [];
        for (let i = 0; i < events.length; i++) {
            const result = await iter.next();
            expect(result.done).toBe(false);
            received.push(result.value!);
        }
        // Call return() to clean up the iterator.
        await iter.return?.();

        expect(received).toHaveLength(3);
        expect(received[0]!["type"]).toBe("ctrl_child_spawned");
        expect(received[1]!["type"]).toBe("ctrl_child_status");
        expect(received[2]!["type"]).toBe("ctrl_child_exited");

        await client.close();
        await srv.close();
    });

    // 4. auto_id_assignment
    it("auto_id_assignment", async () => {
        let receivedId: string | undefined;

        const srv = await startServer(async (conn) => {
            const line = await readLine(conn);
            const req = JSON.parse(line) as Record<string, unknown>;
            receivedId = req["id"] as string;
            await writeLine(conn, {
                type: "ctrl_response",
                command: req["type"],
                id: req["id"],
                success: true,
            });
        });

        const client = await Client.dial({ socket: srv.sockPath });
        clients.push(client);

        // Send request without id.
        const resp = await client.request({ type: "ctrl_status" });
        expect(resp.type).toBe("ctrl_response");

        // Both sides agree on the assigned id.
        expect(receivedId).toBeTruthy();
        expect(resp.id).toBe(receivedId);

        await client.close();
        await srv.close();
    });

    // 4b. getRecent requests rendered frames
    it("getRecent requests rendered frames", async () => {
        let captured: Record<string, unknown> | undefined;

        const srv = await startServer(async (conn) => {
            const line = await readLine(conn);
            captured = JSON.parse(line) as Record<string, unknown>;
            await writeLine(conn, {
                type: "ctrl_response",
                command: captured["type"],
                id: captured["id"],
                success: true,
                data: { events: [] },
            });
        });

        const client = await Client.dial({ socket: srv.sockPath });
        clients.push(client);

        await client.getRecent("c1", 10);

        expect(captured!["type"]).toBe("ctrl_get_recent");
        expect(captured!["rendered"]).toBe(true);
        expect(captured!["limit"]).toBe(10);

        await client.close();
        await srv.close();
    });

    // 5. concurrent_requests_correlated
    it("concurrent_requests_correlated", async () => {
        const srv = await startServer(async (conn) => {
            // Echo each request as a response, with artificial small delays
            // to let requests interleave.
            const onData = async (chunk: Buffer) => {
                const lines = chunk.toString("utf8").split("\n").filter(Boolean);
                for (const line of lines) {
                    const req = JSON.parse(line) as Record<string, unknown>;
                    // Small jitter so responses arrive out-of-order.
                    const delay = Math.random() * 10;
                    setTimeout(async () => {
                        await writeLine(conn, {
                            type: "ctrl_response",
                            command: req["type"],
                            id: req["id"],
                            success: true,
                            data: { sentId: req["id"] },
                        });
                    }, delay);
                }
            };
            conn.on("data", onData);
        });

        const client = await Client.dial({ socket: srv.sockPath });
        clients.push(client);

        const N = 5;
        const promises = Array.from({ length: N }, (_, i) =>
            client.request({ type: "ctrl_status", tag: `req-${i}` })
        );
        const results = await Promise.all(promises);

        expect(results).toHaveLength(N);
        for (const r of results) {
            expect(r.success).toBe(true);
            const data = r.data as Record<string, unknown>;
            // Each response carries the id that was sent to it; confirm it matches.
            expect(r.id).toBe(data["sentId"] as string);
        }

        await client.close();
        await srv.close();
    });

    // 6. close_terminates_pending_requests
    it("close_terminates_pending_requests", async () => {
        const srv = await startServer((_conn) => {
            // Never respond — we'll close the client instead.
        });

        const client = await Client.dial({ socket: srv.sockPath, requestTimeoutMs: 60_000 });
        clients.push(client);

        const p = client.request({ type: "ctrl_status" });
        // Close while the request is in-flight.
        await client.close();

        await expect(p).rejects.toThrow(/closed/);
        await srv.close();
    });

    // 7. close_terminates_subscriber_iterators
    it("close_terminates_subscriber_iterators", async () => {
        const srv = await startServer((_conn) => {
            // No events — we'll close the client instead.
        });

        const client = await Client.dial({ socket: srv.sockPath });
        clients.push(client);

        const iter = client.subscribe();
        const drainPromise = (async () => {
            const results: unknown[] = [];
            for await (const ev of iter) {
                results.push(ev);
            }
            return results;
        })();

        await new Promise((r) => setTimeout(r, 20));
        await client.close();

        const results = await drainPromise;
        expect(results).toHaveLength(0);

        await srv.close();
    });
});
