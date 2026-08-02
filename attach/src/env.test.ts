/**
 * Tests for env.ts — the deprecation-fallback env reader.
 *
 * This exists because a rename applied to only one side of the Go/TypeScript
 * boundary fails silently, and has done so twice: the socket path (fell back to
 * pi-controller's socket) and the child id (killed the daemon's /reload
 * command). The fallback semantics must match internal/envvar.Get exactly.
 */

import { describe, expect, it, beforeEach, afterEach } from "bun:test";
import { envValue, envFlag, envIsSet } from "./env.ts";

const NEW = "RAFIKI_TEST_VAR";
const OLD = "PIC_TEST_VAR";

describe("env", () => {
    let saved: Record<string, string | undefined> = {};

    beforeEach(() => {
        saved = { [NEW]: process.env[NEW], [OLD]: process.env[OLD] };
        delete process.env[NEW];
        delete process.env[OLD];
    });

    afterEach(() => {
        for (const k of [NEW, OLD]) {
            if (saved[k] === undefined) delete process.env[k];
            else process.env[k] = saved[k];
        }
    });

    it("envValue: current name wins over the legacy spelling", () => {
        process.env[NEW] = "new";
        process.env[OLD] = "old";
        expect(envValue(NEW, OLD)).toBe("new");
    });

    it("envValue: falls back to the legacy spelling", () => {
        process.env[OLD] = "old";
        expect(envValue(NEW, OLD)).toBe("old");
    });

    it("envValue: undefined when neither is set", () => {
        expect(envValue(NEW, OLD)).toBeUndefined();
    });

    // Matches envvar.Get, which treats "" as unset because os.Getenv cannot
    // distinguish empty from absent.
    it("envValue: an empty value counts as unset, and falls through", () => {
        process.env[NEW] = "";
        process.env[OLD] = "old";
        expect(envValue(NEW, OLD)).toBe("old");
    });

    it("envValue: empty on both sides is undefined, not \"\"", () => {
        process.env[NEW] = "";
        process.env[OLD] = "";
        expect(envValue(NEW, OLD)).toBeUndefined();
    });

    it("envValue: works with no legacy name given", () => {
        process.env[NEW] = "new";
        expect(envValue(NEW)).toBe("new");
        expect(envValue("RAFIKI_UNSET_ENTIRELY")).toBeUndefined();
    });

    it("envFlag: only exactly \"1\" is true", () => {
        expect(envFlag(NEW, OLD)).toBe(false);
        process.env[NEW] = "1";
        expect(envFlag(NEW, OLD)).toBe(true);
        process.env[NEW] = "true";
        expect(envFlag(NEW, OLD)).toBe(false);
        process.env[NEW] = "0";
        expect(envFlag(NEW, OLD)).toBe(false);
    });

    it("envFlag: honours the legacy spelling", () => {
        process.env[OLD] = "1";
        expect(envFlag(NEW, OLD)).toBe(true);
    });

    it("envIsSet: true for any non-empty value under either name", () => {
        expect(envIsSet(NEW, OLD)).toBe(false);
        process.env[OLD] = "child-42";
        expect(envIsSet(NEW, OLD)).toBe(true);
        delete process.env[OLD];
        process.env[NEW] = "child-42";
        expect(envIsSet(NEW, OLD)).toBe(true);
    });
});
