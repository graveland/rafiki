/**
 * claude-normalized.test.ts — verify fundi-attach consumes daemon-normalized
 * claude frames.
 *
 * The pi-controller daemon translates Claude Code children's stream-json into
 * pi's `AgentSessionEvent` vocabulary ON THE EVENT BUS, wrapping each pi-event
 * in a `ctrl_event` envelope. This package (the attach layer) is supposed to
 * render those claude children with ZERO claude-awareness — it just consumes
 * the standard pi-event vocabulary.
 *
 * This test feeds the EXACT ordered sequence the daemon emits per claude turn
 * (verified by the daemon's Go integration test) through the real consumption
 * path (`consumeEvents` → `translate` → `updateCacheFromEvent` → `emit`) and
 * asserts the resulting state is what `InteractiveMode` would render:
 *   - the emitted AgentSessionEvent stream carries the tool_execution_start /
 *     tool_execution_end frames with their fields intact,
 *   - the cached `_messages` end up with the assistant text, the toolCall
 *     block (name + arguments), and the toolResult,
 *   - the turn ends clean (isStreaming false, no error, no streamingMessage).
 *
 * If the attach layer fails to consume any daemon-emitted frame, the fix
 * belongs in the daemon's emission — this test pins the contract so a mismatch
 * shows up here rather than as a silent render gap.
 */

import { describe, expect, it } from "bun:test";
import type { AgentMessage, ThinkingLevel } from "@earendil-works/pi-agent-core";
import type { AgentSessionEvent } from "@earendil-works/pi-coding-agent";
import type { Model } from "@earendil-works/pi-ai";
import type { ModelRegistry, SessionManager, SettingsManager } from "@earendil-works/pi-coding-agent";
import { RemoteAgentSession, type RemoteSessionInit } from "./session.ts";
import type { Response } from "./client.ts";

// ─── FakeClient ───────────────────────────────────────────────────────────────
//
// Minimal in-process stand-in for Client: delivers pushed frames through the
// subscribe() iterator and auto-acks every request(). Same shape as the one in
// session.test.ts, kept local so this file is self-contained.

class FakeClient {
    public readonly requests: Array<Record<string, unknown>> = [];

    private readonly _pending: Array<(r: IteratorResult<Record<string, unknown>>) => void> = [];
    private readonly _buffered: Array<Record<string, unknown>> = [];
    private _closed = false;

    async request(req: Record<string, unknown>): Promise<Response> {
        this.requests.push(req);
        return {
            type: "ctrl_response",
            command: String(req["type"] ?? ""),
            success: true,
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
                if (self._buffered.length > 0) {
                    return { done: false, value: self._buffered.shift()! };
                }
                if (self._closed) {
                    return { done: true, value: undefined };
                }
                return new Promise<IteratorResult<Record<string, unknown>>>((resolve) => {
                    self._pending.push(resolve);
                });
            },
            async return(): Promise<IteratorResult<Record<string, unknown>>> {
                return { done: true, value: undefined };
            },
        };
    }

    /** Push a frame into consumeEvents(); wake a parked consumer or buffer. */
    push(ev: Record<string, unknown>): void {
        if (this._pending.length > 0) {
            const resolve = this._pending.shift()!;
            resolve({ done: false, value: ev });
        } else {
            this._buffered.push(ev);
        }
    }

    close(): void {
        this._closed = true;
        for (const resolve of this._pending.splice(0)) {
            resolve({ done: true, value: undefined });
        }
    }
}

const FAKE_MODEL = {
    provider: "anthropic",
    id: "claude-sonnet-4",
    name: "Claude Sonnet 4",
} as unknown as Model<any>;

function makeInit(client: FakeClient, childId = "claude-child"): RemoteSessionInit {
    return {
        client: client as unknown as import("./client.ts").Client,
        childId,
        cwd: "/tmp/test",
        sessionId: "session-claude-1",
        sessionFile: "/tmp/test/sessions/s1.jsonl",
        sessionName: undefined,
        model: FAKE_MODEL,
        thinkingLevel: "off" as ThinkingLevel,
        sessionManager: {} as unknown as SessionManager,
        settingsManager: {} as unknown as SettingsManager,
        modelRegistry: {} as unknown as ModelRegistry,
    };
}

/** Yield so the async consumeEvents() loop drains buffered frames. */
async function tick(): Promise<void> {
    await new Promise<void>((resolve) => setTimeout(resolve, 0));
}

/** Wrap a pi-event in the daemon's ctrl_event envelope for a given child. */
function ctrlEvent(childId: string, event: Record<string, unknown>): Record<string, unknown> {
    return { type: "ctrl_event", childId, event };
}

