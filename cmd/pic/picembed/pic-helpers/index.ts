import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

/**
 * pic-helpers — pic-attach-aware slash commands.
 *
 * Registers commands that are useful when running pi via the pi-controller
 * daemon (i.e., driven by pic-attach). These commands also work in native
 * pi, where they're harmless overrides of the built-ins.
 */
export default function(pi: ExtensionAPI) {
    pi.registerCommand("reload", {
        description: "Reload extensions, skills, prompts, and themes",
        handler: async (_args, ctx) => {
            await ctx.reload();
        },
    });

    // Future: /detach (explicit detach signal — no-op for now, useful when
    // pic-attach gains a "what's going on" inspection feature), /restart-self
    // (kill+resume via the controller from inside the TUI), etc.
}
