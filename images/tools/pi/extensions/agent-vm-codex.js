// Design of record: https://github.com/gregwebs/agent-vm/blob/7438927/docs/adr/0044-pi-reaches-host-codex-through-an-image-owned-provider.md
// Pi 0.87.1 JWT-decodes the apiKey with atob to obtain the account id.
// Pi writes Authorization after caller headers, so only a fetch wrapper can replace it.
// Only SSE uses options.fetch; WebSocket has no injection point.
// Replay preserves encrypted reasoning and call|item ids only when api identity matches.
// A distinct provider keeps guest credentials separate (ADR-0011), but a distinct api
// would silently lose replay identity. This is not an ADR-0023 Pi package: no local
// node_modules; Pi's virtual modules supply the only external imports.
// Guest input must never throw from the factory: Pi treats a load failure as fatal.
import { openSync, fstatSync, readSync, closeSync, constants } from "node:fs";
import { openAICodexResponsesApi } from "@earendil-works/pi-ai/compat";
import { getBuiltinModels } from "@earendil-works/pi-ai/providers/all";

const PROVIDER_ID = "agent-vm-codex";
const CODEX_API = "openai-codex-responses";
// Cross-repo contract with agent-vm secrets.rs; renaming needs a coordinated release.
const ACCESS_PLACEHOLDER = "msb-openai-placeholder-a-v2";
const REFRESH_PLACEHOLDER = "msb-openai-placeholder-r-v2";
const AUTH_FILE = "/agent-vm-state/codex/auth.json";
const MAX_AUTH_BYTES = 65536;
const CHATGPT_ORIGIN = "https://chatgpt.com";
const BASE_URL = "https://chatgpt.com/backend-api";
const REFRESH_URL = "https://auth.openai.com/oauth/token";
const REFRESH_FAILED_MESSAGE = "agent-vm-codex: the host could not refresh its Codex login. Run `codex login` on the host, then start a new turn.";
const HOST_REFUSED_MESSAGE = "agent-vm-codex sends requests only to https://chatgpt.com";
const REDIRECT_REFUSED_MESSAGE = "agent-vm-codex does not follow redirects";

function readHostCodexLogin() {
  // One descriptor prevents a guest swapping a checked file for a symlink or FIFO.
  // The catch includes close failures: malformed guest state must cost only the provider.
  try {
    let fd;
    try {
      fd = openSync(AUTH_FILE, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
      const stat = fstatSync(fd);
      if (!stat.isFile() || stat.size > MAX_AUTH_BYTES) return;
      const buf = Buffer.allocUnsafe(MAX_AUTH_BYTES + 1);
      let n = 0;
      while (n < buf.length) {
        const count = readSync(fd, buf, n, buf.length - n, n);
        if (count === 0) break;
        n += count;
      }
      if (n > MAX_AUTH_BYTES) return;
      const tokens = JSON.parse(buf.toString("utf8", 0, n))?.tokens;
      if (tokens?.access_token !== ACCESS_PLACEHOLDER || tokens?.refresh_token !== REFRESH_PLACEHOLDER) return;
      if (typeof tokens.account_id !== "string" || !/^[\x21-\x7e]+$/.test(tokens.account_id)) return;
      return { accountId: tokens.account_id };
    } finally {
      if (fd !== undefined) closeSync(fd);
    }
  } catch {
    return;
  }
}

function syntheticApiKey(accountId) {
  // atob rejects base64url. This non-secret JWT never reaches the wire.
  const b64 = (value) => Buffer.from(JSON.stringify(value), "utf8").toString("base64");
  return [b64({ alg: "none", typ: "JWT" }), b64({ "https://api.openai.com/auth": { chatgpt_account_id: accountId } }), "agent-vm-unsigned"].join(".");
}

function createPlaceholderFetch(inner) {
  let refreshTried = false;
  let failure;
  const abortError = () => new DOMException("Request was aborted", "AbortError");
  const errorResponse = (status, message) => new Response(JSON.stringify({ error: { message } }), {
    status, headers: { "content-type": "application/json" },
  });
  const refreshFailedResponse = () => errorResponse(401, REFRESH_FAILED_MESSAGE);
  // A 401 here would enter the refresh branch; redirects must never cause refresh.
  const redirectRefusedResponse = () => errorResponse(403, REDIRECT_REFUSED_MESSAGE);
  const settle = async (response) => {
    if (!response.bodyUsed) await response.body?.cancel().catch(() => {});
  };
  return async (input, init) => {
    if (init?.signal?.aborted) throw abortError();
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    if (new URL(url).origin !== CHATGPT_ORIGIN) throw new Error(HOST_REFUSED_MESSAGE);
    if (failure === "refresh") return refreshFailedResponse();
    if (failure === "redirect") return redirectRefusedResponse();
    // Pi can resolve a guest-stored key over the configured one. Never trust its bearer.
    const headers = new Headers(init?.headers);
    headers.set("authorization", `Bearer ${ACCESS_PLACEHOLDER}`);
    const sendInference = async () => {
      const response = await inner(input, { ...init, headers, redirect: "manual" });
      if ((response.status >= 300 && response.status < 400) || response.type === "opaqueredirect") {
        await settle(response);
        failure = "redirect";
        return redirectRefusedResponse();
      }
      return response;
    };
    const first = await sendInference();
    if (first.status !== 401 || refreshTried) return first;
    refreshTried = true;
    await settle(first);
    let refresh;
    try {
      // Inference may be zstd-compressed. Copying its init would corrupt this grant.
      refresh = await inner(REFRESH_URL, {
        method: "POST", headers: { "content-type": "application/json" },
        body: JSON.stringify({ grant_type: "refresh_token", refresh_token: REFRESH_PLACEHOLDER }),
        signal: init?.signal, redirect: "manual",
      });
      const ok = refresh.ok;
      // Drain both outcomes, never parse or expose the body. Raw fetch/body errors
      // match Pi's session retry classifier and could restart a host codex exec.
      await refresh.arrayBuffer();
      if (!ok) failure = "refresh";
    } catch (error) {
      if (init?.signal?.aborted || error?.name === "AbortError") throw abortError();
      failure = "refresh";
    } finally {
      if (refresh) await settle(refresh);
    }
    if (init?.signal?.aborted) throw abortError();
    return failure === "refresh" ? refreshFailedResponse() : sendInference();
  };
}

export default function (pi) {
  const login = readHostCodexLogin();
  if (!login) return;
  const codex = openAICodexResponsesApi();
  pi.registerProvider(PROVIDER_ID, {
    name: "ChatGPT via host Codex login (agent-vm)",
    baseUrl: BASE_URL, apiKey: syntheticApiKey(login.accountId), api: CODEX_API,
    models: getBuiltinModels("openai-codex"),
    streamSimple: (model, context, options) => codex.streamSimple(model, context, {
      ...options, transport: "sse", fetch: createPlaceholderFetch(options?.fetch ?? globalThis.fetch),
    }),
  });
}
