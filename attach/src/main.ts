#!/usr/bin/env bun

import { InteractiveMode } from "@earendil-works/pi-coding-agent";
import { RemoteAgentSessionRuntime } from "./runtime.ts";
import { restoreTerminal } from "./session.ts";

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
    console.error("                        when the TUI quits (fallback for direct invocation;");
    console.error("                        when launched via pic, kill/keep is decided by pic)");
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

// Signal to extensions that they're running inside pic-attach's TUI process.
// pic-helpers/index.ts reads these to register the autocomplete provider instead
// of the daemon-side /reload command.
process.env.PIC_ATTACH_TUI = "1";
process.env.PIC_ATTACH_CHILD_ID = childId;

// PIC_KILL_ON_EXIT is a fallback for users invoking pic-attach directly.
// When launched via `pic create` / `pic attach`, pic handles the kill/keep
// decision after this process exits; the env var is not set.
const killOnExit = process.env.PIC_KILL_ON_EXIT === "1";
const socket = process.env.PI_CONTROLLER_SOCKET;

// Startup banner.
const childLabel = childId;
process.stderr.write(`[pic-attach] Connected to ${childLabel}.\n`);
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
let tui: InteractiveMode;
try {
    // The TUI expects an AgentSessionRuntime; ours is duck-typed.
    tui = new InteractiveMode(runtime as any, {});
} catch (err) {
    console.error(`pic-attach: failed to construct TUI: ${err instanceof Error ? err.message : String(err)}`);
    await runtime.dispose();
    process.exit(3);
}

// Signal handlers: on SIGINT/SIGTERM, dispose (which honours killOnExit if
// set) and exit. No prompt — the Go parent (pic) will also receive the signal
// and skip the prompt on its side.
let shuttingDown = false;
async function gracefulExit(code: number): Promise<void> {
    if (shuttingDown) return;
    shuttingDown = true;
    // Restore the terminal before exiting from a signal-driven path — the
    // TUI is still running here and pi's own teardown may not get a chance
    // before we process.exit().  See session.ts: restoreTerminal.
    restoreTerminal();
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
    // TUI crashed mid-run: pi did not finish its cleanup pass, so the
    // terminal is likely still in raw mode / alt screen.  Restore explicitly.
    restoreTerminal();
    console.error(`pic-attach: TUI error: ${err instanceof Error ? err.message : String(err)}`);
    await gracefulExit(4);
}

// Normal exit (TUI quit naturally via Ctrl+D or /quit).
// Dispose here closes the UDS connection (and kills if PIC_KILL_ON_EXIT=1).
// The kill/keep decision for pic-managed sessions is made by pic (Go side)
// after this process exits.
if (!shuttingDown) {
    shuttingDown = true;
    await runtime.dispose();
    process.exit(0);
}
