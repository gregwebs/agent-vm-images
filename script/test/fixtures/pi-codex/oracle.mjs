// This oracle exercises the registered provider, not an imported copy of its internals.
import assert from "node:assert/strict";
import http from "node:http";
import { isRetryableAssistantError } from "@earendil-works/pi-ai";
import { openAICodexResponsesApi } from "@earendil-works/pi-ai/compat";
import { getBuiltinModels } from "@earendil-works/pi-ai/providers/all";

const REFRESH_FAILED = "agent-vm-codex: the host could not refresh its Codex login. Run `codex login` on the host, then start a new turn.";
const HOST_REFUSED = "agent-vm-codex sends requests only to https://chatgpt.com";
const REDIRECT_REFUSED = "agent-vm-codex does not follow redirects";
const ACCESS = "Bearer msb-openai-placeholder-a-v2";
const GRANT = { grant_type: "refresh_token", refresh_token: "msb-openai-placeholder-r-v2" };
const b64 = (v) => Buffer.from(JSON.stringify(v)).toString("base64");
const jwt = (id, signature = "oracle") => `${b64({ alg: "none" })}.${b64({ "https://api.openai.com/auth": { chatgpt_account_id: id } })}.${signature}`;
const user = { role: "user", content: "test", timestamp: 1 };
const context = { messages: [user] };
const tool = { name: "read", description: "read a file", parameters: { type: "object", properties: { path: { type: "string" } }, required: ["path"] } };
const reasoning = { type: "reasoning", id: "rs_test", encrypted_content: "encrypted-test", summary: [] };
const call = { type: "function_call", id: "fc_test", call_id: "call_test", name: "read", arguments: '{"path":"/tmp/t"}' };
const completed = (output = []) => ({ type: "response.completed", response: { status: "completed", output, usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } });
function sse(events) {
  return events.map((event) => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`).join("");
}
function textSse(text = "ok") {
  const item = { type: "message", id: "msg_test", role: "assistant", content: [] };
  return sse([
    { type: "response.output_item.added", output_index: 0, item },
    { type: "response.output_text.delta", output_index: 0, content_index: 0, delta: text },
    { type: "response.output_item.done", output_index: 0, item: { ...item, content: [{ type: "output_text", text }] } },
    completed(),
  ]);
}
function toolSse() {
  return sse([reasoning, call].flatMap((item, output_index) => [
    { type: "response.output_item.added", output_index, item },
    { type: "response.output_item.done", output_index, item },
  ]).concat(completed([reasoning, call])));
}
class TrackedResponse extends Response {
  drained = false;
  cancelled = false;
  constructor(status, text, { bodyError = false, afterDrain } = {}) {
    let owner;
    const body = new ReadableStream({
      start(controller) {
        controller.enqueue(new TextEncoder().encode(text));
        // Error on demand during consumption, not during initial queue filling.
      },
      pull(controller) {
        if (bodyError) controller.error(new Error("BODY-READ-CANARY"));
        else controller.close();
      },
      cancel() { owner.cancelled = true; },
    });
    super(body, { status, headers: { "content-type": status === 200 ? "text/event-stream" : "application/json" } });
    owner = this;
    this.afterDrain = afterDrain;
  }
  async arrayBuffer() {
    const result = await super.arrayBuffer();
    this.drained = true;
    this.afterDrain?.();
    return result;
  }
}
function trackNative(response) {
  const state = { drained: false, cancelled: false };
  const read = response.arrayBuffer.bind(response);
  response.arrayBuffer = async () => { const bytes = await read(); state.drained = true; return bytes; };
  const cancel = response.body.cancel.bind(response.body);
  response.body.cancel = async (...args) => { state.cancelled = true; return cancel(...args); };
  return { response, state };
}
function countingWebSocket() {
  const original = globalThis.WebSocket;
  let count = 0;
  globalThis.WebSocket = class { constructor() { count++; throw new Error("SOCKET-CANARY"); } };
  return { get count() { return count; }, restore() { globalThis.WebSocket = original; } };
}
async function listen(handler) {
  const server = http.createServer(handler);
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  return { server, url: `http://127.0.0.1:${server.address().port}` };
}
async function servers() {
  const targets = [], wire = [];
  const target = await listen((req, res) => { targets.push(req.url); res.end(textSse()); });
  const source = await listen((req, res) => {
    res.writeHead(req.url === "/307" ? 307 : 302, { location: target.url });
    res.end("REDIRECT BODY");
  });
  const refresh = await listen(async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    wire.push({ method: req.method, path: req.url, rawHeaders: req.rawHeaders, bytes: Buffer.concat(chunks) });
    const status = req.url.includes("503") ? 503 : 200;
    res.writeHead(status, { "content-type": "application/json" });
    res.end(status === 200 ? '{"access_token":"msb-openai-placeholder-a-v2"}' : '{"error":"temporarily_unavailable","message":"HOOK-BODY-CANARY"}');
  });
  return {
    targets, wire, source, refresh,
    async close() {
      await Promise.all([target, source, refresh].map(({ server }) => new Promise((resolve) => { server.close(resolve); server.closeAllConnections(); })));
    },
  };
}
function scriptedFetch(steps, hosts) {
  const calls = [], native = [];
  const fetch = async (input, init) => {
    const url = String(input);
    const kind = url === "https://auth.openai.com/oauth/token" ? "refresh" : "infer";
    calls.push({ kind, url, method: init.method, headers: Object.fromEntries(new Headers(init.headers)), bodyBytes: Buffer.from(init.body ?? ""), signal: init.signal, redirect: init.redirect });
    const step = steps.shift();
    assert.ok(step, "unexpected network send");
    assert.equal(kind, step.kind);
    if (step.run) return step.run(init);
    if (step.throw) throw new TypeError("fetch failed");
    if (step.native) {
      const destination = step.native === "refresh" ? `${hosts.refresh.url}/oauth/token${step.status === 503 ? "?503" : ""}` : `${hosts.source.url}/${step.native}`;
      const tracked = trackNative(await globalThis.fetch(destination, init));
      native.push(tracked.state);
      return tracked.response;
    }
    return step.response;
  };
  return { calls, native, fetch, kinds: () => calls.map((call) => call.kind) };
}
const infer = (response = new TrackedResponse(200, textSse())) => ({ kind: "infer", response });
const refresh = (response = new TrackedResponse(200, "{}")) => ({ kind: "refresh", response });
const unauthorized = () => new TrackedResponse(401, "inference unauthorized");
const text = (result) => result.content.filter((part) => part.type === "text").map((part) => part.text).join("");
function success(result) { assert.equal(result.stopReason, "stop"); assert.equal(text(result), "ok"); }
function failure(result, expected) {
  assert.equal(result.stopReason, "error");
  assert.equal(result.errorMessage, expected);
  assert.equal(isRetryableAssistantError(result), false);
}
function disposed(response, disposition) {
  assert.equal(response.drained, disposition === "drained", "drained disposition");
  assert.equal(response.cancelled, disposition === "cancelled", "cancelled disposition");
}
async function runStream(registry, model, ctx, options) {
  const stream = registry.streamSimple(model, ctx, options);
  const events = [];
  for await (const event of stream) events.push(event);
  const result = await stream.result();
  assert.equal(events.filter((e) => e.type === "done" || e.type === "error").length, 1, "one terminal event required");
  return result;
}

