/**
 * Environment variables rafiki-attach reads, with a deprecation fallback to the
 * pre-rename PIC_* spellings.
 *
 * This mirrors the Go side's internal/envvar.Get: the current name wins, and a
 * value found only under the old name is still honoured. Keeping the same policy
 * on both sides matters because several of these cross the boundary — Go sets
 * them, TypeScript reads them — and a rename applied to only one half fails
 * silently. That is exactly how the daemon's /reload command died: the Go
 * envvar migration renamed PI_CONTROLLER_CHILD_ID to RAFIKI_CHILD_ID, the
 * extension kept checking the old name, and its "am I a daemon child?" test
 * simply became permanently false.
 */

/** Returns the value of name, else of legacy, else undefined. Empty is unset. */
export function envValue(name: string, legacy?: string): string | undefined {
    const current = process.env[name];
    if (current !== undefined && current !== "") return current;
    if (legacy === undefined) return undefined;
    const old = process.env[legacy];
    return old !== undefined && old !== "" ? old : undefined;
}

/** True when name (or legacy) is set to exactly "1". */
export function envFlag(name: string, legacy?: string): boolean {
    return envValue(name, legacy) === "1";
}

/** True when name (or legacy) is set to any non-empty value. */
export function envIsSet(name: string, legacy?: string): boolean {
    return envValue(name, legacy) !== undefined;
}
