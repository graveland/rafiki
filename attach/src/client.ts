/**
 * UDS JSONL client for the pi-controller daemon.
 *
 * Mirrors the shape of internal/client/client.go: Dial, request, subscribe,
 * close, plus the same request/response correlation model.
 *
 * Protocol: JSONL over Unix domain socket. LF-only framing (never split on
 * \r, U+2028, U+2029). Frame cap: 16 MB. Requests auto-assigned an id when
 * the caller omits one.
 */

import * as net from "node:net";
import * as os from "node:os";
import * as path from "node:path";
import { envValue } from "./env.ts";

// ─── Socket resolution ────────────────────────────────────────────────────────

/** The per-application leaf every XDG base directory gets. Mirrors paths.appName. */
const APP_NAME = "rafiki";

/**
 * Resolve the daemon socket path the way the Go side does.
 *
 * This MUST agree with internal/paths.SocketPath and client.DefaultSocketPath,
 * or the TUI dials a socket nobody is listening on. It previously fell back to
 * ~/.pi/run/controller.sock — which is *pi-controller's* socket. rafiki moved to
 * XDG specifically so the two could coexist, and this half never got the memo,
 * so attaching without an explicitly exported socket path reached the wrong
 * daemon (or none at all).
 *
 * $RAFIKI_SOCKET wins; rafiki's CLI injects it when it spawns this process. The
 * pre-rename $PI_CONTROLLER_SOCKET spelling is still honoured.
 */
export function defaultSocketPath(): string {
    const explicit = envValue("RAFIKI_SOCKET", "PI_CONTROLLER_SOCKET");
    if (explicit) return explicit;
    return path.join(runtimeDir(), "controller.sock");
}

/**
 * Mirrors paths.RuntimeDir(): $XDG_RUNTIME_DIR/rafiki when that is set to an
 * absolute path, else the state dir. XDG_RUNTIME_DIR is a Linux/systemd
 * convention and normally unset on macOS, hence the fallback rather than a
 * path outside the spec. A relative value is ignored, as the spec requires.
 */
function runtimeDir(): string {
    const runtime = process.env["XDG_RUNTIME_DIR"];
    if (runtime && path.isAbsolute(runtime)) return path.join(runtime, APP_NAME);
    const state = process.env["XDG_STATE_HOME"];
    if (state && path.isAbsolute(state)) return path.join(state, APP_NAME);
    return path.join(os.homedir(), ".local", "state", APP_NAME);
}

// ─── Public types ─────────────────────────────────────────────────────────────

export interface ClientOptions {
    /** Defaults to defaultSocketPath(): $RAFIKI_SOCKET, else the XDG runtime path. */
    socket?: string;
    /** Default 30 000 ms */
    requestTimeoutMs?: number;
    /** Default 16 MiB */
    maxFrameBytes?: number;
}

export interface Response {
    type: "ctrl_response";
    command: string;
    id?: string;
    success: boolean;
    data?: unknown;
    error?: { code: string; message?: string };
}

/**
 * Renders a failed ctrl_auth response (auth_invalid, auth_required, ...) as
 * an Error, for dispatchFrame to stash when nobody is waiting to receive the
 * response frame itself. Mirrors pkg/client/client.go's authError.
 */
function authError(resp: Response): Error {
    if (!resp.error) return new Error("auth: rejected");
    if (!resp.error.message) return new Error(`auth: ${resp.error.code}`);
    return new Error(`auth: ${resp.error.code}: ${resp.error.message}`);
}

// ─── Frame splitter ───────────────────────────────────────────────────────────

/**
 * Accumulates raw bytes and yields complete JSONL frames.
 *
 * Splits exclusively on 0x0A (LF). Strips a trailing 0x0D (CR) from each
 * frame for defensive Windows-line-ending tolerance. Throws if the
 * accumulated unparsed buffer exceeds maxBytes.
 */
export class FrameSplitter {
    private buf: Buffer = Buffer.alloc(0);

    constructor(private readonly maxBytes: number) {}

    push(chunk: Buffer): Buffer[] {
        this.buf = Buffer.concat([this.buf, chunk]);
        const frames: Buffer[] = [];
        let start = 0;
        for (let i = 0; i < this.buf.length; i++) {
            if (this.buf[i] === 0x0a) {
                // LF
                let end = i;
                // Strip trailing CR defensively.
                if (end > start && this.buf[end - 1] === 0x0d) end -= 1;
                frames.push(this.buf.subarray(start, end));
                start = i + 1;
            }
        }
        if (start > 0) {
            this.buf = this.buf.subarray(start);
        }
        if (this.buf.length > this.maxBytes) {
            throw new Error(`frame too large (>${this.maxBytes} bytes)`);
        }
        return frames;
    }
}

