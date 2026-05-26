#!/usr/bin/env bun

// pic-attach: thin TUI client for pi-controller-managed children.
// This task delivers the skeleton; subsequent tasks add the UDS client,
// remote runtime, and TUI wiring.

const VERSION = "0.1.0";

function usage(): void {
    console.error(`pic-attach v${VERSION}`);
    console.error("usage: pic-attach <childId>");
    console.error("");
    console.error("Connects to the pi-controller daemon's UDS socket and opens");
    console.error("the pi TUI driving the given child.");
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

// Skeleton: just echo what we'd do.
const childId = args[0];
console.log(`pic-attach v${VERSION} — would connect to child ${childId}`);
console.log("(skeleton only; runtime wiring lands in subsequent tasks)");