export default function (pi) {
  const codex = openAICodexResponsesApi();
  const mutantApi = "agent-vm-codex-oracle-mutant-api";
  pi.registerProvider("agent-vm-codex-oracle-mutant", {
    baseUrl: "https://chatgpt.com/backend-api", apiKey: jwt("acct-~~~?"), api: mutantApi,
    models: getBuiltinModels("openai-codex").map((model) => ({ ...model, api: mutantApi })),
    streamSimple: (model, ctx, options) => codex.streamSimple(model, ctx, { ...options, transport: "sse" }),
  });
  pi.on("session_start", async (_event, ctx) => {
    const cases = [];
    const add = (name, test) => cases.push({ name, test });
    const model = ctx.modelRegistry.find("agent-vm-codex", "gpt-6.1-sol");
    let hosts;
    try {
      assert.ok(model, "registered model missing");
      hosts = await servers();
      const run = (script, options = {}, chosen = model, messages = context) => runStream(ctx.modelRegistry, chosen, messages, {
        maxRetries: 0, fetch: script.fetch, ...options,
        signal: options.signal ? AbortSignal.any([options.signal, AbortSignal.timeout(20000)]) : AbortSignal.timeout(20000),
      });
      const script = (steps) => scriptedFetch(steps, hosts);
      add("headers-and-account", async () => {
        assert.match(b64({ "https://api.openai.com/auth": { chatgpt_account_id: "acct-~~~?" } }), /[+/]/);
        const s = script([infer()]); success(await run(s));
        assert.deepEqual(s.kinds(), ["infer"]);
        assert.equal(s.calls[0].headers.authorization, ACCESS);
        assert.equal(s.calls[0].headers["chatgpt-account-id"], "acct-~~~?");
        assert.ok(Object.values(s.calls[0].headers).every((v) => !v.includes(".agent-vm-unsigned")));
      });
      add("guest-apikey-not-forwarded", async () => {
        const s = script([infer()]); success(await run(s, { apiKey: jwt("acct-canary", "GUEST-TOKEN-CANARY") }));
        assert.deepEqual(s.kinds(), ["infer"]);
        assert.equal(s.calls[0].headers.authorization, ACCESS);
        assert.ok(!JSON.stringify(s.calls[0].headers).includes("GUEST-TOKEN-CANARY"));
        assert.ok(!s.calls[0].bodyBytes.includes("GUEST-TOKEN-CANARY"));
      });
      add("transport-forced-sse", async () => {
        const sockets = countingWebSocket();
        try {
          const s = script([infer()]); success(await run(s, { transport: "websocket", sessionId: `sse-${Date.now()}` }));
          assert.equal(sockets.count, 0); assert.deepEqual(s.kinds(), ["infer"]);
        } finally { sockets.restore(); }
      });
      add("foreign-host-refused", async () => {
        for (const baseUrl of ["https://example.invalid/backend-api", "http://chatgpt.com/backend-api", "https://chatgpt.com:8443/backend-api"]) {
          const s = script([]); failure(await run(s, {}, { ...model, baseUrl }), HOST_REFUSED); assert.equal(s.calls.length, 0);
        }
      });
      for (const name of ["infer-redirect-refused", "refresh-redirect-refused", "resend-redirect-refused-n0", "resend-redirect-refused-n2"]) {
        add(name, async () => {
          const before = hosts.targets.length;
          const first = unauthorized();
          const isRefresh = name === "refresh-redirect-refused";
          const isResend = name.startsWith("resend");
          const steps = isRefresh ? [infer(first), { kind: "refresh", native: "307" }]
            : isResend ? [infer(first), refresh(), { kind: "infer", native: "302" }]
            : [{ kind: "infer", native: "302" }];
          const s = script(steps);
          const result = await run(s, { maxRetries: name.endsWith("n2") ? 2 : 0 });
          assert.equal(hosts.targets.length, before, "redirect target must receive zero requests");
          failure(result, isRefresh ? REFRESH_FAILED : REDIRECT_REFUSED);
          assert.deepEqual(s.kinds(), isRefresh ? ["infer", "refresh"] : isResend ? ["infer", "refresh", "infer"] : ["infer"]);
          disposed(s.native[0], isRefresh ? "drained" : "cancelled");
        });
      }
      add("refresh-then-success", async () => {
        const first = unauthorized(), statuses = [];
        const before = hosts.wire.length;
        const s = script([infer(first), { kind: "refresh", native: "refresh" }, infer()]);
        success(await run(s, { onResponse: (r) => statuses.push(r.status) }));
        assert.deepEqual(s.kinds(), ["infer", "refresh", "infer"]);
        disposed(first, "cancelled"); disposed(s.native[0], "drained");
        assert.deepEqual(s.calls[0].bodyBytes, s.calls[2].bodyBytes);
        assert.equal(s.calls[0].signal, s.calls[2].signal);
        assert.deepEqual(statuses, [200]);
        assert.equal(hosts.wire.length, before + 1);
        const wire = hosts.wire.at(-1), raw = wire.rawHeaders;
        const values = (name) => raw.filter((_v, i) => i % 2 === 1 && raw[i - 1].toLowerCase() === name);
        assert.equal(wire.method, "POST"); assert.equal(wire.path, "/oauth/token");
        assert.deepEqual(values("content-type"), ["application/json"]);
        assert.deepEqual(values("content-length"), [String(wire.bytes.length)]);
        for (const header of ["transfer-encoding", "content-encoding", "authorization", "chatgpt-account-id"]) assert.deepEqual(values(header), []);
        assert.deepEqual(JSON.parse(wire.bytes.toString()), GRANT);
      });
      for (const n of [0, 2]) {
        add(`infer-5xx-untouched-n${n}`, async () => {
          const statuses = [], s = script([infer(new TrackedResponse(503, "sentinel-unavailable")), infer()]);
          const result = await run(s, { maxRetries: n, onResponse: (r) => statuses.push(r.status) });
          if (n) { success(result); assert.deepEqual(s.kinds(), ["infer", "infer"]); assert.deepEqual(statuses, [503, 200]); }
          else { assert.equal(result.stopReason, "error"); assert.match(result.errorMessage, /sentinel-unavailable/); assert.deepEqual(s.kinds(), ["infer"]); assert.deepEqual(statuses, [503]); }
        });
        add(`refresh-503-n${n}`, async () => {
          const s = script([infer(unauthorized()), { kind: "refresh", native: "refresh", status: 503 }]);
          failure(await run(s, { maxRetries: n }), REFRESH_FAILED);
          assert.deepEqual(s.kinds(), ["infer", "refresh"]); disposed(s.native[0], "drained");
        });
        add(`refresh-throws-n${n}`, async () => {
          const s = script([infer(unauthorized()), { kind: "refresh", throw: true }]);
          failure(await run(s, { maxRetries: n }), REFRESH_FAILED); assert.deepEqual(s.kinds(), ["infer", "refresh"]);
        });
        add(`second-401-n${n}`, async () => {
          const first = unauthorized(), later = Array.from({ length: n + 1 }, unauthorized), statuses = [];
          const s = script([infer(first), refresh(), ...later.map(infer)]);
          const result = await run(s, { maxRetries: n, onResponse: (r) => statuses.push(r.status) });
          assert.equal(result.stopReason, "error"); assert.equal(result.errorMessage, "inference unauthorized");
          assert.deepEqual(s.kinds(), ["infer", "refresh", ...later.map(() => "infer")]);
          disposed(first, "cancelled"); assert.ok(later.every((r) => r.bodyUsed));
          assert.deepEqual(statuses, later.map(() => 401));
        });
      }
      add("refresh-403-n0", async () => {
        const r = new TrackedResponse(403, "HOOK-BODY-CANARY"), s = script([infer(unauthorized()), refresh(r)]);
        failure(await run(s), REFRESH_FAILED); assert.deepEqual(s.kinds(), ["infer", "refresh"]); disposed(r, "drained");
      });
      add("refresh-body-error-n0", async () => {
        const r = new TrackedResponse(200, "partial", { bodyError: true }), s = script([infer(unauthorized()), refresh(r)]);
        failure(await run(s), REFRESH_FAILED); assert.deepEqual(s.kinds(), ["infer", "refresh"]); disposed(r, "neither");
      });
      add("malformed-200", async () => {
        const s = script([infer(new Response("not a stream", { headers: { "content-type": "application/json" } }))]);
        const result = await run(s); assert.equal(result.stopReason, "error"); assert.deepEqual(s.kinds(), ["infer"]);
      });
      add("non-401-untouched", async () => {
        const statuses = [], s = script([infer(new TrackedResponse(403, "sentinel-forbidden"))]);
        const result = await run(s, { onResponse: (r) => statuses.push(r.status) });
        assert.equal(result.stopReason, "error"); assert.match(result.errorMessage, /sentinel-forbidden/);
        assert.deepEqual(s.kinds(), ["infer"]); assert.deepEqual(statuses, [403]);
      });
      add("abort-during-refresh", async () => {
        const controller = new AbortController(), first = unauthorized();
        const s = script([infer(first), { kind: "refresh", run: (init) => new Promise((_resolve, reject) => {
          init.signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true }); controller.abort();
        }) }]);
        assert.equal((await run(s, { signal: controller.signal })).stopReason, "aborted");
        assert.deepEqual(s.kinds(), ["infer", "refresh"]); disposed(first, "cancelled");
      });
      add("abort-after-refresh-before-resend", async () => {
        const controller = new AbortController(), r = new TrackedResponse(200, "{}", { afterDrain: () => controller.abort() });
        const s = script([infer(unauthorized()), refresh(r)]);
        assert.equal((await run(s, { signal: controller.signal })).stopReason, "aborted");
        assert.deepEqual(s.kinds(), ["infer", "refresh"]); disposed(r, "drained");
      });
      async function replay(chosen, mutant) {
        assert.ok(chosen);
        const s1 = script([infer(new TrackedResponse(200, toolSse()))]);
        const turn1 = await run(s1, {}, chosen, { messages: [user], tools: [tool] });
        assert.equal(turn1.stopReason, "toolUse");
        assert.equal(turn1.provider, chosen.provider); assert.equal(turn1.api, "openai-codex-responses");
        const signature = JSON.parse(turn1.content.find((part) => part.type === "thinking").thinkingSignature);
        assert.equal(signature.id, "rs_test"); assert.equal(signature.encrypted_content, "encrypted-test");
        assert.equal(turn1.content.find((part) => part.type === "toolCall").id, "call_test|fc_test");
        let payload;
        const messages = [user, turn1, { role: "toolResult", toolCallId: "call_test|fc_test", toolName: "read", content: [{ type: "text", text: "t" }], isError: false, timestamp: 2 }];
        const s2 = script([infer()]); success(await run(s2, { onPayload: (p) => { payload = p; } }, chosen, { messages, tools: [tool] }));
        assert.deepEqual(s1.kinds(), ["infer"]); assert.deepEqual(s2.kinds(), ["infer"]);
        const fn = payload.input.find((item) => item.type === "function_call");
        const out = payload.input.find((item) => item.type === "function_call_output");
        assert.ok(fn, "function call must survive"); assert.ok(out, "tool result must survive"); assert.equal(out.output, "t");
        if (mutant) {
          assert.ok(!payload.input.some((item) => item.type === "reasoning" && item.encrypted_content === "encrypted-test"));
          assert.notEqual(fn.id, "fc_test"); assert.equal(fn.call_id, "call_test_fc_test"); assert.equal(out.call_id, "call_test_fc_test");
        } else {
          const item = payload.input.find((item) => item.type === "reasoning");
          assert.deepEqual({
            reasoningId: item?.id, encryptedContent: item?.encrypted_content,
            callId: fn.call_id, itemId: fn.id, outputCallId: out.call_id,
          }, {
            reasoningId: "rs_test", encryptedContent: "encrypted-test",
            callId: "call_test", itemId: "fc_test", outputCallId: "call_test",
          }, "encrypted reasoning and function-call item identity must survive replay");
        }
      }
      add("replay-identity", () => replay(model, false));
      add("replay-mutant-control", () => replay(ctx.modelRegistry.find("agent-vm-codex-oracle-mutant", "gpt-6.1-sol"), true));
      for (const { name, test } of cases) {
        try { await test(); ctx.ui.notify(`AGENT-VM-CODEX-CASE PASS ${name}`, "warning"); }
        catch (error) { ctx.ui.notify(`AGENT-VM-CODEX-CASE FAIL ${name}: ${error.message}`, "warning"); }
      }
      ctx.ui.notify(`AGENT-VM-CODEX-ORACLE DONE cases=${cases.length}`, "warning");
    } finally { if (hosts) await hosts.close(); }
  });
}