// ─── AsyncQueue ───────────────────────────────────────────────────────────────

/**
 * Bounded async queue for subscribers.
 *
 * Pushers call push(item) or close(). Consumers await pop(); a null result
 * signals the queue is closed. Items beyond capacity are dropped.
 */
class AsyncQueue<T> {
    private readonly items: T[] = [];
    private readonly waiters: Array<(item: T | null) => void> = [];
    private done = false;

    constructor(private readonly capacity: number = 256) {}

    /** Deliver an item to the queue; dropped silently when full. */
    push(item: T): void {
        if (this.done) return;
        if (this.waiters.length > 0) {
            // Fast path: a consumer is already waiting.
            const resolve = this.waiters.shift()!;
            resolve(item);
            return;
        }
        if (this.items.length < this.capacity) {
            this.items.push(item);
        }
        // else: drop — slow consumer.
    }

    /** Signal end-of-stream; pending and future pops return null. */
    close(): void {
        if (this.done) return;
        this.done = true;
        while (this.waiters.length > 0) {
            const resolve = this.waiters.shift()!;
            resolve(null);
        }
    }

    /** Resolve with the next item, or null if closed. */
    pop(): Promise<T | null> {
        if (this.items.length > 0) {
            return Promise.resolve(this.items.shift()!);
        }
        if (this.done) {
            return Promise.resolve(null);
        }
        return new Promise<T | null>((resolve) => {
            this.waiters.push(resolve);
        });
    }
}

// ─── Pending request entry ────────────────────────────────────────────────────

interface PendingEntry {
    resolve: (r: Response) => void;
    reject: (e: Error) => void;
    timer: ReturnType<typeof setTimeout>;
}

// ─── Client ───────────────────────────────────────────────────────────────────

export class Client {
    private readonly sock: net.Socket;
    private readonly splitter: FrameSplitter;
    private readonly pending = new Map<string, PendingEntry>();
    private readonly queues = new Set<AsyncQueue<Record<string, unknown>>>();
    private readonly timeoutMs: number;
    private _closed = false;
    private _readError: Error | null = null;
    private nextID = 0;

    private constructor(
        sock: net.Socket,
        opts: Required<ClientOptions>,
        pending?: Buffer,
    ) {
        this.sock = sock;
        this.splitter = new FrameSplitter(opts.maxFrameBytes);
        this.timeoutMs = opts.requestTimeoutMs;
        this.startReadLoop();
        // Bytes the peer pipelined behind the upgrade response, pushed through
        // the SAME FrameSplitter the read loop uses rather than a second
        // parser. No production path fills this today — upgradeAndServe reads
        // the full 101 before writing anything of its own, so the daemon has
        // nothing to answer yet — but pkg/upgradeconn.Dial's Go-side comment
        // is explicit that a future protocol might pipeline behind its 101,
        // and this is what makes that forward-compatible rather than a
        // silent frame loss.
        if (pending && pending.length > 0) {
            for (const frame of this.splitter.push(pending)) {
                this.dispatchFrame(frame);
            }
        }
    }

    /**
     * Constructs a Client from a socket that has already completed the HTTP
     * upgrade (and, if applicable, sent ctrl_auth). The only caller is
     * upgradeAndServe; this exists solely so the constructor can stay
     * private — TypeScript has no module-private access modifier, so a
     * bare `constructor` here would also be callable, bypassing dial()/
     * dialURL()/upgradeAndServe(), from any file that imports Client.
     */
    static fromUpgrade(sock: net.Socket, opts: Required<ClientOptions>, pending?: Buffer): Client {
        return new Client(sock, opts, pending);
    }

    // ─── Static factories ──────────────────────────────────────────────────────

    /**
     * Opens a UDS connection and returns a ready Client.
     * Throws if the socket path does not exist / is unreachable.
     */
    static dial(opts?: ClientOptions): Promise<Client> {
        const resolved = Client.resolveOpts(opts);
        const sock = net.createConnection({ path: resolved.socket });

        return new Promise<Client>((resolve, reject) => {
            sock.once("connect", () => {
                sock.removeAllListeners("error");
                resolve(new Client(sock, resolved));
            });
            sock.once("error", (err) => {
                sock.destroy();
                reject(err);
            });
        });
    }

    /**
     * Opens a TLS connection to url and returns a ready Client. url must be
     * "https://host[:port]" — the scheme the daemon's shared listener speaks.
     */
    static async dialURL(url: string, token: string): Promise<Client> {
        const u = new URL(url);
        if (u.protocol !== "https:") {
            throw new Error(`RAFIKI_URL scheme must be "https:", got "${u.protocol}"`);
        }
        const port = u.port ? parseInt(u.port, 10) : 443;
        const tls = await import("node:tls");
        const sock = await new Promise<import("node:tls").TLSSocket>((resolve, reject) => {
            const s = tls.connect({ host: u.hostname, port, rejectUnauthorized: true });
            s.once("secureConnect", () => { s.removeAllListeners("error"); resolve(s); });
            s.once("error", reject);
        });
        return upgradeAndServe(sock, `${u.hostname}:${port}`, token);
    }