// ─── The daemon-normalized claude turn ────────────────────────────────────────
//
// A single claude turn with one tool call (Read), built from the exact frame
// shapes the daemon emits. Block/message shapes per the daemon's contract:
//   AssistantMessage content blocks: {type:"text",text}, {type:"toolCall",id,name,arguments},
//                                    {type:"thinking",thinking}
//   ToolResultMessage: {role:"toolResult",toolCallId,toolName,content:[{type:"text",text}],isError,timestamp}

const TOOL_CALL_ID = "toolu_abc123";
const TOOL_NAME = "Read";
const TOOL_ARGS = { file_path: "/etc/hostname" };

const ASSISTANT_TEXT = "I'll read that file for you.";

/** AssistantMessage with full content (text + a toolCall block). */
const ASSISTANT_MESSAGE = {
    role: "assistant",
    id: "msg_assistant_1",
    content: [
        { type: "text", text: ASSISTANT_TEXT },
        { type: "toolCall", id: TOOL_CALL_ID, name: TOOL_NAME, arguments: TOOL_ARGS },
    ],
    timestamp: 1000,
} as unknown as AgentMessage;

/** Empty-content AssistantMessage for message_start. */
const ASSISTANT_MESSAGE_EMPTY = {
    role: "assistant",
    id: "msg_assistant_1",
    content: [],
    timestamp: 1000,
} as unknown as AgentMessage;

/** The user message that opened the turn. */
const USER_MESSAGE = {
    role: "user",
    content: [{ type: "text", text: "read /etc/hostname" }],
    timestamp: 990,
} as unknown as AgentMessage;

/** ToolResultMessage produced by the daemon for the Read tool. */
const TOOL_RESULT_MESSAGE = {
    role: "toolResult",
    toolCallId: TOOL_CALL_ID,
    toolName: TOOL_NAME,
    content: [{ type: "text", text: "critter\n" }],
    isError: false,
    timestamp: 1100,
} as unknown as AgentMessage;

/** Full AgentMessage[] the daemon ships in agent_end. */
const AGENT_END_MESSAGES: AgentMessage[] = [
    USER_MESSAGE,
    ASSISTANT_MESSAGE,
    TOOL_RESULT_MESSAGE,
];

/**
 * The ordered daemon emission for one claude turn, each entry the inner
 * `ctrl_event.event` object.
 */
const DAEMON_TURN: Array<Record<string, unknown>> = [
    { type: "agent_start" },
    { type: "message_start", message: ASSISTANT_MESSAGE_EMPTY },
    {
        type: "message_update",
        message: ASSISTANT_MESSAGE,
        assistantMessageEvent: { type: "done" },
    },
    {
        type: "tool_execution_start",
        toolCallId: TOOL_CALL_ID,
        toolName: TOOL_NAME,
        args: TOOL_ARGS,
    },
    { type: "message_end", message: ASSISTANT_MESSAGE },
    {
        type: "tool_execution_end",
        toolCallId: TOOL_CALL_ID,
        toolName: TOOL_NAME,
        result: "critter\n",
        isError: false,
    },
    { type: "agent_end", messages: AGENT_END_MESSAGES, willRetry: false },
];

// ─── Test ─────────────────────────────────────────────────────────────────────

