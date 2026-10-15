// Synthetic plugin API: tests verifier plumbing, not the reviewed upstream code.
import { resolveClaudeCodeExecutable } from './executable-path.js';
export async function probeClaudeAgentSdk() {
  await import('@anthropic-ai/claude-agent-sdk');
  return { available: true };
}
export async function startClaudeQuery(params) {
  const env = { ...params.env };
  for (const key of ['ANTHROPIC_API_KEY', 'ANTHROPIC_AUTH_TOKEN', 'CLAUDE_CODE_OAUTH_TOKEN']) delete env[key];
  params.queryImpl({})({ options: { env, cwd: params.cwd, pathToClaudeCodeExecutable: await resolveClaudeCodeExecutable() } });
  return { close() {} };
}
export async function listClaudeSupportedModels() { return process.env.TEST_EMPTY_MODELS ? [] : ['fixture']; }
