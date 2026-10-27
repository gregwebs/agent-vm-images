#!/usr/bin/env python3
"""Drive a real editor-facing LSP session against a command that speaks LSP.

The go-dev example promises a working language server, so its native harness
asks for what an editor asks for: initialize, a definition, a hover, a
diagnostic after an edit, a formatting result, then shutdown. Everything here is
JSON-RPC with ``Content-Length`` framing over the child's stdin/stdout.

Usage: lsp-smoke.py [--project DIR] [--log FILE] [--deadline SECONDS] -- CMD...

The command is the whole transport: for the image it is a ``docker run -i`` that
executes gopls. The fixture project is read locally for the file contents and the
expected positions, so the expectations follow the fixture instead of hard-coded
line numbers.

Every requirement has a named reason, and a deadline of its own: a server that
hangs, answers something else, or exits early is a labelled failure rather than a
timeout that looks like a slow machine. ``--self-test`` modes of
``fixtures/go-example/fake-gopls.py`` exist so those reasons are exercised
without a daemon.
"""
from __future__ import annotations

import argparse
import json
import queue
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any, Callable

CLOSE_REASON = 'the session closed before answering'
DEFAULT_DEADLINE = 60.0


class Failure(Exception):
    """A named reason the session did not meet the editor contract."""


class Client:
    def __init__(self, command: list[str], deadline: float, transcript: Path | None):
        self.deadline = deadline
        self.next_id = 0
        self.messages: queue.Queue = queue.Queue()
        self.transcript = transcript.open('w', encoding='utf-8') if transcript else None
        stderr = (transcript.with_suffix('.server.log').open('wb')
                  if transcript is not None else subprocess.DEVNULL)
        self.process = subprocess.Popen(command, stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=stderr)
        self.reader = threading.Thread(target=self._read, daemon=True)
        self.reader.start()

    # --- transport ---------------------------------------------------------
    def _record(self, direction: str, payload: Any) -> None:
        if self.transcript is not None:
            self.transcript.write(f'{direction} {json.dumps(payload)}\n')
            self.transcript.flush()

    def _read(self) -> None:
        stream = self.process.stdout
        assert stream is not None
        while True:
            length: int | None = None
            while True:
                line = stream.readline()
                if not line:
                    self.messages.put(None)
                    return
                stripped = line.strip()
                if not stripped:
                    break
                name, _, value = stripped.partition(b':')
                if name.strip().lower() == b'content-length':
                    try:
                        length = int(value.strip())
                    except ValueError:
                        self.messages.put(None)
                        return
            if length is None:
                self.messages.put(None)
                return
            body = stream.read(length)
            if not body or len(body) != length:
                self.messages.put(None)
                return
            try:
                message = json.loads(body)
            except ValueError:
                self.messages.put(None)
                return
            self._record('<-', message)
            self.messages.put(message)

    def _send(self, payload: dict) -> None:
        body = json.dumps(payload).encode('utf-8')
        stdin = self.process.stdin
        assert stdin is not None
        self._record('->', payload)
        stdin.write(b'Content-Length: %d\r\n\r\n' % len(body) + body)
        stdin.flush()

    def notify(self, method: str, params: dict | None = None) -> None:
        payload: dict[str, Any] = {'jsonrpc': '2.0', 'method': method}
        if params is not None:
            payload['params'] = params
        self._send(payload)

    def request(self, method: str, params: dict, what: str) -> Any:
        self.next_id += 1
        identifier = self.next_id
        self._send({'jsonrpc': '2.0', 'id': identifier, 'method': method, 'params': params})
        reply = self.await_message(
            lambda message: message.get('id') == identifier and 'method' not in message, what)
        if 'error' in reply:
            raise Failure(f'{what}: the server returned {reply["error"]!r}')
        return reply.get('result')

    def _answer(self, message: dict) -> None:
        method = message['method']
        result: Any = None
        if method == 'workspace/configuration':
            result = [None for _ in message.get('params', {}).get('items', [])]
        elif method == 'workspace/workspaceFolders':
            result = [{'uri': WORKSPACE_URI, 'name': 'work'}]
        self._send({'jsonrpc': '2.0', 'id': message['id'], 'result': result})

    def await_message(self, predicate: Callable[[dict], bool], what: str) -> dict:
        deadline = time.monotonic() + self.deadline
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise Failure(f'{what}: no answer within {self.deadline:g}s')
            try:
                message = self.messages.get(timeout=remaining)
            except queue.Empty:
                raise Failure(f'{what}: no answer within {self.deadline:g}s') from None
            if message is None:
                raise Failure(f'{what}: {CLOSE_REASON} (server exit {self.process.poll()})')
            if 'method' in message and 'id' in message:
                self._answer(message)
                continue
            if predicate(message):
                return message

    # --- session -----------------------------------------------------------
    def close(self) -> int:
        if self.process.stdin is not None:
            try:
                self.process.stdin.close()
            except OSError:
                pass
        try:
            return self.process.wait(timeout=self.deadline)
        except subprocess.TimeoutExpired:
            self.process.kill()
            return self.process.wait()