describe("fundi-attach consumes daemon-normalized claude frames", () => {
    it("renders a claude tool turn: assistant text, toolCall, toolResult, clean end", async () => {
        const client = new FakeClient();
        const session = new RemoteAgentSession(makeInit(client));
        // Constructor no longer auto-starts the consume loop; start it for the
        // live-event path this test exercises.
        session.start();

        // Capture the emitted AgentSessionEvent stream — this is exactly what
        // InteractiveMode subscribes to for live rendering.
        const emitted: AgentSessionEvent[] = [];
        session.subscribe((ev) => emitted.push(ev));

        try {
            // Drive the full daemon turn through the wire (ctrl_event envelopes).
            for (const event of DAEMON_TURN) {
                client.push(ctrlEvent("claude-child", event));
                await tick();
            }

            // ── 1. Every frame consumed and emitted, in order ────────────────
            expect(emitted.map((e) => e.type)).toEqual([
                "agent_start",
                "message_start",
                "message_update",
                "tool_execution_start",
                "message_end",
                "tool_execution_end",
                "agent_end",
            ]);

            // ── 2. tool_execution_start carries the tool name + args ─────────
            const toolStart = emitted.find((e) => e.type === "tool_execution_start") as
                | Record<string, unknown>
                | undefined;
            expect(toolStart).toBeDefined();
            expect(toolStart!["toolCallId"]).toBe(TOOL_CALL_ID);
            expect(toolStart!["toolName"]).toBe(TOOL_NAME);
            expect(toolStart!["args"]).toEqual(TOOL_ARGS);

            // ── 3. tool_execution_end carries the result, not an error ───────
            const toolEnd = emitted.find((e) => e.type === "tool_execution_end") as
                | Record<string, unknown>
                | undefined;
            expect(toolEnd).toBeDefined();
            expect(toolEnd!["toolCallId"]).toBe(TOOL_CALL_ID);
            expect(toolEnd!["result"]).toBe("critter\n");
            expect(toolEnd!["isError"]).toBe(false);

            // ── 4. agent_end survives translate() with willRetry intact ──────
            const agentEnd = emitted.find((e) => e.type === "agent_end") as
                | Record<string, unknown>
                | undefined;
            expect(agentEnd).toBeDefined();
            expect(agentEnd!["willRetry"]).toBe(false);

            // ── 5. Cached _messages reflect the full turn ────────────────────
            // (this is what session.state.messages hands InteractiveMode after
            //  the turn — assistant text, the toolCall block, the toolResult).
            const msgs = session.messages;
            expect(msgs).toHaveLength(3);

            const user = msgs.find(
                (m) => (m as unknown as Record<string, unknown>)["role"] === "user"
            ) as unknown as Record<string, unknown> | undefined;
            expect(user).toBeDefined();

            const assistant = msgs.find(
                (m) => (m as unknown as Record<string, unknown>)["role"] === "assistant"
            ) as unknown as Record<string, unknown> | undefined;
            expect(assistant).toBeDefined();
            const aContent = assistant!["content"] as Array<Record<string, unknown>>;
            const textBlock = aContent.find((b) => b["type"] === "text");
            expect(textBlock?.["text"]).toBe(ASSISTANT_TEXT);
            const toolCallBlock = aContent.find((b) => b["type"] === "toolCall");
            expect(toolCallBlock).toBeDefined();
            expect(toolCallBlock!["id"]).toBe(TOOL_CALL_ID);
            expect(toolCallBlock!["name"]).toBe(TOOL_NAME);
            expect(toolCallBlock!["arguments"]).toEqual(TOOL_ARGS);

            const toolResult = msgs.find(
                (m) => (m as unknown as Record<string, unknown>)["role"] === "toolResult"
            ) as unknown as Record<string, unknown> | undefined;
            expect(toolResult).toBeDefined();
            expect(toolResult!["toolCallId"]).toBe(TOOL_CALL_ID);
            expect(toolResult!["toolName"]).toBe(TOOL_NAME);
            expect(toolResult!["isError"]).toBe(false);
            const trContent = toolResult!["content"] as Array<Record<string, unknown>>;
            expect(trContent[0]?.["text"]).toBe("critter\n");

            // ── 6. The assistant text is reachable via getLastAssistantText ──
            // (the helper InteractiveMode / callers use to echo the reply).
            expect(session.getLastAssistantText()).toBe(ASSISTANT_TEXT);

            // ── 7. Clean turn end: not streaming, no error, no partial msg ───
            expect(session.isStreaming).toBe(false);
            expect(session.state.errorMessage).toBeUndefined();
            expect(session.state.streamingMessage).toBeUndefined();
            expect(session.isRetrying).toBe(false);
        } finally {
            session.dispose();
            client.close();
        }
    });

    it("mid-turn streamingMessage tracks the in-flight assistant message", async () => {
        const client = new FakeClient();
        const session = new RemoteAgentSession(makeInit(client));
        // Constructor no longer auto-starts the consume loop; start it for the
        // live-event path this test exercises.
        session.start();

        try {
            // agent_start → streaming begins, no partial message yet.
            client.push(ctrlEvent("claude-child", { type: "agent_start" }));
            await tick();
            expect(session.isStreaming).toBe(true);
            expect(session.state.streamingMessage).toBeUndefined();

            // message_start → the (empty) assistant message is the live one.
            client.push(
                ctrlEvent("claude-child", { type: "message_start", message: ASSISTANT_MESSAGE_EMPTY })
            );
            await tick();
            expect(session.state.streamingMessage).toBeDefined();

            // message_update → live message gains full content (text + toolCall).
            client.push(
                ctrlEvent("claude-child", {
                    type: "message_update",
                    message: ASSISTANT_MESSAGE,
                    assistantMessageEvent: { type: "done" },
                })
            );
            await tick();
            const streaming = session.state.streamingMessage as unknown as Record<string, unknown>;
            const content = streaming?.["content"] as Array<Record<string, unknown>>;
            expect(content.some((b) => b["type"] === "text" && b["text"] === ASSISTANT_TEXT)).toBe(true);
            expect(content.some((b) => b["type"] === "toolCall" && b["name"] === TOOL_NAME)).toBe(true);

            // message_end → live message clears, lands in the cache.
            client.push(ctrlEvent("claude-child", { type: "message_end", message: ASSISTANT_MESSAGE }));
            await tick();
            expect(session.state.streamingMessage).toBeUndefined();
            expect(session.messages.some(
                (m) => (m as unknown as Record<string, unknown>)["id"] === "msg_assistant_1"
            )).toBe(true);
        } finally {
            session.dispose();
            client.close();
        }
    });
});
