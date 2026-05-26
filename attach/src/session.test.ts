/**
 * Tests for session.ts — RemoteAgentSession.
 *
 * Uses a FakeClient that can push events and track requests so tests
 * run in-process without a real UDS server.
 */

import { describe, expect, it, beforeEach, afterEach } from "bun:test";
import type { AgentMessage, ThinkingLevel } from "@earendil-works/pi-agent-core";
import type { ImageContent, Model } from "@earendil-works/pi-ai";
import type { ModelRegistry, SessionManager, SettingsManager } from "@earendil-works/pi-coding-agent";
import { RemoteAgentSession, type RemoteSessionInit } from "./session.ts";
import type { Response } from "./client.ts";

// ─── FakeClient ───────────────────────────────────────────────────────────────

/**
 * An in-process stand-in for Client that:
 *   - Records every call to request()
 *   - Delivers pushed events through the subscribe() iterator
 *   - Can be closed to terminate the consumeEvents() loop
 */
class FakeClient {
    public readonly requests: Array<Record<string, unknown>> = [];

    /** Response override — use to inject error responses for testing. */
    public nextResponse?: Partial<Response>;

    private readonly _pending: Array<(r: IteratorResult<Record<string, unknown>>) => void> = [];
    private readonly _buffered: Array<Record<string, unknown>> = [];
    private _closed = false;

    readonly closed = false;

    async request(req: Record<string, unknown>): Promise<Response> {
        this.requests.push(req);
        const override = this.nextResponse;
        this.nextResponse = undefined;
        return {
            type: "ctrl_response",
            command: String(req["type"] ?? ""),
            success: true,
            ...override,
        } as Response;
    }

    subscribe(): AsyncIterableIterator<Record<string, unknown>> {
        // eslint-disable-next-line @typescript-eslint/no-this-alias
        const self = this;

        return {
            [Symbol.asyncIterator]() {
                return this;
            },
            async next(): Promise<IteratorResult<Record<string, unknown>>> {
                // Return a buffered event immediately if one is available.
                if (self._buffered.length > 0) {
                    return { done: false, value: self._buffered.shift()! };
                }
                // If closed with no buffer, signal end-of-stream.
                if (self._closed) {
                    return { done: true, value: undefined };
                }
                // Park until push() or close() resolves us.
                return new Promise<IteratorResult<Record<string, unknown>>>((resolve) => {
                    self._pending.push(resolve);
                });
            },
            async return(): Promise<IteratorResult<Record<string, unknown>>> {
                // Called by consumeEvents() on dispose().
                return { done: true, value: undefined };
            },
        };
    }

    /**
     * Push an event into consumeEvents().  If there is a parked consumer
     * awaiting the next event, wake it directly; otherwise buffer.
     */
    push(ev: Record<string, unknown>): void {
        if (this._pending.length > 0) {
            const resolve = this._pending.shift()!;
            resolve({ done: false, value: ev });
        } else {
            this._buffered.push(ev);
        }
    }

    /** Close the event stream, waking any parked consumer with done:true. */
    close(): void {
        this._closed = true;
        for (const resolve of this._pending.splice(0)) {
            resolve({ done: true, value: undefined });
        }
    }

    /** Implement the Client close method for compatibility. */
    async asyncClose(): Promise<void> {
        this.close();
    }
}

// ─── Test fixtures ────────────────────────────────────────────────────────────

const FAKE_MODEL = {
    provider: "anthropic",
    id: "claude-3-5-sonnet-20241022",
    name: "Claude 3.5 Sonnet",
} as unknown as Model<any>;

const FAKE_THINKING_LEVEL: ThinkingLevel = "off";

/**
 * Build a RemoteSessionInit with minimal stubs so tests can focus on
 * specific behaviour without constructing real pi services.
 */
function makeInit(client: FakeClient, childId = "child-1"): RemoteSessionInit {
    return {
        client: client as unknown as import("./client.ts").Client,
        childId,
        cwd: "/tmp/test",
        sessionId: "session-test-1",
        sessionFile: "/tmp/test/sessions/s1.jsonl",
        sessionName: undefined,
        model: FAKE_MODEL,
        thinkingLevel: FAKE_THINKING_LEVEL,
        sessionManager: {} as unknown as SessionManager,
        settingsManager: {} as unknown as SettingsManager,
        modelRegistry: {} as unknown as ModelRegistry,
    };
}

