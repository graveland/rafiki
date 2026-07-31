#!/usr/bin/env bun

import { InteractiveMode } from "@earendil-works/pi-coding-agent";
import { RemoteAgentSessionRuntime } from "./runtime.ts";
import { restoreTerminal } from "./session.ts";
import { envFlag } from "./env.ts";

const VERSION = "0.1.0";

// Matches what pi's own cli.js does before constructing InteractiveMode.
process.env.PI_CODING_AGENT = "true";
process.emitWarning = () => {};

function usage(): void {
    console.error(`fundi-attach v${VERSION}`);
    console.error("usage: fundi-attach <childId>");
    console.error("");
    console.error("env vars:");
    console.error("  FUNDI_SOCKET        override default socket path");
    console.error("  FUNDI_KILL_ON_EXIT  set to 1 to terminate the daemon's child");
    console.error("                      when the TUI quits (fallback for direct invocation;");
    console.error("                      when launched via fundi, kill/keep is decided by fundi)");
    console.error("  FUNDI_ATTACH_TAIL   scrollback events to replay (-1 all, 0 none)");
    console.error("  FUNDI_ATTACH_DEBUG  set to 1 to log every received event to stderr");
}

const args = process.argv.slice(2);
if (args.length === 0 || args[0] === "--help" || args[0] === "-h") {
    usage();
    process.exit(args.length === 0 ? 1 : 0);
}
if (args[0] === "--version" || args[0] === "-V") {
    console.log(`fundi-attach ${VERSION}`);
    process.exit(0);
}

const childId = args[0];

// Signal to extensions that they're running inside fundi-attach's TUI process.
// fundi-helpers/index.ts reads these to register the autocomplete provider instead
// of the daemon-side /reload command. Only the current spellings are set; the
// extension accepts the pre-rename ones so an out-of-date on-disk copy still
// works until the next install.
process.env.FUNDI_ATTACH_TUI = "1";
process.env.FUNDI_ATTACH_CHILD_ID = childId;

// FUNDI_KILL_ON_EXIT is a fallback for users invoking fundi-attach directly.
// When launched via `fundi create` / `fundi attach`, fundi handles the kill/keep
// decision after this process exits; the env var is not set.
const killOnExit = envFlag("FUNDI_KILL_ON_EXIT", "PIC_KILL_ON_EXIT");
// Socket resolution is client.ts's job (defaultSocketPath): one place that has
// to agree with the Go side's internal/paths, rather than two that can drift.

// Startup banner.
const childLabel = childId;
process.stderr.write(`[fundi-attach] Connected to ${childLabel}.\n`);
process.stderr.write(`[fundi-attach] ${"─".repeat(60)}\n`);

let runtime: RemoteAgentSessionRuntime;
try {
    runtime = await RemoteAgentSessionRuntime.connect({
        childId,
        killOnExit,
    });
} catch (err) {
    console.error(`fundi-attach: failed to connect: ${err instanceof Error ? err.message : String(err)}`);
    process.exit(2);
}

// Construct and run the TUI.
let tui: InteractiveMode;
try {
    // The TUI expects an AgentSessionRuntime; ours is duck-typed.
    tui = new InteractiveMode(runtime as any, {});
} catch (err) {
    console.error(`fundi-attach: failed to construct TUI: ${err instanceof Error ? err.message : String(err)}`);
    await runtime.dispose();
    process.exit(3);
}

// Signal handlers: on SIGINT/SIGTERM, dispose (which honours killOnExit if
// set) and exit. No prompt — the Go parent (fundi) will also receive the signal
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
        console.error("fundi-attach: error during dispose:", err);
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
    console.error(`fundi-attach: TUI error: ${err instanceof Error ? err.message : String(err)}`);
    await gracefulExit(4);
}

// Normal exit (TUI quit naturally via Ctrl+D or /quit).
// Dispose here closes the UDS connection (and kills if FUNDI_KILL_ON_EXIT=1).
// The kill/keep decision for fundi-managed sessions is made by fundi (Go side)
// after this process exits.
if (!shuttingDown) {
    shuttingDown = true;
    await runtime.dispose();
    process.exit(0);
}