    // ─── Public API ───────────────────────────────────────────────────────────

    get closed(): boolean {
        return this._closed;
    }

    /**
     * Sends a request and waits for the matching ctrl_response.
     *
     * Auto-assigns an id when the caller omits one. Returns the Response
     * envelope; success:false is returned, not thrown — the caller decides.
     * Throws on timeout, connection error, or closed connection.
     */
    request<_T = Response>(req: Record<string, unknown>): Promise<Response> {
        if (this._closed) {
            return Promise.reject(this.closedError());
        }

        // Assign id if missing; work on a shallow copy to avoid mutating caller's object.
        const id =
            typeof req["id"] === "string" && req["id"] !== ""
                ? req["id"]
                : `c${++this.nextID}`;
        const frame: Record<string, unknown> = { ...req, id };

        const payload = JSON.stringify(frame) + "\n";

        return new Promise<Response>((resolve, reject) => {
            const timer = setTimeout(() => {
                this.pending.delete(id);
                reject(new Error(`request ${id} timed out after ${this.timeoutMs}ms`));
            }, this.timeoutMs);

            this.pending.set(id, { resolve, reject, timer });

            // Write; on error immediately reject and clean up.
            const ok = this.sock.write(payload, "utf8");
            if (!ok) {
                // Backpressure; still technically enqueued — let the timeout handle failure.
                // Node will drain and flush; this is not an error in itself.
            }
        });
    }

    /**
     * Returns an async iterator that yields every non-response frame
     * received from the daemon. Each call returns an independent iterator.
     * The iterator terminates when close() is called.
     */
    subscribe(): AsyncIterableIterator<Record<string, unknown>> {
        const queue = new AsyncQueue<Record<string, unknown>>(256);
        this.queues.add(queue);
        const self = this;

        return {
            [Symbol.asyncIterator]() {
                return this;
            },
            async next(): Promise<IteratorResult<Record<string, unknown>>> {
                const item = await queue.pop();
                if (item === null) {
                    return { done: true, value: undefined };
                }
                return { done: false, value: item };
            },
            async return(): Promise<IteratorResult<Record<string, unknown>>> {
                self.queues.delete(queue);
                queue.close();
                return { done: true, value: undefined };
            },
        };
    }

    /**
     * Fetch the last `limit` retained event frames for a child via
     * ctrl_get_recent. limit <= 0 means "all in buffer". Returns the inner pi
     * event frames (already unwrapped — the ring stores raw child stdout).
     */
    async getRecent(childId: string, limit: number): Promise<Record<string, unknown>[]> {
        const req: Record<string, unknown> = { type: "ctrl_get_recent", childId, rendered: true };
        if (limit > 0) req["limit"] = limit;
        const resp = await this.request(req);
        if (!resp.success) {
            throw new Error(`ctrl_get_recent failed: ${resp.error?.code ?? "unknown"}`);
        }
        const data = resp.data as { events?: Record<string, unknown>[] } | undefined;
        return data?.events ?? [];
    }

    /**
     * Shuts down the connection. Pending requests reject; subscriber iterators
     * terminate. Idempotent.
     */
    async close(): Promise<void> {
        if (this._closed) return;
        this._closed = true;

        // Reject all pending requests.
        for (const [id, entry] of this.pending) {
            clearTimeout(entry.timer);
            entry.reject(this.closedError());
            this.pending.delete(id);
        }

        // Close all subscriber queues.
        for (const q of this.queues) {
            q.close();
        }
        this.queues.clear();

        this.sock.destroy();
    }

    // ─── Internal ─────────────────────────────────────────────────────────────

    private startReadLoop(): void {
        this.sock.on("data", (chunk: Buffer) => {
            let frames: Buffer[];
            try {
                frames = this.splitter.push(chunk);
            } catch (err) {
                this._readError = err instanceof Error ? err : new Error(String(err));
                this.close();
                return;
            }
            for (const frame of frames) {
                this.dispatchFrame(frame);
            }
        });

        this.sock.on("end", () => {
            this.close();
        });

        this.sock.on("error", (err) => {
            this._readError = err;
            this.close();
        });
    }

