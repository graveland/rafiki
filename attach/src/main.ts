#!/usr/bin/env bun

import { InteractiveMode } from "@earendil-works/pi-coding-agent";
import { RemoteAgentSessionRuntime } from "./runtime.ts";

const VERSION = "0.1.0";

// Matches what pi's own cli.js does before constructing InteractiveMode.
process.env.PI_CODING_AGENT = "true";
process.emitWarning = () => {};

function usage(): void {
    console.error(`pic-attach v${VERSION}`);
    console.error("usage: pic-attach <childId>");
    console.error("");
    console.error("env vars:");
    console.error("  PI_CONTROLLER_SOCKET  override default socket path");
    console.error("  PIC_KILL_ON_EXIT      set to 1 to terminate the daemon's child");
    console.error("                        on TUI quit (default: detach, leave running)");
}

const args = process.argv.slice(2);
if (args.length === 0 || args[0] === "--help" || args[0] === "-h") {
    usage();
    process.exit(args.length === 0 ? 1 : 0);
}
if (args[0] === "--version" || args[0] === "-V") {
    console.log(`pic-attach ${VERSION}`);
    process.exit(0);
}

const childId = args[0];
const killOnExit = process.env.PIC_KILL_ON_EXIT === "1";
const socket = process.env.PI_CONTROLLER_SOCKET;

// Startup banner per the plan's exit-semantics section.
const childLabel = childId;  // v1 shows childId; a future pass could resolve name via ctrl_get
process.stderr.write(`[pic-attach] Connected to ${childLabel}.\n`);
process.stderr.write(`[pic-attach] Ctrl+D / /quit detaches; the session keeps running.\n`);
process.stderr.write(`[pic-attach] To terminate the session, use \`pic kill ${childLabel}\` from another shell\n`);
process.stderr.write(`[pic-attach] (or relaunch with --kill-on-exit for native pi exit semantics).\n`);
process.stderr.write(`[pic-attach] ${"─".repeat(60)}\n`);

let runtime: RemoteAgentSessionRuntime;
try {
    runtime = await RemoteAgentSessionRuntime.connect({
        socket,
        childId,
        killOnExit,
    });
} catch (err) {
    console.error(`pic-attach: failed to connect: ${err instanceof Error ? err.message : String(err)}`);
    process.exit(2);
}

// Construct and run the TUI.
// InteractiveMode.run() is the main entry point (confirmed from dist/main.js).
let tui: InteractiveMode;
try {
    // The TUI expects an AgentSessionRuntime; ours is duck-typed.
    tui = new InteractiveMode(runtime as any, {});
} catch (err) {
    console.error(`pic-attach: failed to construct TUI: ${err instanceof Error ? err.message : String(err)}`);
    await runtime.dispose();
    process.exit(3);
}

// Clean exit on signals — close the UDS connection (and optionally kill the
// daemon's child if --kill-on-exit was requested).
let shuttingDown = false;
async function gracefulExit(code: number): Promise<void> {
    if (shuttingDown) return;
    shuttingDown = true;
    try {
        await runtime.dispose();
    } catch (err) {
        console.error("pic-attach: error during dispose:", err);
    }
    process.exit(code);
}

process.on("SIGINT", () => void gracefulExit(130));
process.on("SIGTERM", () => void gracefulExit(143));

try {
    await tui.run();
} catch (err) {
    console.error(`pic-attach: TUI error: ${err instanceof Error ? err.message : String(err)}`);
    await gracefulExit(4);
}

// Normal exit (TUI quit naturally via Ctrl+D or /quit).
await gracefulExit(0);