WORKSPACE_URI = 'file:///work'

# What `gofmt` produces for the misindented fixture below, written out here rather
# than derived from whatever the server returns: an edit list can be structurally
# well-formed -- every entry a dict with a ``newText`` -- and still destroy the
# document. The oracle is the resulting source, so an edit that returns the right
# *shape* with the wrong (or empty) content fails the session.
EXPECTED_FORMATTED = (
    'package main\n'
    '\n'
    'import "fmt"\n'
    '\n'
    'func main() {\n'
    '\tfmt.Println(greeting())\n'
    '}\n'
)


def flattened(value: Any) -> str:
    if isinstance(value, str):
        return value
    if isinstance(value, dict):
        return ' '.join(flattened(item) for item in value.values())
    if isinstance(value, list):
        return ' '.join(flattened(item) for item in value)
    return ''


def offset_of(text: str, needle: str) -> tuple[int, int]:
    index = text.index(needle)
    line = text.count('\n', 0, index)
    character = index - (text.rfind('\n', 0, index) + 1)
    return line, character


def line_of(text: str, needle: str) -> int:
    for number, line in enumerate(text.splitlines()):
        if line.lstrip().startswith(needle):
            return number
    raise Failure(f'the fixture has no line starting with {needle!r}')


def definition_targets(result: Any) -> list[dict]:
    if result is None:
        return []
    if isinstance(result, dict):
        return [result]
    if isinstance(result, list):
        return [item for item in result if isinstance(item, dict)]
    return []


def apply_edits(text: str, edits: list[Any]) -> str:
    """Apply LSP ``TextEdit``s to ``text`` and return the resulting document.

    The fixture is ASCII, so one character is one code point. A malformed edit, a
    range outside the document, or a backwards range is a named failure: the point
    of applying the edits is that a well-formed list can still be destructive.
    """
    lines = text.splitlines(keepends=True)

    def offset(range_object: Any) -> int:
        if not isinstance(range_object, dict):
            raise Failure(f'formatting returned a malformed range: {range_object!r}')
        line = range_object.get('line')
        character = range_object.get('character')
        if not isinstance(line, int) or not isinstance(character, int):
            raise Failure(f'formatting returned a malformed position: {range_object!r}')
        if line < 0 or line > len(lines) or character < 0:
            raise Failure(f'formatting edit names line {line}, character {character}, '
                          'which is outside the document')
        if line == len(lines):
            if character != 0:
                raise Failure(f'formatting edit names character {character} past the '
                              'last line')
            return len(text)
        if character > len(lines[line]):
            raise Failure(f'formatting edit names character {character} past the end '
                          f'of line {line}')
        return sum(len(item) for item in lines[:line]) + character

    placed: list[tuple[int, int, str]] = []
    for edit in edits:
        if not isinstance(edit, dict):
            raise Failure(f'formatting returned a malformed edit: {edit!r}')
        new_text = edit.get('newText')
        if not isinstance(new_text, str):
            raise Failure(f'formatting returned an edit without text: {edit!r}')
        range_object = edit.get('range')
        if not isinstance(range_object, dict):
            raise Failure(f'formatting returned an edit without a range: {edit!r}')
        start = offset(range_object.get('start'))
        end = offset(range_object.get('end'))
        if end < start:
            raise Failure(f'formatting returned a backwards range: {edit!r}')
        placed.append((start, end, new_text))

    result = text
    for start, end, new_text in sorted(placed, reverse=True):
        result = result[:start] + new_text + result[end:]
    return result