    private dispatchFrame(frame: Buffer): void {
        let obj: Record<string, unknown>;
        try {
            obj = JSON.parse(frame.toString("utf8")) as Record<string, unknown>;
        } catch {
            // Malformed frame — ignore, same as Go client.
            return;
        }

        const type = obj["type"];
        const id = typeof obj["id"] === "string" ? obj["id"] : undefined;

        if (type === "ctrl_response" && id !== undefined) {
            const entry = this.pending.get(id);
            if (entry) {
                clearTimeout(entry.timer);
                this.pending.delete(id);
                entry.resolve(obj as unknown as Response);
                return;
            }
            // No Request is waiting on this id. The one case that matters is
            // a rejected ctrl_auth: upgradeAndServe writes it before any
            // request() has registered a waiter (id "0", never reused by a
            // real request — request()'s auto-ids start at "c1"), and the
            // daemon's writeAuthError reply arrives with nobody listening.
            // Stash the reason so the caller sees "auth_invalid"/
            // "auth_required" instead of a bare "client connection closed"
            // once the daemon hangs up. Mirrors pkg/client/client.go's
            // authError handling in readLoop.
            const resp = obj as unknown as Response;
            if (resp.command === "ctrl_auth" && resp.success === false) {
                this._readError = authError(resp);
            }
            return;
        }

        // Event frame — dispatch to all subscribers.
        for (const q of this.queues) {
            q.push(obj);
        }
    }

    private closedError(): Error {
        if (this._readError) {
            return new Error(`client connection closed: ${this._readError.message}`);
        }
        return new Error("client connection closed");
    }

    // ─── Options resolution ───────────────────────────────────────────────────

    private static resolveOpts(opts?: ClientOptions): Required<ClientOptions> {
        return {
            socket: opts?.socket ?? defaultSocketPath(),
            requestTimeoutMs: opts?.requestTimeoutMs ?? 30_000,
            maxFrameBytes: opts?.maxFrameBytes ?? 16 << 20,
        };
    }

    // resolveTLSOpts returns default ClientOptions for a TLS connection
    // (no socket path — the underlying socket is already connected).
    // Not private: upgradeAndServe (module scope, same file) needs it.
    static resolveTLSOpts(): Required<ClientOptions> {
        return {
            socket: "", // unused — the socket is already connected
            requestTimeoutMs: 30_000,
            maxFrameBytes: 16 << 20,
        };
    }
}

/**
 * Performs the daemon's HTTP upgrade on an already-connected socket, writes the
 * ctrl_auth frame when a token is supplied, and returns a ready Client.
 *
 * host is the authority for the Host header ("example.dev:443"). An empty token
 * skips ctrl_auth entirely, matching the Go client: the daemon's bootstrap mode
 * admits a connection only when its first frame is NOT ctrl_auth.
 */
export async function upgradeAndServe(
    sock: net.Socket,
    host: string,
    token: string,
): Promise<Client> {
    const head =
        `GET /control HTTP/1.1\r\n` +
        `Host: ${host}\r\n` +
        `Upgrade: rafiki-control\r\n` +
        `Connection: Upgrade\r\n\r\n`;
    sock.write(head, "utf8");

    // Read exactly the response head and not one byte further into the frame
    // stream. Anything after the blank line is already-framed payload.
    const pending = await new Promise<Buffer>((resolve, reject) => {
        let buf = Buffer.alloc(0);
        const onData = (chunk: Buffer) => {
            buf = Buffer.concat([buf, chunk]);
            const end = buf.indexOf("\r\n\r\n");
            if (end === -1) {
                if (buf.length > 64 * 1024) {
                    cleanup();
                    sock.destroy();
                    reject(new Error("upgrade: response head exceeded 64 KiB"));
                }
                return;
            }
            const statusLine = buf.subarray(0, buf.indexOf("\r\n")).toString("utf8");
            cleanup();
            if (!/^HTTP\/1\.1 101\b/.test(statusLine)) {
                sock.destroy();
                reject(new Error(`upgrade: server answered "${statusLine}", want 101 Switching Protocols`));
                return;
            }
            resolve(buf.subarray(end + 4));
        };
        const onErr = (err: Error) => { cleanup(); sock.destroy(); reject(err); };
        const onEnd = () => { cleanup(); sock.destroy(); reject(new Error("upgrade: connection closed before response")); };
        const cleanup = () => {
            sock.off("data", onData);
            sock.off("error", onErr);
            sock.off("end", onEnd);
        };
        sock.on("data", onData);
        sock.on("error", onErr);
        sock.on("end", onEnd);
    });

    // Auth success is implicit: the daemon stays silent and holds the
    // connection open. Do not wait for an acknowledgement — none is coming.
    if (token !== "") {
        sock.write(JSON.stringify({ type: "ctrl_auth", id: "0", token }) + "\n", "utf8");
    }
    return Client.fromUpgrade(sock, Client.resolveTLSOpts(), pending);
}
