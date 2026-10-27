#!/usr/bin/env python3
"""A deliberately unreliable LSP server for the go-example harness's self-test.

The native harness's editor leg is only evidence if it can distinguish a working
load from a broken one, and this fake is the daemon-free half of that proof: each
``--mode`` is one defect the client must name instead of passing.

    healthy            answers everything the way gopls does
    no-diagnostics     never publishes a diagnostic
    wrong-definition   resolves the call to main.go instead of greet.go
    corrupt-formatting returns a well-formed edit list that empties the file
    hang               accepts the connection and never answers
    crash              exits before the session starts

Usage: fake-gopls.py --mode MODE [--project DIR]
"""
from __future__ import annotations

import argparse
import json
import sys
import time
from pathlib import Path


def read_message(stream) -> dict | None:
    length: int | None = None
    while True:
        line = stream.readline()
        if not line:
            return None
        stripped = line.strip()
        if not stripped:
            break
        name, _, value = stripped.partition(b':')
        if name.strip().lower() == b'content-length':
            length = int(value.strip())
    if length is None:
        return None
    body = stream.read(length)
    if not body or len(body) != length:
        return None
    return json.loads(body)


def write_message(stream, payload: dict) -> None:
    body = json.dumps(payload).encode('utf-8')
    stream.write(b'Content-Length: %d\r\n\r\n' % len(body) + body)
    stream.flush()


def line_of(text: str, needle: str) -> int:
    for number, line in enumerate(text.splitlines()):
        if line.lstrip().startswith(needle):
            return number
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--mode', required=True,
                        choices=['healthy', 'no-diagnostics', 'wrong-definition',
                                 'corrupt-formatting', 'hang', 'crash'])
    parser.add_argument('--project', default=str(Path(__file__).with_name('project')))
    arguments = parser.parse_args(argv)
    stdin = sys.stdin.buffer
    stdout = sys.stdout.buffer
    if arguments.mode == 'crash':
        print('fake-gopls: crashing on purpose', file=sys.stderr)
        return 1

    greet = Path(arguments.project) / 'greet.go'
    definition_line = line_of(greet.read_text(encoding='utf-8'), 'func greeting(')

    while True:
        message = read_message(stdin)
        if message is None:
            return 0
        method = message.get('method')
        if method is None:
            continue
        if method == 'exit':
            return 0
        if 'id' not in message:
            if method == 'textDocument/didChange':
                text = message['params']['contentChanges'][0]['text']
                uri = message['params']['textDocument']['uri']
                if 'var _ int' in text and arguments.mode != 'no-diagnostics':
                    write_message(stdout, {
                        'jsonrpc': '2.0',
                        'method': 'textDocument/publishDiagnostics',
                        'params': {
                            'uri': uri,
                            'diagnostics': [{
                                'range': {'start': {'line': 7, 'character': 0},
                                          'end': {'line': 7, 'character': 16}},
                                'severity': 1,
                                'source': 'fake-gopls',
                                'message': 'cannot use "x" (untyped string constant) '
                                           'as int value in variable declaration',
                            }],
                        },
                    })
            continue
        if arguments.mode == 'hang':
            time.sleep(3600)
        result = None
        if method == 'initialize':
            result = {'capabilities': {
                'textDocumentSync': 2,
                'definitionProvider': True,
                'hoverProvider': True,
                'documentFormattingProvider': True,
            }}
        elif method == 'textDocument/definition':
            target = 'main.go' if arguments.mode == 'wrong-definition' else 'greet.go'
            line = 0 if target == 'main.go' else definition_line
            result = {'uri': f'file:///work/{target}',
                      'range': {'start': {'line': line, 'character': 0},
                                'end': {'line': line, 'character': 20}}}
        elif method == 'textDocument/hover':
            result = {'contents': {'kind': 'markdown',
                                   'value': '```go\nfunc greeting() string\n```'}}
        elif method == 'textDocument/formatting':
            # The corrupt mode keeps the edit list's shape (a dict with a range and
            # a `newText`) and only empties the content, which is exactly what a
            # shape-only assertion would accept.
            new_text = ('' if arguments.mode == 'corrupt-formatting'
                        else '\tfmt.Println(greeting())\n')
            result = [{'range': {'start': {'line': 5, 'character': 0},
                                 'end': {'line': 6, 'character': 0}},
                       'newText': new_text}]
        write_message(stdout, {'jsonrpc': '2.0', 'id': message['id'], 'result': result})


if __name__ == '__main__':
    sys.exit(main())
