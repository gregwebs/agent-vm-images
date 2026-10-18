// Keep in sync with verify-pi.sh.
export default function (pi) {
  pi.on("session_start", (_event, ctx) => {
    const p = ctx.modelRegistry.getProvider("agent-vm-codex");
    const n = p && typeof p.getModels === "function" ? p.getModels().length
            : p && Array.isArray(p.models) ? p.models.length : 0;
    ctx.ui.notify(p ? `AGENT-VM-CODEX-REGISTERED models=${n}` : "AGENT-VM-CODEX-MISSING", "warning");
  });
}
