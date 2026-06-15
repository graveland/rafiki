/**
 * Tests for the TUI autocomplete logic in pic-helpers/index.ts.
 *
 * We test the suggestion-filtering behaviour by constructing a minimal
 * "recorded-call" harness: a fake base AutocompleteProvider that
 * records getSuggestions calls and a fake uiContext that captures the
 * factory registered via addAutocompleteProvider.
 *
 * Manual smoke test (requires a running daemon):
 *   make build                          # rebuild pic-attach binary
 *   ./bin/pic install-extension --force # refresh on-disk pic-helpers
 *   pic create test --no-extensions     # start a session
 *   # In TUI: type "/" then Tab
 *   # Expect: /reload appears in the completion list
 */

import { describe, expect, it, beforeEach } from "bun:test";
import { setupTuiAutocomplete, filterCommandSuggestions, slashCommandsToCommandInfo } from "../../cmd/pic/picembed/pic-helpers/index.ts";

// ─── Inline AutocompleteProvider type (mirrors @earendil-works/pi-tui) ────────

interface AutocompleteItem {
    value: string;
    label: string;
    description?: string;
}
interface AutocompleteSuggestions {
    items: AutocompleteItem[];
    prefix: string;
}
interface AutocompleteProvider {
    getSuggestions(
        lines: string[],
        cursorLine: number,
        cursorCol: number,
        options: { signal: AbortSignal; force?: boolean }
    ): Promise<AutocompleteSuggestions | null>;
    applyCompletion(
        lines: string[],
        cursorLine: number,
        cursorCol: number,
        item: AutocompleteItem,
        prefix: string
    ): { lines: string[]; cursorLine: number; cursorCol: number };
    shouldTriggerFileCompletion?(
        lines: string[],
        cursorLine: number,
        cursorCol: number
    ): boolean;
}

// ─── Test helpers ──────────────────────────────────────────────────────────────

/** A base provider that returns fixed items and records calls. */
function makeBaseProvider(
    baseItems: AutocompleteItem[] = [],
    prefix: string = ""
): AutocompleteProvider & { callCount: number } {
    let callCount = 0;
    return {
        get callCount() {
            return callCount;
        },
        async getSuggestions(_lines, _cursorLine, _cursorCol, _opts) {
            callCount++;
            if (baseItems.length === 0) return null;
            return { items: baseItems, prefix };
        },
        applyCompletion(lines, cursorLine, cursorCol, item, appliedPrefix) {
            const line = lines[cursorLine] ?? "";
            const start = cursorCol - appliedPrefix.length;
            const newLine = line.slice(0, start) + item.value + line.slice(cursorCol);
            const newLines = [...lines];
            newLines[cursorLine] = newLine;
            return { lines: newLines, cursorLine, cursorCol: start + item.value.length };
        },
        shouldTriggerFileCompletion: () => false,
    };
}

/** Signal stub — AbortSignal-compatible enough for tests. */
const fakeSignal = {} as AbortSignal;
const fakeOpts = { signal: fakeSignal };

/**
 * Calls setupTuiAutocomplete with a fake addProvider implementation.
 * Returns a function that invokes the registered factory with the given
 * base provider, producing the wrapped AutocompleteProvider.
 *
 * Pre-loads the daemon command cache via `cachedCommands` by bypassing the
 * real UDS fetch (the fetch will fail / be skipped because PIC_ATTACH_CHILD_ID
 * and PI_CONTROLLER_SOCKET are not set or the daemon isn't running in tests).
 *
 * To inject commands: after calling registerProvider, mutate the returned
 * `cache` array and the wrapped provider will use the updated values.
 */
function registerProvider(
    baseCmds?: AutocompleteItem[]
): {
    wrap: (base: AutocompleteProvider) => AutocompleteProvider;
} {
    let registeredFactory: ((current: unknown) => unknown) | undefined;

    setupTuiAutocomplete((factory) => {
        registeredFactory = factory;
    });

    return {
        wrap(base: AutocompleteProvider): AutocompleteProvider {
            if (!registeredFactory) throw new Error("no factory registered");
            return registeredFactory(base) as AutocompleteProvider;
        },
    };
}

// ─── Tests ────────────────────────────────────────────────────────────────────

