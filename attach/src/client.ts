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

// ─── Public types ─────────────────────────────────────────────────────────────

export interface ClientOptions {
    /** Defaults to $PI_CONTROLLER_SOCKET or ~/.pi/run/controller.sock */
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

    private constructor(sock: net.Socket, opts: Required<ClientOptions>) {
        this.sock = sock;
        this.splitter = new FrameSplitter(opts.maxFrameBytes);
        this.timeoutMs = opts.requestTimeoutMs;
        this.startReadLoop();
    }

    // ─── Static factory ──────────────────────────────────────────────────────

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
        const socket =
            opts?.socket ??
            process.env["PI_CONTROLLER_SOCKET"] ??
            path.join(os.homedir(), ".pi", "run", "controller.sock");
        return {
            socket,
            requestTimeoutMs: opts?.requestTimeoutMs ?? 30_000,
            maxFrameBytes: opts?.maxFrameBytes ?? 16 << 20,
        };
    }
}