/** Wrap a session push with a small tick so consumeEvents() processes it. */
async function tick(): Promise<void> {
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
}

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("RemoteAgentSession", () => {
    let client: FakeClient;
    let session: RemoteAgentSession;

    beforeEach(() => {
        client = new FakeClient();
        session = new RemoteAgentSession(makeInit(client));
    });

    afterEach(() => {
        session.dispose();
        client.close();
    });

    // ── Constructor / initial state ───────────────────────────────────────────

    it("constructor: initial state is sane", () => {
        expect(session.sessionId).toBe("session-test-1");
        expect(session.sessionFile).toBe("/tmp/test/sessions/s1.jsonl");
        expect(session.isStreaming).toBe(false);
        expect(session.isCompacting).toBe(false);
        expect(session.isRetrying).toBe(false);
        expect(session.retryAttempt).toBe(0);
        expect(session.messages).toEqual([]);
        expect(session.thinkingLevel).toBe("off");
        expect(session.steeringMode).toBe("one-at-a-time");
        expect(session.followUpMode).toBe("one-at-a-time");
        expect(session.pendingMessageCount).toBe(0);
        expect(session.sessionName).toBeUndefined();
        expect(session.model).toBeDefined();
    });

    // ── subscribe / emit ──────────────────────────────────────────────────────

    it("subscribe: listener receives events pushed from daemon", async () => {
        const received: Array<{ type: string }> = [];
        session.subscribe((ev) => {
            received.push({ type: ev.type });
        });

        // Wrap in a ctrl_event envelope (the daemon's wire format).
        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: { type: "agent_start" },
        });
        await tick();

        expect(received).toHaveLength(1);
        expect(received[0]?.type).toBe("agent_start");
    });

    it("subscribe: events for other children are ignored", async () => {
        const received: Array<Record<string, unknown>> = [];
        session.subscribe((ev) => received.push(ev as unknown as Record<string, unknown>));

        client.push({
            type: "ctrl_event",
            childId: "other-child",  // different child
            event: { type: "agent_start" },
        });
        await tick();

        expect(received).toHaveLength(0);
    });

    it("subscribe: non-ctrl_event frames are ignored", async () => {
        const received: Array<Record<string, unknown>> = [];
        session.subscribe((ev) => received.push(ev as unknown as Record<string, unknown>));

        client.push({ type: "ctrl_child_spawned", childId: "child-1" });
        await tick();

        expect(received).toHaveLength(0);
    });

    it("subscribe: returns an unsubscribe function that stops delivery", async () => {
        const received: Array<string> = [];
        const unsub = session.subscribe((ev) => received.push(ev.type));

        client.push({ type: "ctrl_event", childId: "child-1", event: { type: "agent_start" } });
        await tick();

        unsub();

        client.push({ type: "ctrl_event", childId: "child-1", event: { type: "agent_end", messages: [] } });
        await tick();

        expect(received).toHaveLength(1);
        expect(received[0]).toBe("agent_start");
    });

    // ── State cache updates ────────────────────────────────────────────────────

    it("state cache: agent_start sets isStreaming = true", async () => {
        expect(session.isStreaming).toBe(false);

        client.push({ type: "ctrl_event", childId: "child-1", event: { type: "agent_start" } });
        await tick();

        expect(session.isStreaming).toBe(true);
    });

    it("state cache: agent_end sets isStreaming = false and updates messages", async () => {
        // First start streaming
        client.push({ type: "ctrl_event", childId: "child-1", event: { type: "agent_start" } });
        await tick();
        expect(session.isStreaming).toBe(true);

        const fakeMessages: AgentMessage[] = [
            { role: "user", content: [{ type: "text", text: "hello" }], timestamp: 1 } as unknown as AgentMessage,
        ];
        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: { type: "agent_end", messages: fakeMessages, willRetry: false },
        });
        await tick();

        expect(session.isStreaming).toBe(false);
        expect(session.messages).toHaveLength(1);
    });

    it("state cache: compaction_start / compaction_end toggle isCompacting", async () => {
        expect(session.isCompacting).toBe(false);

        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: { type: "compaction_start", reason: "manual" },
        });
        await tick();
        expect(session.isCompacting).toBe(true);

        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: {
                type: "compaction_end",
                reason: "manual",
                result: undefined,
                aborted: false,
                willRetry: false,
            },
        });
        await tick();
        expect(session.isCompacting).toBe(false);
    });

    it("state cache: thinking_level_changed updates thinkingLevel", async () => {
        expect(session.thinkingLevel).toBe("off");

        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: { type: "thinking_level_changed", level: "medium" },
        });
        await tick();

        expect(session.thinkingLevel).toBe("medium");
    });

    it("state cache: session_info_changed updates sessionName", async () => {
        expect(session.sessionName).toBeUndefined();

        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: { type: "session_info_changed", name: "my-session" },
        });
        await tick();

        expect(session.sessionName).toBe("my-session");
    });

    it("state cache: queue_update syncs steering/followUp messages", async () => {
        expect(session.pendingMessageCount).toBe(0);

        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: {
                type: "queue_update",
                steering: ["steer 1", "steer 2"],
                followUp: ["follow 1"],
            },
        });
        await tick();

        expect(session.getSteeringMessages()).toEqual(["steer 1", "steer 2"]);
        expect(session.getFollowUpMessages()).toEqual(["follow 1"]);
        expect(session.pendingMessageCount).toBe(3);
    });

    it("state cache: auto_retry_start / auto_retry_end toggle isRetrying", async () => {
        expect(session.isRetrying).toBe(false);

        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: {
                type: "auto_retry_start",
                attempt: 1,
                maxAttempts: 3,
                delayMs: 1000,
                errorMessage: "overloaded",
            },
        });
        await tick();
        expect(session.isRetrying).toBe(true);
        expect(session.retryAttempt).toBe(1);

        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: { type: "auto_retry_end", success: false, attempt: 1 },
        });
        await tick();
        expect(session.isRetrying).toBe(false);
    });

    // ── agent_end event translation ────────────────────────────────────────────

    it("translate: agent_end gains willRetry:false when not in payload", async () => {
        const received: Array<Record<string, unknown>> = [];
        session.subscribe((ev) => received.push(ev as unknown as Record<string, unknown>));

        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: { type: "agent_end", messages: [] },  // no willRetry
        });
        await tick();

        expect(received).toHaveLength(1);
        expect(received[0]?.["type"]).toBe("agent_end");
        expect(received[0]?.["willRetry"]).toBe(false);
    });

    it("translate: agent_end preserves willRetry:true from payload", async () => {
        const received: Array<Record<string, unknown>> = [];
        session.subscribe((ev) => received.push(ev as unknown as Record<string, unknown>));

        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: { type: "agent_end", messages: [], willRetry: true },
        });
        await tick();

        expect(received[0]?.["willRetry"]).toBe(true);
    });

    // ── prompt ────────────────────────────────────────────────────────────────

    it("prompt: sends ctrl_send with type=prompt and message text", async () => {
        await session.prompt("hello world");

        expect(client.requests).toHaveLength(1);
        const req = client.requests[0]!;
        expect(req["type"]).toBe("ctrl_send");
        expect(req["childId"]).toBe("child-1");
        const frame = req["frame"] as Record<string, unknown>;
        expect(frame?.["type"]).toBe("prompt");
        expect(frame?.["message"]).toBe("hello world");
    });

    it("prompt: propagates images from PromptOptions", async () => {
        // ImageContent shape: { type: "image", data: string, mimeType: string }
        const images: ImageContent[] = [{ type: "image", data: "", mimeType: "image/png" }];
        await session.prompt("look at this", { images });

        const frame = (client.requests[0]!["frame"]) as Record<string, unknown>;
        expect(frame?.["images"]).toBe(images);
    });

    it("prompt: throws when daemon returns success:false", async () => {
        client.nextResponse = { success: false, error: { code: "not_found" } };

        await expect(session.prompt("hello")).rejects.toThrow("prompt: not_found");
    });

    // ── steer ─────────────────────────────────────────────────────────────────

    it("steer: sends ctrl_send with type=steer", async () => {
        await session.steer("steer message");

        const req = client.requests[0]!;
        expect(req["type"]).toBe("ctrl_send");
        const frame = req["frame"] as Record<string, unknown>;
        expect(frame?.["type"]).toBe("steer");
        expect(frame?.["message"]).toBe("steer message");
    });

    // ── followUp ──────────────────────────────────────────────────────────────

    it("followUp: sends ctrl_send with type=follow_up", async () => {
        await session.followUp("follow up message");

        const frame = (client.requests[0]!["frame"]) as Record<string, unknown>;
        expect(frame?.["type"]).toBe("follow_up");
    });

    // ── sendUserMessage ───────────────────────────────────────────────────────

    it("sendUserMessage: sends ctrl_send with type=send_user_message", async () => {
        await session.sendUserMessage("user content");

        const frame = (client.requests[0]!["frame"]) as Record<string, unknown>;
        expect(frame?.["type"]).toBe("send_user_message");
        expect(frame?.["content"]).toBe("user content");
    });

    // ── abort ─────────────────────────────────────────────────────────────────

    it("abort: sends ctrl_send with type=abort", async () => {
        await session.abort();

        const frame = (client.requests[0]!["frame"]) as Record<string, unknown>;
        expect(frame?.["type"]).toBe("abort");
    });

    // ── setModel ──────────────────────────────────────────────────────────────

    it("setModel: sends ctrl_send with provider and modelId, updates cached model", async () => {
        const newModel = {
            provider: "openai",
            id: "gpt-4o",
            name: "GPT-4o",
        } as unknown as Model<any>;

        await session.setModel(newModel);

        const frame = (client.requests[0]!["frame"]) as Record<string, unknown>;
        expect(frame?.["type"]).toBe("set_model");
        expect(frame?.["provider"]).toBe("openai");
        expect(frame?.["modelId"]).toBe("gpt-4o");
        expect(session.model).toBe(newModel);
    });

    it("setModel: throws when daemon returns success:false", async () => {
        client.nextResponse = { success: false, error: { code: "invalid_model" } };
        await expect(
            session.setModel({ provider: "bad", id: "bad" } as unknown as Model<any>)
        ).rejects.toThrow("setModel: invalid_model");
    });

    // ── setThinkingLevel ──────────────────────────────────────────────────────

    it("setThinkingLevel: updates cached level optimistically and fires ctrl_send", async () => {
        expect(session.thinkingLevel).toBe("off");
        session.setThinkingLevel("high");

        expect(session.thinkingLevel).toBe("high");

        // Give the async request a tick to fire
        await tick();
        expect(client.requests).toHaveLength(1);
        const frame = (client.requests[0]!["frame"]) as Record<string, unknown>;
        expect(frame?.["type"]).toBe("set_thinking_level");
        expect(frame?.["level"]).toBe("high");
    });

    // ── setSteeringMode / setFollowUpMode ─────────────────────────────────────

    it("setSteeringMode: updates cached mode and fires ctrl_send", async () => {
        session.setSteeringMode("all");
        expect(session.steeringMode).toBe("all");

        await tick();
        const frame = (client.requests[0]!["frame"]) as Record<string, unknown>;
        expect(frame?.["type"]).toBe("set_steering_mode");
        expect(frame?.["mode"]).toBe("all");
    });

    it("setFollowUpMode: updates cached mode and fires ctrl_send", async () => {
        session.setFollowUpMode("all");
        expect(session.followUpMode).toBe("all");

        await tick();
        const frame = (client.requests[0]!["frame"]) as Record<string, unknown>;
        expect(frame?.["type"]).toBe("set_follow_up_mode");
        expect(frame?.["mode"]).toBe("all");
    });

    // ── clearQueue ────────────────────────────────────────────────────────────

    it("clearQueue: returns current queues and empties them", async () => {
        // Seed via a queue_update event
        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: {
                type: "queue_update",
                steering: ["s1"],
                followUp: ["f1", "f2"],
            },
        });
        await tick();

        const result = session.clearQueue();
        expect(result.steering).toEqual(["s1"]);
        expect(result.followUp).toEqual(["f1", "f2"]);
        expect(session.pendingMessageCount).toBe(0);
    });

    // ── setSessionName ────────────────────────────────────────────────────────

    it("setSessionName: updates local cache and fires ctrl_send", async () => {
        session.setSessionName("my-session");
        expect(session.sessionName).toBe("my-session");

        await tick();
        const frame = (client.requests[0]!["frame"]) as Record<string, unknown>;
        expect(frame?.["type"]).toBe("set_session_name");
        expect(frame?.["name"]).toBe("my-session");
    });

    // ── getSessionStats ───────────────────────────────────────────────────────

    it("getSessionStats: returns correct sessionId and sessionFile", () => {
        const stats = session.getSessionStats();
        expect(stats.sessionId).toBe("session-test-1");
        expect(stats.sessionFile).toBe("/tmp/test/sessions/s1.jsonl");
        expect(stats.totalMessages).toBe(0);
        expect(stats.cost).toBe(0);
    });

    // ── getLastAssistantText ──────────────────────────────────────────────────

    it("getLastAssistantText: returns undefined when no messages", () => {
        expect(session.getLastAssistantText()).toBeUndefined();
    });

    it("getLastAssistantText: extracts text from last assistant message", async () => {
        const assistantMsg = {
            role: "assistant",
            content: [
                { type: "text", text: "Hello from assistant" },
            ],
        } as unknown as AgentMessage;

        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: {
                type: "agent_end",
                messages: [assistantMsg],
                willRetry: false,
            },
        });
        await tick();

        expect(session.getLastAssistantText()).toBe("Hello from assistant");
    });

    // ── state getter ──────────────────────────────────────────────────────────

    it("state getter: reflects isStreaming from cache", async () => {
        expect(session.state.isStreaming).toBe(false);

        client.push({ type: "ctrl_event", childId: "child-1", event: { type: "agent_start" } });
        await tick();

        expect(session.state.isStreaming).toBe(true);
    });

    it("state getter: reflects messages from cache", async () => {
        expect(session.state.messages).toHaveLength(0);

        const msg = { role: "user", content: [{ type: "text", text: "hi" }] } as unknown as AgentMessage;
        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: { type: "agent_end", messages: [msg], willRetry: false },
        });
        await tick();

        expect(session.state.messages).toHaveLength(1);
    });

    // ── dispose ───────────────────────────────────────────────────────────────

    it("dispose: clears listeners so no more events are delivered", async () => {
        const received: string[] = [];
        session.subscribe((ev) => received.push(ev.type));

        session.dispose();

        client.push({ type: "ctrl_event", childId: "child-1", event: { type: "agent_start" } });
        await tick();

        // After dispose, no events should reach any listener.
        expect(received).toHaveLength(0);
    });

    // ── stub methods ──────────────────────────────────────────────────────────

    it("stubs: getActiveToolNames returns empty array", () => {
        expect(session.getActiveToolNames()).toEqual([]);
    });

    it("stubs: getAllTools returns empty array", () => {
        expect(session.getAllTools()).toEqual([]);
    });

    it("stubs: getToolDefinition returns undefined", () => {
        expect(session.getToolDefinition("bash")).toBeUndefined();
    });

    it("stubs: getContextUsage returns undefined", () => {
        expect(session.getContextUsage()).toBeUndefined();
    });

    it("stubs: scopedModels returns empty array", () => {
        expect(session.scopedModels).toEqual([]);
    });

    it("stubs: promptTemplates returns empty array", () => {
        expect(session.promptTemplates).toEqual([]);
    });

    it("stubs: autoCompactionEnabled returns false", () => {
        expect(session.autoCompactionEnabled).toBe(false);
    });

    it("stubs: autoRetryEnabled returns false", () => {
        expect(session.autoRetryEnabled).toBe(false);
    });

    it("stubs: hasExtensionHandlers returns false", () => {
        expect(session.hasExtensionHandlers("agent_start")).toBe(false);
    });

    it("stubs: executeBash throws v1-error with actionable message", async () => {
        await expect(session.executeBash("ls")).rejects.toThrow(
            "Bash execution (! prefix) is not supported in pic-attach v1"
        );
    });

    // ── reload ─────────────────────────────────────────────────────────────

    it("reload: forwards /reload as a prompt frame to the daemon", async () => {
        await session.reload();

        expect(client.requests).toHaveLength(1);
        const req = client.requests[0]!;
        expect(req["type"]).toBe("ctrl_send");
        expect(req["childId"]).toBe("child-1");
        const frame = req["frame"] as Record<string, unknown>;
        expect(frame?.["type"]).toBe("prompt");
        expect(frame?.["message"]).toBe("/reload");
    });

    it("reload: throws when daemon returns success:false", async () => {
        client.nextResponse = { success: false, error: { code: "child_not_found" } };
        await expect(session.reload()).rejects.toThrow("reload: child_not_found");
    });

    // ── compact ────────────────────────────────────────────────────────────────

    it("compact: forwards /compact as a prompt frame to the daemon", async () => {
        await session.compact();

        expect(client.requests).toHaveLength(1);
        const req = client.requests[0]!;
        expect(req["type"]).toBe("ctrl_send");
        expect(req["childId"]).toBe("child-1");
        const frame = req["frame"] as Record<string, unknown>;
        expect(frame?.["type"]).toBe("prompt");
        expect(frame?.["message"]).toBe("/compact");
    });

    it("compact: includes custom instructions in the forwarded prompt", async () => {
        await session.compact("focus on removing tool results");

        const frame = (client.requests[0]!["frame"]) as Record<string, unknown>;
        expect(frame?.["message"]).toBe("/compact focus on removing tool results");
    });

    it("stubs: navigateTree throws with actionable v1 message", async () => {
        await expect(session.navigateTree("some-id")).rejects.toThrow(
            "/tree and /fork navigation are not supported in pic-attach v1"
        );
    });

    it("stubs: exportToHtml throws with actionable v1 message", async () => {
        await expect(session.exportToHtml()).rejects.toThrow(
            "/export and /share are not supported in pic-attach v1"
        );
    });

    it("stubs: getUserMessagesForForking returns empty array", () => {
        expect(session.getUserMessagesForForking()).toEqual([]);
    });

    // ── cycleThinkingLevel / cycleModel graceful stubs ───────────────────────

    it("cycleThinkingLevel: returns undefined (TUI shows 'model does not support thinking')", () => {
        // The TUI's InteractiveMode.cycleThinkingLevel() does NOT have a
        // try-catch; returning undefined is the only safe outcome here.
        expect(session.cycleThinkingLevel()).toBeUndefined();
    });

    it("cycleModel: returns undefined (TUI shows 'only one model available')", async () => {
        // The TUI's cycleModel() wraps this in try-catch, but returning
        // undefined gives a nicer status message than an error dialog.
        await expect(session.cycleModel("forward")).resolves.toBeUndefined();
        await expect(session.cycleModel("backward")).resolves.toBeUndefined();
        await expect(session.cycleModel()).resolves.toBeUndefined();
    });

    // ── sendCustomMessage v1-error ───────────────────────────────────────

    it("sendCustomMessage: throws with standard v1-error format", async () => {
        await expect(session.sendCustomMessage({ type: "custom" })).rejects.toThrow(
            "sendCustomMessage is not supported in pic-attach v1."
        );
    });

    // ── agent stub: waitForIdle / signal ───────────────────────────────

    it("agent.waitForIdle: resolves immediately when not streaming", async () => {
        expect(session.isStreaming).toBe(false);
        // Should resolve synchronously (or in the next microtask)
        await expect(session.agent.waitForIdle()).resolves.toBeUndefined();
    });

    it("agent.waitForIdle: resolves when agent_end arrives while streaming", async () => {
        // Put the session into streaming state
        client.push({ type: "ctrl_event", childId: "child-1", event: { type: "agent_start" } });
        await tick();
        expect(session.isStreaming).toBe(true);

        // Start waiting for idle
        const idlePromise = session.agent.waitForIdle();

        // Deliver agent_end
        client.push({
            type: "ctrl_event",
            childId: "child-1",
            event: { type: "agent_end", messages: [], willRetry: false },
        });
        await tick();

        await expect(idlePromise).resolves.toBeUndefined();
    });

    it("agent.signal: returns undefined (no active run from client side)", () => {
        expect(session.agent.signal).toBeUndefined();
    });
});
