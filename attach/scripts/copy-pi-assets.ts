/**
 * copy-pi-assets.ts — post-build step
 *
 * When bun compiles pic-attach to a static binary (isBunBinary = true), pi's
 * config.js resolves package assets relative to dirname(process.execPath), i.e.
 * the bin/ directory. This script copies the necessary asset files there so the
 * TUI can find them at runtime.
 *
 * Files needed in bin/ alongside pic-attach:
 *   package.json      — read at module init to get APP_NAME, VERSION, etc.
 *   theme/dark.json   — built-in TUI themes (loaded at startup)
 *   theme/light.json
 *   theme/theme-schema.json
 *   assets/           — clankolas.png and any other bundled assets
 */

import { copyFileSync, mkdirSync, readdirSync } from "node:fs";
import { join, resolve } from "node:path";

const srcRoot = resolve("node_modules/@earendil-works/pi-coding-agent");
const binDir = resolve("../bin");

// package.json
copyFileSync(join(srcRoot, "package.json"), join(binDir, "package.json"));

// theme files
const themeDir = join(srcRoot, "dist/modes/interactive/theme");
const outThemeDir = join(binDir, "theme");
mkdirSync(outThemeDir, { recursive: true });
for (const f of readdirSync(themeDir).filter((n) => n.endsWith(".json"))) {
    copyFileSync(join(themeDir, f), join(outThemeDir, f));
}

// assets files
const assetsDir = join(srcRoot, "dist/modes/interactive/assets");
const outAssetsDir = join(binDir, "assets");
mkdirSync(outAssetsDir, { recursive: true });
for (const f of readdirSync(assetsDir)) {
    copyFileSync(join(assetsDir, f), join(outAssetsDir, f));
}

console.log("✓ Copied pi assets to bin/");