def run_session(client: Client, project: Path) -> int:
    main_text = (project / 'main.go').read_text(encoding='utf-8')
    greet_text = (project / 'greet.go').read_text(encoding='utf-8')
    call_line, call_character = offset_of(main_text, 'greeting()')
    definition_line = line_of(greet_text, 'func greeting(')
    broken_text = main_text + '\nvar _ int = "x"\n'
    misindented = 'package main\n\nimport "fmt"\n\nfunc main() {\nfmt.Println(greeting())\n}\n'

    client.request('initialize', {
        'processId': None,
        'clientInfo': {'name': 'agent-vm-go-example'},
        'rootUri': WORKSPACE_URI,
        'workspaceFolders': [{'uri': WORKSPACE_URI, 'name': 'work'}],
        'capabilities': {
            'workspace': {'configuration': True, 'workspaceFolders': True},
            'textDocument': {
                'definition': {'linkSupport': True},
                'hover': {'contentFormat': ['markdown', 'plaintext']},
                'formatting': {},
                'publishDiagnostics': {'relatedInformation': True},
                'synchronization': {'didSave': False},
            },
            'window': {'workDoneProgress': True},
        },
    }, 'initialize')
    print('lsp-smoke: ok   initialize')
    client.notify('initialized', {})

    uri = f'{WORKSPACE_URI}/main.go'
    client.notify('textDocument/didOpen', {'textDocument': {
        'uri': uri, 'languageId': 'go', 'version': 1, 'text': main_text}})

    position = {'line': call_line, 'character': call_character}
    targets = definition_targets(client.request(
        'textDocument/definition',
        {'textDocument': {'uri': uri}, 'position': position}, 'definition'))
    if not targets:
        raise Failure('definition: the server returned no location')
    if not str(targets[0].get('uri', '')).endswith('/greet.go'):
        raise Failure(f'definition does not name greet.go: {targets[0].get("uri")!r}')
    start_line = targets[0].get('range', {}).get('start', {}).get('line')
    if start_line != definition_line:
        raise Failure(f'definition points at greet.go line {start_line}, '
                      f'not {definition_line}')
    print(f'lsp-smoke: ok   definition resolves to greet.go:{definition_line + 1}')

    hover = flattened(client.request(
        'textDocument/hover',
        {'textDocument': {'uri': uri}, 'position': position}, 'hover'))
    if 'func greeting() string' not in ' '.join(hover.split()):
        raise Failure(f'hover does not describe the function: {hover[:120]!r}')
    print('lsp-smoke: ok   hover describes func greeting() string')

    client.notify('textDocument/didChange', {
        'textDocument': {'uri': uri, 'version': 2},
        'contentChanges': [{'text': broken_text}]})

    def reports_cannot_use(message: dict) -> bool:
        if message.get('method') != 'textDocument/publishDiagnostics':
            return False
        if message['params'].get('uri') != uri:
            return False
        return any('cannot use' in diagnostic.get('message', '')
                   for diagnostic in message['params'].get('diagnostics', []))

    client.await_message(reports_cannot_use, 'diagnostics for a type error')
    print('lsp-smoke: ok   a type error is published as a diagnostic')

    client.notify('textDocument/didChange', {
        'textDocument': {'uri': uri, 'version': 3},
        'contentChanges': [{'text': misindented}]})
    edits = client.request('textDocument/formatting', {
        'textDocument': {'uri': uri},
        'options': {'tabSize': 4, 'insertSpaces': False}}, 'formatting')
    if not isinstance(edits, list) or not edits:
        raise Failure(f'formatting produced no edits: {edits!r}')
    formatted = apply_edits(misindented, edits)
    if formatted != EXPECTED_FORMATTED:
        raise Failure('formatting does not produce the gofmt output; it produced '
                      f'{formatted!r}, not {EXPECTED_FORMATTED!r}')
    print(f'lsp-smoke: ok   formatting returns {len(edits)} edit(s) that produce the '
          'gofmt output')

    client.request('shutdown', {}, 'shutdown')
    client.notify('exit')
    status = client.close()
    if status != 0:
        raise Failure(f'the session exited {status}, not 0')
    print('lsp-smoke: ok   shutdown and exit')
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--project', default=str(Path(__file__).with_name('project')))
    parser.add_argument('--log')
    parser.add_argument('--deadline', type=float, default=DEFAULT_DEADLINE)
    parser.add_argument('command', nargs=argparse.REMAINDER)
    arguments = parser.parse_args(argv)
    command = arguments.command
    if command and command[0] == '--':
        command = command[1:]
    if not command:
        parser.error('a command is required after --')
    if arguments.deadline <= 0:
        parser.error('--deadline must be positive')
    client: Client | None = None
    try:
        client = Client(command, arguments.deadline,
                        Path(arguments.log) if arguments.log else None)
        return run_session(client, Path(arguments.project))
    except Failure as failure:
        print(f'lsp-smoke: FAIL {failure}', file=sys.stderr)
        if client is not None:
            client.close()
        return 1
    except OSError as error:
        print(f'lsp-smoke: FAIL cannot run the session: {error}', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
