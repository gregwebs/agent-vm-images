// Dependency-free runtime certification; build-only lock parsing is separate.
import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { pathToFileURL } from "node:url";
import assert from "node:assert/strict";

// The owning gate supplies fresh HOME/XDG; do not let an alternate login store
// or authority override escape that isolation during real CLI initialization.
for (const key of Object.keys(process.env)) {
  if (/^(CLAUDE_|ANTHROPIC_|AWS_|GOOGLE_|AZURE_|OPENCODE_)/.test(key) || /_(API_KEY|AUTH_TOKEN|OAUTH_TOKEN)$/.test(key)) delete process.env[key];
}
process.env.CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC = "1";

async function check(prefixArg, claudeArg) {
  const prefix = fs.realpathSync(prefixArg);
  const inside = file => {
    const resolved = fs.realpathSync(file);
    assert.ok(resolved.startsWith(`${prefix}${path.sep}`), `outside owned prefix: ${file}`);
    return resolved;
  };
  const manifests = new Map();
  const visited = new Set();
  function walk(file, chain = new Set(), ancestors = new Set()) {
    const stat = fs.lstatSync(file);
    if (stat.isSymbolicLink()) {
      assert.ok(!chain.has(file), `cyclic link: ${file}`);
      const next = new Set(chain).add(file);
      const target = path.resolve(path.dirname(file), fs.readlinkSync(file));
      inside(target);
      walk(target, next, ancestors);
      return;
    }
    const real = inside(file);
    assert.ok(!ancestors.has(real), `cyclic directory link: ${file}`);
    if (visited.has(real)) return;
    visited.add(real);
    if (stat.isDirectory()) {
      const manifest = path.join(file, "package.json");
      if (fs.existsSync(manifest) && fs.statSync(manifest).isFile()) {
        const metadata = JSON.parse(fs.readFileSync(manifest, "utf8"));
        if (metadata.name) manifests.set(real, metadata);
      }
      const nextAncestors = new Set(ancestors).add(real);
      for (const child of fs.readdirSync(file)) walk(path.join(file, child), chain, nextAncestors);
    }
  }
  // Check physical virtual-store directories as well as logical package links.
  walk(path.join(prefix, "node_modules"));
  const root = createRequire(path.join(prefix, "index.js"));
  const pluginEntry = inside(root.resolve("@openchamber/opencode-claude"));
  const pluginDirectory = packageDirectory(pluginEntry, "@openchamber/opencode-claude");
  const queryEntry = inside(path.join(pluginDirectory, "dist/query.js"));
  const fromPlugin = createRequire(queryEntry);
  const sdkEntry = inside(fromPlugin.resolve("@anthropic-ai/claude-agent-sdk"));
  assert.equal(sdkEntry, inside(root.resolve("@anthropic-ai/claude-agent-sdk")));
  function packageDirectory(entry, expected) {
    let directory = path.dirname(entry);
    while (directory.startsWith(prefix + path.sep)) {
      const file = path.join(directory, "package.json");
      if (fs.existsSync(file) && JSON.parse(fs.readFileSync(file, "utf8")).name === expected) return fs.realpathSync(directory);
      directory = path.dirname(directory);
    }
    throw new Error(`package metadata unavailable: ${expected}`);
  }
  const sdkDirectory = packageDirectory(sdkEntry, "@anthropic-ai/claude-agent-sdk");
  assert.equal(sdkDirectory, packageDirectory(inside(root.resolve("@anthropic-ai/claude-agent-sdk")), "@anthropic-ai/claude-agent-sdk"));
  const sdkMetadata = JSON.parse(fs.readFileSync(path.join(sdkDirectory, "package.json"), "utf8"));
  assert.equal(sdkMetadata.version, "0.3.281");
  const manifest = JSON.parse(fs.readFileSync(path.join(prefix, "package.json"), "utf8"));
  const pluginMetadata = JSON.parse(fs.readFileSync(path.join(pluginDirectory, "package.json"), "utf8"));
  assert.equal(pluginMetadata.version, manifest.dependencies["@openchamber/opencode-claude"]);
  const sdkInstances = [...manifests.entries()].filter(([, m]) => m.name === "@anthropic-ai/claude-agent-sdk");
  assert.equal(sdkInstances.length, 1, "exactly one physical SDK instance required");
  assert.equal(sdkInstances[0][0], sdkDirectory);
  const installedNames = new Set([...manifests.values()].map(m => m.name));
  for (const name of installedNames) {
    assert.ok(!name.startsWith("@anthropic-ai/claude-agent-sdk-"), `optional platform installed: ${name}`);
    assert.ok(!["@anthropic-ai/sdk", "@modelcontextprotocol/sdk", "@opencode/plugin", "@opencode-ai/plugin", "@opencode/cli", "opencode-ai"].includes(name), `unwanted host/peer installed: ${name}`);
  }
  assert.deepEqual([...installedNames].sort(), ["@anthropic-ai/claude-agent-sdk", "@openchamber/opencode-claude", "zod"].sort());
  for (const missing of ["@anthropic-ai/sdk", "@modelcontextprotocol/sdk"]) {
    for (const context of [root, fromPlugin, createRequire(sdkEntry)]) {
      assert.throws(() => context.resolve(missing), { code: "MODULE_NOT_FOUND" });
    }
  }
  const zodEntry = inside(fromPlugin.resolve("zod"));
  assert.equal(zodEntry, inside(createRequire(sdkEntry).resolve("zod")));
  assert.equal(JSON.parse(fs.readFileSync(path.join(packageDirectory(zodEntry, "zod"), "package.json"))).version, "4.6.7");
  const query = await import(pathToFileURL(queryEntry));
  assert.equal((await query.probeClaudeAgentSdk()).available, true);
  const executable = await import(pathToFileURL(path.join(pluginDirectory, "dist/executable-path.js")));
  const claude = fs.realpathSync(claudeArg);
  assert.equal(fs.realpathSync(await executable.resolveClaudeCodeExecutable()), claude);
  let handoff = false;
  const cwd = fs.realpathSync(process.cwd());
  const handle = await query.startClaudeQuery({
    prompt: "certification does not send this prompt",
    cwd,
    env: { ...process.env, ANTHROPIC_API_KEY: "fixture-api", ANTHROPIC_AUTH_TOKEN: "fixture-auth", CLAUDE_CODE_OAUTH_TOKEN: "fixture-oauth" },
    queryImpl: () => ({ options }) => {
      assert.equal(fs.realpathSync(options.pathToClaudeCodeExecutable), claude);
      assert.equal(fs.realpathSync(options.cwd), cwd);
      for (const key of ["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"]) assert.ok(!(key in options.env));
      handoff = true;
      return (async function* () {})();
    },
  });
  handle.close();
  assert.equal(handoff, true);
  const adapter = await import(pathToFileURL(path.join(prefix, "index.js")));
  assert.ok(adapter.default);
  // No prompt or authenticated turn: initialize the real SDK/CLI and ask its
  // supported-model control API. This must work with platform packages absent.
  // Completion is the offline initialization evidence; the count is recorded,
  // not required. Zero is tolerated because the build RUN has no guest
  // credentials, so the CLI may not enumerate models offline. The non-empty
  // requirement is discharged by the OpenCode probe's /api/model assertion
  // against the plugin's fallback catalog.
  const models = await query.listClaudeSupportedModels(12000);
  assert.ok(Array.isArray(models), "real SDK/CLI initialization did not return a model list");
  return {
    schema: 1, plugin_version: pluginMetadata.version, sdk_version: sdkMetadata.version,
    sdk_entry: sdkEntry, sdk_directory: sdkDirectory, sdk_instances: 1,
    claude_path: claude, handoff: true, sdk_available: true, adapter: true,
    omitted_peers: true, optional_platforms: false, initialization_models: models.length,
  };
}

try {
  if (process.argv.length !== 4) throw new Error("usage: check-claude-runtime.mjs PLUGIN_PREFIX EXPECTED_CLAUDE_PATH");
  console.log(JSON.stringify(await check(process.argv[2], process.argv[3])));
} catch (error) {
  console.error(`check-claude-runtime: ${error.message}`);
  process.exitCode = 1;
}