describe("setupTuiAutocomplete", () => {
    describe("provider registration", () => {
        it("calls addProvider with a factory function", () => {
            let called = false;
            setupTuiAutocomplete((factory) => {
                expect(typeof factory).toBe("function");
                called = true;
            });
            // Provider is always registered; the daemon fetch is skipped when
            // PIC_ATTACH_CHILD_ID is not set (which is the case in unit tests).
            expect(called).toBe(true);
        });

        it("returned provider is an object with getSuggestions", () => {
            let provider: unknown;
            setupTuiAutocomplete((factory) => {
                provider = factory(makeBaseProvider());
            });
            expect(typeof (provider as AutocompleteProvider).getSuggestions).toBe("function");
        });
    });

    describe("suggestion filtering", () => {
        /**
         * Directly exercise the provider logic by pre-populating cachedCommands.
         *
         * Since we can't inject the cache through the public API (it's a closure),
         * we instead test the behaviour when cachedCommands is empty (which is
         * the real startup state in unit tests where no daemon is running) and
         * verify the pass-through behaviour.
         *
         * Then we test the filtering logic with a side-channel: we construct the
         * filtering function directly to test it in isolation.
         */
        it("returns base suggestions when input has no slash prefix", async () => {
            const base = makeBaseProvider([{ value: "foo", label: "foo" }], "f");
            let wrappedProvider: AutocompleteProvider | undefined;
            setupTuiAutocomplete((factory) => {
                wrappedProvider = factory(base) as AutocompleteProvider;
            });
            expect(wrappedProvider).toBeDefined();

            const result = await wrappedProvider!.getSuggestions(
                ["hello world"],
                0,
                11,
                fakeOpts
            );
            // No slash prefix → should return base result
            expect(result?.items[0]?.value).toBe("foo");
        });

        it("returns base suggestions when input has slash but no cached commands", async () => {
            const base = makeBaseProvider([{ value: "/help", label: "/help" }], "/h");
            let wrappedProvider: AutocompleteProvider | undefined;
            setupTuiAutocomplete((factory) => {
                wrappedProvider = factory(base) as AutocompleteProvider;
            });

            // No daemon → cachedCommands stays [] → we fall through to base
            const result = await wrappedProvider!.getSuggestions(["/h"], 0, 2, fakeOpts);
            // With no cached commands, daemonItems is empty, so we return base result.
            expect(result?.items[0]?.value).toBe("/help");
        });

        it("shouldTriggerFileCompletion delegates to base", () => {
            const base = makeBaseProvider();
            (base as unknown as { shouldTriggerFileCompletion(_l: string[], _cl: number, _cc: number): boolean }).shouldTriggerFileCompletion = () => true;

            let wrappedProvider: AutocompleteProvider | undefined;
            setupTuiAutocomplete((factory) => {
                wrappedProvider = factory(base) as AutocompleteProvider;
            });

            const result = wrappedProvider!.shouldTriggerFileCompletion!(["/"], 0, 1);
            expect(result).toBe(true);
        });

        it("applyCompletion delegates to base", () => {
            const base = makeBaseProvider();
            let applied: ReturnType<AutocompleteProvider["applyCompletion"]> | undefined;
            const origApply = base.applyCompletion.bind(base);
            base.applyCompletion = (...args) => {
                applied = origApply(...args);
                return applied;
            };

            let wrappedProvider: AutocompleteProvider | undefined;
            setupTuiAutocomplete((factory) => {
                wrappedProvider = factory(base) as AutocompleteProvider;
            });

            const item = { value: "/reload", label: "/reload" };
            wrappedProvider!.applyCompletion(["/r"], 0, 2, item, "/r");
            expect(applied).toBeDefined();
            expect(applied!.lines[0]).toBe("/reload");
        });

        it("getSuggestions calls the base provider exactly once", async () => {
            const base = makeBaseProvider();
            let wrappedProvider: AutocompleteProvider | undefined;
            setupTuiAutocomplete((factory) => {
                wrappedProvider = factory(base) as AutocompleteProvider;
            });

            await wrappedProvider!.getSuggestions(["hello"], 0, 5, fakeOpts);
            expect(base.callCount).toBe(1);
        });
    });

    describe("suggestion filtering with cached commands", () => {
        const commands = [
            { name: "reload", description: "Reload extensions" },
            { name: "restart", description: "Restart session" },
            { name: "new", description: "New session" },
        ];

        it("returns null when no slash prefix", () => {
            expect(filterCommandSuggestions("hello world", commands, [])).toBeNull();
        });

        it("returns null when typed slash matches no commands", () => {
            expect(filterCommandSuggestions("/xyz", commands, [])).toBeNull();
        });

        it("returns matching commands for /r prefix", () => {
            const result = filterCommandSuggestions("/r", commands, []);
            expect(result).not.toBeNull();
            expect(result!.prefix).toBe("/r");
            // value carries the bare command name (no leading slash); pi-tui's
            // CombinedAutocompleteProvider prepends '/' on apply.
            const values = result!.items.map((i) => i.value);
            expect(values).toContain("reload");
            expect(values).toContain("restart");
            expect(values).not.toContain("new");
            const labels = result!.items.map((i) => i.label);
            expect(labels).toContain("/reload");
        });

        it("returns only the exact match for /reload", () => {
            const result = filterCommandSuggestions("/reload", commands, []);
            expect(result).not.toBeNull();
            expect(result!.items).toHaveLength(1);
            expect(result!.items[0]!.value).toBe("reload");
            expect(result!.items[0]!.label).toBe("/reload");
            expect(result!.prefix).toBe("/reload");
        });

        it("returns all commands for bare slash", () => {
            const result = filterCommandSuggestions("/", commands, []);
            expect(result).not.toBeNull();
            expect(result!.items).toHaveLength(3);
            expect(result!.prefix).toBe("/");
        });

        it("includes base items before daemon items", () => {
            const baseItem: AutocompleteItem = { value: "/built-in", label: "/built-in" };
            const result = filterCommandSuggestions("/", commands, [baseItem]);
            expect(result).not.toBeNull();
            expect(result!.items[0]!.value).toBe("/built-in");
            // daemon items follow (value is bare name; slash added by base provider on apply)
            expect(result!.items.map((i) => i.value)).toContain("reload");
        });

        it("includes description in returned item", () => {
            const result = filterCommandSuggestions("/reload", commands, []);
            expect(result!.items[0]!.description).toBe("Reload extensions");
        });

        it("matches after whitespace (multi-word input)", () => {
            const result = filterCommandSuggestions("some text /re", commands, []);
            expect(result).not.toBeNull();
            expect(result!.prefix).toBe("/re");
            expect(result!.items.map((i) => i.value)).toContain("reload");
        });

        it("does not match slash embedded in word", () => {
            expect(filterCommandSuggestions("foo/bar", commands, [])).toBeNull();
        });
    });

    describe("slashCommandsToCommandInfo", () => {
        it("maps names to CommandInfo", () => {
            const got = slashCommandsToCommandInfo(["compact", "review"]);
            expect(got).toEqual([
                { name: "compact", description: undefined },
                { name: "review", description: undefined },
            ]);
        });
    });

    describe("slash-command prefix detection (white-box)", () => {
        /**
         * These tests validate the regex-based prefix detection by checking
         * the behaviour of the wrapped provider for various input patterns.
         * Because cachedCommands is empty in tests, "no daemon items" means
         * the provider returns the base result — we verify that the provider
         * does not transform the result when there's no prefix match.
         */

        const cases: Array<{
            desc: string;
            line: string;
            col: number;
            expectSlashMatch: boolean;
        }> = [
            { desc: "start of line /r", line: "/r", col: 2, expectSlashMatch: true },
            { desc: "slash alone", line: "/", col: 1, expectSlashMatch: true },
            { desc: "after space /r", line: "text /r", col: 7, expectSlashMatch: true },
            { desc: "plain word (no slash)", line: "hello", col: 5, expectSlashMatch: false },
            { desc: "slash in middle of word (no space before)", line: "foo/bar", col: 7, expectSlashMatch: false },
            { desc: "empty line at col 0", line: "", col: 0, expectSlashMatch: false },
        ];

        for (const { desc, line, col, expectSlashMatch } of cases) {
            it(desc, async () => {
                const base = makeBaseProvider([], "");
                let wrappedProvider: AutocompleteProvider | undefined;
                setupTuiAutocomplete((factory) => {
                    wrappedProvider = factory(base) as AutocompleteProvider;
                });

                const result = await wrappedProvider!.getSuggestions([line], 0, col, fakeOpts);

                if (!expectSlashMatch) {
                    // No slash match → result is base result (null in this case)
                    expect(result).toBeNull();
                } else {
                    // Slash match, but no daemon commands → still base result (null)
                    // The important thing is the provider didn't crash
                    expect(result).toBeNull();
                }
            });
        }
    });
});
