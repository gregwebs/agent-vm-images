// Stdio MCP client for the Chrome example's runtime checks (#41).
//
// It is a client only: it speaks what agent-vm's launcher speaks and it owns the
// child's lifetime, so a failing check cannot leave a browser or a manager
// behind. Node standard library only, no dependencies: the image under test is
// the thing that carries package state, and this probe must be able to run where
// no package manager is allowed to.
import {spawn} from 'node:child_process';

const PROTOCOL_VERSION = '2025-06-18';
const DEADLINE_MS = 90_000;
const EXIT_WAIT_MS = 10_000;
const KILL_AFTER_MS = 5_000;
// The server frames messages as newline-delimited JSON and writes a lot of page
// text; 1 MiB is far above a real tools/list + snapshot and far below a hang.
const STDOUT_LIMIT_BYTES = 1024 * 1024;
const REQUIRED_TOOLS = ['new_page', 'evaluate_script'];
const PAGE_URL = 'data:text/html,<title>agent-vm-41</title><h1>offline-ok-41</h1>';
const PAGE_SCRIPT = "() => document.querySelector('h1').textContent";
const PAGE_TEXT = 'offline-ok-41';
const USAGE = 'usage: node mcp-probe.mjs --expect-version VERSION -- COMMAND [ARG ...]';

let child = null;
let exitPromise = null;
let exitInfo = null;
let deadline = null;
let finished = false;
let stdoutBuffer = '';
let stdoutBytes = 0;
let nextId = 0;
const pending = new Map();

function usage() {
    process.stderr.write(`${USAGE}\n`);
    process.exit(2);
}

function parseArgs(argv) {
    const separator = argv.indexOf('--');
    if (argv[0] !== '--expect-version' || separator !== 2 || !argv[1] || argv.length <= separator + 1) {
        usage();
    }
    return {expectVersion: argv[1], command: argv[separator + 1], args: argv.slice(separator + 2)};
}

// Report one plan-mandated line, then make sure the child is gone before the
// probe itself exits: an orphan would keep a browser alive inside the container
// and make the next check read someone else's state.
function finish(code, line, note) {
    if (finished) {
        return;
    }
    finished = true;
    clearTimeout(deadline);
    if (note) {
        process.stderr.write(`probe: ${note}\n`);
    }
    const stream = code === 0 ? process.stdout : process.stderr;
    stream.write(`${line}\n`, () => stopChild(code));
}

function fail(kind, detail, note) {
    finish(1, detail ? `PROBE FAIL ${kind} ${detail}` : `PROBE FAIL ${kind}`, note);
}

async function stopChild(code) {
    // Ending stdin is the protocol's shutdown request; the escalation below only
    // runs when the server ignores it.
    child.stdin.end();
    if (!exitInfo) {
        await waitForExit(EXIT_WAIT_MS);
    }
    if (!exitInfo) {
        child.kill('SIGTERM');
        await waitForExit(KILL_AFTER_MS);
    }
    if (!exitInfo) {
        child.kill('SIGKILL');
        await waitForExit(KILL_AFTER_MS);
    }
    process.exit(code);
}

function waitForExit(milliseconds) {
    return Promise.race([exitPromise, new Promise(resolve => setTimeout(resolve, milliseconds))]);
}

function send(message) {
    child.stdin.write(`${JSON.stringify(message)}\n`);
}

// Every request resolves with the result; every failure path terminates the
// probe, so a request promise never rejects.
function request(method, params, toolName) {
    nextId += 1;
    const id = nextId;
    return new Promise(resolve => {
        pending.set(id, {resolve, toolName});
        send({jsonrpc: '2.0', id, method, params});
    });
}

function onMessage(line) {
    let message;
    try {
        message = JSON.parse(line);
    } catch {
        return fail('protocol', null, `not JSON: ${line.slice(0, 120)}`);
    }
    if (message === null || typeof message !== 'object' || message.jsonrpc !== '2.0') {
        return fail('protocol', null, `not a JSON-RPC 2.0 message: ${line.slice(0, 120)}`);
    }
    if (message.id === undefined) {
        return undefined; // server-initiated notification (logging, progress)
    }
    const waiter = pending.get(message.id);
    if (waiter === undefined) {
        return fail('protocol', null, `response to unknown request id ${JSON.stringify(message.id)}`);
    }
    pending.delete(message.id);
    if (message.error !== undefined) {
        return waiter.toolName
            ? fail('tool', waiter.toolName, `error response: ${JSON.stringify(message.error).slice(0, 200)}`)
            : fail('protocol', null, `error response: ${JSON.stringify(message.error).slice(0, 200)}`);
    }
    waiter.resolve(message.result);
    return undefined;
}

