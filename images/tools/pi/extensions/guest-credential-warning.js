// The one Pi extension the agent-vm wrapper always loads explicitly, so
// `--no-extensions` cannot silence it and no project state can remove it. See
// docs/adr/0012-stable-pi-image-customization-seam.md.
//
// Plain ESM, zero imports, zero npm dependencies, on purpose: Pi loads it
// through jiti with no `node_modules` beside it and no writable directory (the
// file lives under a root-owned /opt).
//
// #96 maps Pi's guest-owned auth to per-project persisted state, so guest sign-ins
// are scoped to this project's state in the base advisory.
// This advisory is not a boundary; the microVM is. The host-import clause
// describes the Codex credential provisioned by agent-vm#360, never Pi's own
// auth.json. ADR-0044 D7 requires observed registration, not a duplicate file
// predicate: this mandatory extension has no file I/O and must never throw.
// No host credential shares a guest provider id, so no precedence claim applies
// (ADR-0011). A guest models.json or extension can spoof this registration;
// the warning remains advisory, as ADR-0044's Consequences acknowledge.
//
// #93 added a *separate*, host-side surface: `agent-vm` reports EXISTING
// guest-managed Pi state on every launch, and `agent-vm doctor` shows the same
// facts. That report is complementary to this future-sign-in advisory, runs
// whether or not Pi starts, and does not inherit this `hasUI` gate -- so the
// two must not be merged into one message.

const WARNING = [
  "agent-vm: signing in here (for example with /login) writes a credential into",
  "THIS project's persistent guest state, where any process in this guest can",
  "read it. The microVM -- not this warning -- is the boundary.",
].join(" ");

const HOST_IMPORT = "Credentials imported from your host stay on the host and never appear in guest files.";
const CODEX_PROVIDER_ID = "agent-vm-codex";

export default function (pi) {
  pi.on("session_start", (_event, ctx) => {
    // hasUI is true exactly for the two modes a human reads (tui, rpc) and
    // false for print and json, whose stdout is consumed by programs. One
    // predicate, so "suppress in print/json" is not a second mechanism.
    if (!ctx.hasUI) {
      return;
    }
    let message = WARNING;
    try {
      if (ctx.modelRegistry.getProvider(CODEX_PROVIDER_ID)) message += " " + HOST_IMPORT;
    } catch {
      // An advisory must never throw or vanish because a registry lookup failed.
    }
    ctx.ui.notify(message, "warning");
  });
}
