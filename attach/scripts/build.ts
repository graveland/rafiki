/**
 * build.ts — compile rafiki-attach for the host platform.
 *
 * bun's `--compile --target` names map exactly onto Node's process.platform
 * (darwin|linux) and process.arch (arm64|x64), so the target is derived from the
 * host instead of being hard-coded. To cross-compile for a different platform,
 * use the build:<platform> scripts in package.json.
 */

import { spawnSync } from "node:child_process";

const target = `bun-${process.platform}-${process.arch}`;

const { status } = spawnSync(
    "bun",
    ["build", "--compile", `--target=${target}`, "./src/main.ts", "--outfile", "../bin/rafiki-attach"],
    { stdio: "inherit" },
);

if (status !== 0) {
    process.exit(status ?? 1);
}

console.log(`✓ Compiled rafiki-attach (${target})`);