function onStdout(chunk) {
    stdoutBytes += chunk.length;
    if (stdoutBytes > STDOUT_LIMIT_BYTES) {
        return fail('protocol', 'oversize', `more than ${STDOUT_LIMIT_BYTES} bytes on stdout`);
    }
    stdoutBuffer += chunk.toString('utf8');
    return flushStdout();
}

function flushStdout() {
    let index = stdoutBuffer.indexOf('\n');
    while (index >= 0) {
        const line = stdoutBuffer.slice(0, index);
        stdoutBuffer = stdoutBuffer.slice(index + 1);
        if (line.trim() !== '') {
            onMessage(line);
        }
        index = stdoutBuffer.indexOf('\n');
    }
    return undefined;
}

function startChild(command, args) {
    child = spawn(command, args, {stdio: ['pipe', 'pipe', 'inherit']});
    // A dead child would otherwise raise an unhandled 'error' event on the first
    // write, or on the stream's own error path.
    child.stdin.on('error', () => {});
    child.on('error', error => {
        exitInfo = {code: null, signal: null}; // nothing was started
        fail('spawn', null, error.message);
    });
    exitPromise = new Promise(resolve => {
        child.once('exit', (code, signal) => {
            exitInfo = {code, signal};
            // A server that prints garbage and then dies is a protocol failure,
            // not a silent exit: give already-queued stdout data a turn first.
            setImmediate(() => {
                flushStdout();
                if (!finished) {
                    fail('server-exit', `code=${code === null ? 'none' : code} signal=${signal === null ? 'none' : signal}`);
                }
            });
            resolve();
        });
    });
    child.stdout.on('data', onStdout);
    deadline = setTimeout(() => fail('timeout'), DEADLINE_MS);
}

function resultText(result) {
    const parts = [];
    for (const item of result?.content ?? []) {
        if (typeof item?.text === 'string') {
            parts.push(item.text);
        }
    }
    return parts.join('\n');
}

async function callTool(name, parameters) {
    const result = await request('tools/call', {name, arguments: parameters}, name);
    if (result?.isError === true) {
        return fail('tool', name, resultText(result).slice(0, 200));
    }
    return result;
}

async function run() {
    const {expectVersion, command, args} = parseArgs(process.argv.slice(2));
    startChild(command, args);

    const initialized = await request('initialize', {
        protocolVersion: PROTOCOL_VERSION,
        capabilities: {},
        clientInfo: {name: 'agent-vm-mcp-probe', version: '1'},
    });
    const server = initialized?.serverInfo;
    if (server?.name !== 'chrome_devtools') {
        return fail('protocol', null, `serverInfo.name=${JSON.stringify(server?.name)}`);
    }
    if (server?.version !== expectVersion) {
        return fail('protocol', null, `serverInfo.version=${JSON.stringify(server?.version)} expected ${expectVersion}`);
    }
    send({jsonrpc: '2.0', method: 'notifications/initialized'});

    const listed = await request('tools/list', {});
    const names = (listed?.tools ?? []).map(tool => tool?.name);
    for (const name of REQUIRED_TOOLS) {
        if (!names.includes(name)) {
            return fail('protocol', null, `tools/list lacks ${name}`);
        }
    }

    await callTool('new_page', {url: PAGE_URL});
    const evaluation = await callTool('evaluate_script', {function: PAGE_SCRIPT});
    const text = resultText(evaluation);
    if (!text.includes(PAGE_TEXT)) {
        return fail('tool', 'evaluate_script', `result does not contain ${PAGE_TEXT}: ${text.slice(0, 200)}`);
    }
    finish(0, `PROBE OK version=${server.version} tools=${names.length}`);
}

await run();
