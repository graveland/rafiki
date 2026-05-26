#!/usr/bin/env bun

import { InteractiveMode } from "@earendil-works/pi-coding-agent";
import { stdin, stdout } from "node:process";
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
    console.error("                        on TUI quit (skips exit prompt)");
    console.error("  PIC_KEEP_ON_EXIT      set to 1 to always keep the session running");
    console.error("                        on TUI quit (skips exit prompt)");
    console.error("                        (default without either: prompt at exit time)");
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
const keepOnExit = process.env.PIC_KEEP_ON_EXIT === "1";
const socket = process.env.PI_CONTROLLER_SOCKET;

// Startup banner.
const childLabel = childId;
process.stderr.write(`[pic-attach] Connected to ${childLabel}.\n`);
if (killOnExit) {
    process.stderr.write(`[pic-attach] Ctrl+D / /quit will terminate the session (--kill-on-exit).\n`);
} else if (keepOnExit) {
    process.stderr.write(`[pic-attach] Ctrl+D / /quit will detach; the session keeps running (--keep-on-exit).\n`);
} else {
    process.stderr.write(`[pic-attach] Ctrl+D / /quit will prompt to keep or terminate the session.\n`);
    process.stderr.write(`[pic-attach] Use --keep-on-exit or --kill-on-exit to skip the prompt.\n`);
}
process.stderr.write(`[pic-attach] ${"─".repeat(60)}\n`);

// Always construct the runtime with killOnExit=false; we decide kill vs keep
// after the TUI exits (or honour the env vars directly).
let runtime: RemoteAgentSessionRuntime;
try {
    runtime = await RemoteAgentSessionRuntime.connect({
        socket,
        childId,
        killOnExit: false,
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

// Signal handlers: no prompt on signal-driven exit — default to keep.
let shuttingDown = false;
async function gracefulExit(code: number): Promise<void> {
    if (shuttingDown) return;
    shuttingDown = true;
    try {
        // No prompt on signal-driven exit; just detach.
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
// Ask what to do unless an explicit flag was given.
if (!shuttingDown) {
    shuttingDown = true;

    const shouldKill = await decideKillOnExit(killOnExit, keepOnExit, childLabel);

    if (shouldKill) {
        await runtime.killChild();
    }
    await runtime.dispose();
    process.exit(0);
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

/**
 * Decide whether to kill the session based on env flags or user prompt.
 * Returns true → kill, false → keep/detach.
 */
async function decideKillOnExit(
    kill: boolean,
    keep: boolean,
    childLabel: string
): Promise<boolean> {
    if (kill) return true;
    if (keep) return false;
    return await promptKillOrKeep(childLabel);
}

/**
 * Prompt the user to keep or terminate the session.
 * Default answer (Enter / empty input) is keep.
 */
async function promptKillOrKeep(childLabel: string): Promise<boolean> {
    // Restore line mode in case the TUI left stdin in raw mode.
    if (stdin.isTTY && stdin.setRawMode) {
        stdin.setRawMode(false);
    }

    stdout.write("\n");
    stdout.write(`Session "${childLabel}" is still running.\n`);
    stdout.write(`  K  Keep running (detach, default)\n`);
    stdout.write(`  T  Terminate the session\n`);
    stdout.write(`Choice [K/t]: `);

    return await new Promise<boolean>((resolve) => {
        const onData = (chunk: Buffer | string) => {
            const ans = (typeof chunk === "string" ? chunk : chunk.toString("utf8"))
                .trim()
                .toLowerCase();
            stdin.off("data", onData);
            stdin.off("end", onEnd);
            if (ans === "t" || ans === "x" || ans === "terminate" || ans === "kill") {
                resolve(true);
            } else {
                if (ans !== "" && ans !== "k" && ans !== "keep") {
                    stdout.write(`(treating as keep)\n`);
                }
                resolve(false);
            }
        };
        const onEnd = () => {
            stdin.off("data", onData);
            stdout.write(`\n(stdin closed, treating as keep)\n`);
            resolve(false);
        };
        stdin.on("data", onData);
        stdin.once("end", onEnd);
    });
}
