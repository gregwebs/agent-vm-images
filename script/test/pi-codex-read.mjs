// Deterministic permitted short reads, without a production test hook or real credentials.
// Evaluate the unchanged factory with only its import dependencies substituted: Pi's
// virtual modules are unavailable on the host, and kernel short reads are not reliable.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const source = readFileSync(new URL("../../images/tools/pi/extensions/agent-vm-codex.js", import.meta.url), "utf8")
  .replace(/^import .*;$/gm, "")
  .replace("export default function (pi)", "globalThis.factory = function (pi)");
const valid = readFileSync(new URL("fixtures/pi-codex/placeholder.json", import.meta.url), "utf8").trim();

function probe(document, chunk, statSize = Buffer.byteLength(document)) {
  const data = Buffer.from(document);
  let registered = 0, reads = 0, closes = 0, nextPosition = 0;
  const calls = [];
  const descriptor = 42;
  const constants = { O_RDONLY: 0, O_NOFOLLOW: 1, O_NONBLOCK: 2 };
  const context = vm.createContext({
    Buffer, constants,
    openSync(path, flags) {
      calls.push(["open", path, flags]);
      return descriptor;
    },
    fstatSync(fd) {
      calls.push(["stat", fd]);
      return { isFile: () => true, size: statSize };
    },
    readSync(fd, buffer, offset, length, position) {
      calls.push(["read", fd, offset, length, position, nextPosition]);
      reads++;
      const count = Math.min(chunk, length, data.length - position);
      data.copy(buffer, offset, position, position + count);
      nextPosition += count;
      return count;
    },
    closeSync(fd) { calls.push(["close", fd]); closes++; },
    openAICodexResponsesApi: () => ({}),
    getBuiltinModels: () => [{}],
  });
  vm.runInContext(source, context);
  context.factory({ registerProvider(id) { assert.equal(id, "agent-vm-codex"); registered++; } });
  assert.deepEqual(calls[0], ["open", "/agent-vm-state/codex/auth.json", 3]);
  for (const [kind, fd, offset, length, position, expectedPosition] of calls.slice(1)) {
    assert.equal(fd, descriptor);
    if (kind === "read") {
      assert.equal(position, expectedPosition);
      assert.equal(offset, expectedPosition);
      assert.equal(length, 65537 - expectedPosition);
    }
  }
  assert.equal(closes, 1, "descriptor must close on success and rejection");
  return { registered, reads, bytes: nextPosition };
}

assert.equal(probe(valid, Infinity).registered, 1);
const short = probe(valid, 31);
assert.equal(short.registered, 1, "valid JSON must survive short reads");
assert.ok(short.reads > 2, "read through EOF, not just the first chunk");
const invalid = valid + " trailing garbage";
assert.equal(probe(invalid, Infinity).registered, 0);
const boundary = probe(invalid, Buffer.byteLength(valid));
assert.equal(boundary.registered, 0, "complete JSON prefix must not hide an invalid suffix");
assert.ok(boundary.reads > 1);
const grown = probe(valid + " ".repeat(65537), 31, Buffer.byteLength(valid));
assert.equal(grown.registered, 0, "growth after fstat must remain bounded");
assert.equal(grown.bytes, 65537);
console.log("pi-codex bounded short-read tests passed");
