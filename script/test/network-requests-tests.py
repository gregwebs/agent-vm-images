#!/usr/bin/env python3
"""The network-request label contract's own table (ADR 0014).

Every case here is a boundary the label writer can get wrong: a grammar edge,
a sort order that two plausible implementations disagree about, a Dockerfile
form that would put a key where the image never sees it, or an input that
should be a message instead of a traceback. The launcher-parity vectors are
copied from agent-vm's own tables rather than derived, so a drift between the
two grammars fails here instead of in the launcher.

usage: python3 script/test/network-requests-tests.py
"""
from __future__ import annotations

import json
import random
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[2]
MODULE = REPO_ROOT / 'script' / 'test' / 'network_requests.py'
sys.path.insert(0, str(MODULE.parent))

import network_requests as nr  # noqa: E402  (the path insert above is the import)


def run_cli(*arguments: str) -> subprocess.CompletedProcess:
    # No shell: every argument is one argv entry, and the cwd is the repository
    # so the CLI's default --root is the tree under test.
    return subprocess.run([sys.executable, str(MODULE), *arguments],
                          capture_output=True, text=True, cwd=str(REPO_ROOT))


class ScratchTree:
    """A throwaway repository root with just the directories under test."""

    def __init__(self) -> None:
        self._scratch = tempfile.TemporaryDirectory(prefix='network-requests-tests.')
        self.root = Path(self._scratch.name)

    def write(self, relative: str, text: str) -> Path:
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        return path

    def close(self) -> None:
        self._scratch.cleanup()


def recipe(*lines: str) -> str:
    return ''.join(line + '\n' for line in lines)


BASE_LABEL = 'LABEL org.agent-vm.network-requests.image.base=""'


class ScratchCase(unittest.TestCase):
    def setUp(self) -> None:
        self.tree = ScratchTree()
        self.addCleanup(self.tree.close)


class ValueGrammar(unittest.TestCase):
    def assert_accepted(self, raw: str) -> nr.RequestList:
        try:
            return nr.parse_value(raw)
        except nr.RequestError as error:
            self.fail(f'{raw!r} should be accepted but was refused: {error}')

    def assert_refused(self, raw: str, fragment: str = '') -> None:
        with self.assertRaises(nr.RequestError) as raised:
            nr.parse_value(raw)
        if fragment:
            self.assertIn(fragment, str(raised.exception))

    def test_accepted_values(self):
        for raw, expected in [
                ('', ''),
                ('crates.io,index.crates.io,static.crates.io',
                 'crates.io,index.crates.io,static.crates.io'),
                ('api.github.com,files.pythonhosted.org,github.com,pypi.org,'
                 'registry.npmjs.org',
                 'api.github.com,files.pythonhosted.org,github.com,pypi.org,'
                 'registry.npmjs.org'),
                ('h.example:8443', 'h.example:8443'),
                ('build', 'build'),
                ('xn--bcher-kva.example', 'xn--bcher-kva.example'),
                ('0x7f.example', '0x7f.example'),
                ('a--b.example', 'a--b.example'),
                ('h.example:1', 'h.example:1'),
                ('h.example:65535', 'h.example:65535'),
                ('h.example:52', 'h.example:52'),
                ('h.example:54', 'h.example:54'),
                ('h.example:852', 'h.example:852'),
                ('h.example:854', 'h.example:854')]:
            with self.subTest(raw=raw):
                self.assertEqual(str(self.assert_accepted(raw)), expected)

    def test_refused_values(self):
        for raw, fragment in [
                ('index.crates.io,crates.io', 'not in canonical order'),
                ('crates.io, index.crates.io', 'whitespace'),
                ('crates.io,\tindex.crates.io', 'whitespace'),
                ('crates.o\u00a0', 'whitespace'),
                ('crates.io,', 'empty'),
                (',crates.io', 'empty'),
                ('a,,b', 'empty'),
                ('Crates.io', 'lowercase hostname'),
                ('crates.io.', 'lowercase hostname'),
                ('*.crates.io', 'lowercase hostname'),
                ('1.2.3.4', 'numeric'),
                ('a.123', 'numeric'),
                ('a.0x7f', 'numeric'),
                ('a.0x', 'numeric'),
                ('-a.example', "starts or ends with '-'"),
                ('a-.example', "starts or ends with '-'"),
                ('https://crates.io', ''),
                ('crates.io/x', 'lowercase hostname'),
                ('u@crates.io', 'lowercase hostname'),
                ('host.microsandbox.internal', 'sandbox host'),
                ('h.example:443', 'write the host bare'),
                ('h.example:53', 'DNS path'),
                ('h.example:853', 'DNS path'),
                ('h.example:0', 'must be 1..65535'),
                ('h.example:65536', 'must be 1..65535'),
                ('h.example:0443', 'must be 1..65535'),
                ('h.example:', 'without a port'),
                ('h.example:8:9', 'more than one ":"'),
                ('a.example,a.example', 'duplicate'),
                ('a.example,a.example:443', 'write the host bare'),
                ('h.example:1,h.example', 'not in canonical order'),
                ('bücher.example', 'lowercase hostname'),
                ('a.example\n', 'whitespace'),
                ('a.example ', 'whitespace')]:
            with self.subTest(raw=raw):
                self.assert_refused(raw, fragment)

    def test_length_boundaries(self):
        self.assert_accepted('.'.join(['a' * 63] * 3 + ['a' * 60]))   # 252
        self.assert_accepted('.'.join(['a' * 63] * 3 + ['a' * 61]))   # 253
        self.assert_refused('.'.join(['a' * 63] * 3 + ['a' * 62]))    # 254
        self.assert_refused('a' * 64 + '.example', 'over-long label')
        self.assert_accepted('a' * 63 + '.example')

    def test_entry_count_boundary(self):
        names = [f'e{i:03d}.example' for i in range(nr.MAX_ENTRIES)]
        self.assertEqual(len(nr.parse_value(','.join(names)).entries), nr.MAX_ENTRIES)
        self.assert_refused(','.join(names + ['e999.example']), 'at most 64')

    def test_launcher_parity_vectors(self):
        # Copied from agent-vm `crates/agent-vm/src/network_requests.rs`
        # (the reject table) and `crates/agent-vm/src/egress_hostname.rs` (the
        # hostname oracle). Literal data on purpose: this is a differential
        # test against another repository's table, not a restatement of ours.
        for raw in ['*.example.com', '.example.com', 'a..b', '1.2.3', '127.1',
                    '0x7f.1', '1234', 'user@h.example', 'h.example/path',
                    'h.example?q', 'h.example#f', '[h.example]:443',
                    'bücher.example', '10.0.0.0/24:22', '1.2.3.4', '::1',
                    'h.example:53', 'h.example:853', 'host.microsandbox.internal',
                    'API.Example.COM.', 'tcp://h.example', 'h.example:443']:
            with self.subTest(raw=raw):
                self.assert_refused(raw)
        # The launcher accepts these; so must we, byte for byte.
        for raw in ['api.example.com', 'h.example:8443', 'build', '0x7f.example',
                    'xn--bcher-kva.example', 'h.example:1', 'h.example:65535']:
            with self.subTest(raw=raw):
                self.assertEqual(str(self.assert_accepted(raw)), raw)


class CanonicalOrder(unittest.TestCase):
    def test_same_host_order(self):
        canonical = 'h.example,h.example:1,h.example:8443'
        self.assertEqual(str(nr.parse_value(canonical)), canonical)
        for raw, expected in [
                ('h.example:1,h.example', 'h.example,h.example:1'),
                ('h.example:8443,h.example:1', 'h.example:1,h.example:8443'),
                ('h.example,h.example:8443,h.example:1',
                 'h.example,h.example:1,h.example:8443')]:
            with self.subTest(raw=raw):
                with self.assertRaises(nr.RequestError) as raised:
                    nr.parse_value(raw)
                self.assertIn(f'expected: {expected}', str(raised.exception))

    def test_orders_a_string_sort_gets_wrong(self):
        # Numeric, not lexicographic, port order.
        self.assertEqual(str(nr.parse_value('h.example:9,h.example:10')),
                         'h.example:9,h.example:10')
        with self.assertRaises(nr.RequestError):
            nr.parse_value('h.example:10,h.example:9')
        # Whole host first: '-' sorts before ':' as characters, so a string sort
        # of the entries would put the ported a.example:8443 after a.example-x.
        self.assertEqual(str(nr.parse_value('a.example:8443,a.example-x.example')),
                         'a.example:8443,a.example-x.example')
        with self.assertRaises(nr.RequestError):
            nr.parse_value('a.example-x.example,a.example:8443')

    def test_host_order_wins_over_port(self):
        self.assertEqual(str(nr.parse_value('a.example:8443,b.example')),
                         'a.example:8443,b.example')
        with self.assertRaises(nr.RequestError):
            nr.parse_value('b.example,a.example:8443')

    def test_no_generated_ordering(self):
        # canonical_key is the only order; a key-less sort must not silently
        # invent a third one.
        first = nr.NetworkRequest('h.example', None)
        second = nr.NetworkRequest('h.example', 8443)
        with self.assertRaises(TypeError):
            sorted([second, first])
        self.assertEqual(sorted([second, first], key=nr.canonical_key), [first, second])


class Properties(unittest.TestCase):
    ALPHABET = 'abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.:/-[]@*_, '

    def test_round_trip_and_permutations(self):
        rng = random.Random(20260214)
        for _ in range(2000):
            hosts = [f'{rng.choice("abcdefghij")}{rng.randint(0, 9)}.example'
                     for _ in range(rng.randint(1, 4))]
            entries = set()
            for host in hosts:
                # Deliberately includes same-host groups: bare, :1 and a random
                # port are the case where two sort orders disagree.
                entries.add(nr.NetworkRequest(host, None))
                ports = {1, 8443, rng.randint(2, 65535)} - {nr.DEFAULT_PORT} \
                    - nr.FORBIDDEN_PORTS
                for port in rng.sample(sorted(ports), rng.randint(0, len(ports))):
                    entries.add(nr.NetworkRequest(host, port))
            entries = sorted(entries, key=nr.canonical_key)[:nr.MAX_ENTRIES]
            canonical = ','.join(str(entry) for entry in entries)
            parsed = nr.parse_value(canonical)
            self.assertEqual(str(nr.parse_value(str(parsed))), canonical)
            shuffled = list(entries)
            rng.shuffle(shuffled)
            other = ','.join(str(entry) for entry in shuffled)
            if other == canonical:
                continue
            with self.assertRaises(nr.RequestError):
                nr.parse_value(other)

    def test_parser_only_raises_request_error(self):
        rng = random.Random(7)
        for _ in range(2000):
            raw = ''.join(rng.choice(self.ALPHABET) for _ in range(rng.randint(0, 24)))
            try:
                nr.parse_value(raw)
            except nr.RequestError:
                pass


class KeyScheme(unittest.TestCase):
    def test_accepted_keys(self):
        for raw, scope, component in [
                (nr.PREFIX + 'image.base', nr.Scope.IMAGE, 'base'),
                (nr.PREFIX + 'agent.claude', nr.Scope.AGENT, 'claude'),
                (nr.PREFIX + 'agent.rust-dev', nr.Scope.AGENT, 'rust-dev')]:
            with self.subTest(raw=raw):
                key = nr.parse_key(raw)
                self.assertEqual((key.scope, key.component), (scope, component))
                self.assertEqual(key.label(), raw)

    def test_refused_keys(self):
        for raw in [nr.PREFIX + 'image.', nr.PREFIX + 'agent.', nr.PREFIX + 'foo.x',
                    nr.PREFIX + 'image.Rust', nr.PREFIX + 'image.rust_dev',
                    nr.PREFIX + 'image.rust-dev.extra', nr.PREFIX + 'image.-x',
                    nr.PREFIX + 'image..base', nr.PREFIX.rstrip('.'),
                    'org.agent-vm.version.claude']:
            with self.subTest(raw=raw):
                with self.assertRaises(nr.RequestError):
                    nr.parse_key(raw)

    def test_labels_from_mapping(self):
        declared = nr.labels_from_mapping({
            'org.agent-vm.version.claude': '2.1.286',
            nr.PREFIX + 'image.base': 'pypi.org',
            nr.PREFIX + 'agent.dsh': '',
        })
        self.assertEqual(sorted(key.label() for key in declared),
                         [nr.PREFIX + 'agent.dsh', nr.PREFIX + 'image.base'])
        self.assertEqual(str(declared[nr.RequestKey(nr.Scope.AGENT, 'dsh')]), '')
        # The singular typo would otherwise be silently ignored.
        with self.assertRaises(nr.RequestError):
            nr.labels_from_mapping({'org.agent-vm.network-request.image.base': ''})
        with self.assertRaises(nr.RequestError):
            nr.labels_from_mapping({nr.PREFIX + 'image.base': 'pypi.org,'})


class SourceForm(ScratchCase):
    def labels_for(self, text: str) -> tuple[nr.SourceLabel, ...]:
        path = self.tree.write('x/Dockerfile', text)
        return nr.source_labels(path, root=self.tree.root)

    def problems_for(self, text: str) -> str:
        path = self.tree.write('x/Dockerfile', text)
        with self.assertRaises(nr.RequestError) as raised:
            nr.source_labels(path, root=self.tree.root)
        return str(raised.exception)

    def test_accepted_form(self):
        labels = self.labels_for(recipe('FROM scratch', BASE_LABEL))
        self.assertEqual([(label.key.label(), str(label.value), label.line) for label in labels],
                         [(nr.PREFIX + 'image.base', '', 2)])
        self.assertEqual(labels[0].path, 'x/Dockerfile')

    def test_builder_stage_label(self):
        self.assertIn('builder stage',
                      self.problems_for(recipe('FROM build', BASE_LABEL, 'FROM scratch')))

    def test_continued_two_pair_label(self):
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch',
            'LABEL org.agent-vm.network-requests.image.base="" \\',
            '    org.agent-vm.network-requests.agent.pi="pi.dev"')))

    def test_argument_value(self):
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch',
            'ARG HOSTS',
            'LABEL org.agent-vm.network-requests.image.base="${HOSTS}"')))

    def test_unquoted_value(self):
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch', 'LABEL org.agent-vm.network-requests.image.base=pypi.org')))

    def test_lowercase_verb(self):
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch', 'label org.agent-vm.network-requests.image.base=""')))

    def test_duplicate_key(self):
        self.assertIn('duplicate key', self.problems_for(
            recipe('FROM scratch', BASE_LABEL, BASE_LABEL)))

    def test_comment_is_ignored(self):
        self.assertEqual(self.labels_for(recipe('FROM scratch', '# ' + BASE_LABEL)), ())

    def test_typo_key(self):
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch', 'LABEL org.agent-vm.network-request.image.base=""')))

    def test_run_label_flag(self):
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch',
            'RUN docker build --label org.agent-vm.network-requests.image.base=')))


class InstructionBoundaries(ScratchCase):
    def problems_for(self, text: str) -> str:
        path = self.tree.write('x/Dockerfile', text)
        with self.assertRaises(nr.RequestError) as raised:
            nr.source_labels(path, root=self.tree.root)
        return str(raised.exception)

    def lines_for(self, text: str) -> list[int]:
        path = self.tree.write('x/Dockerfile', text)
        return [label.line for label in nr.source_labels(path, root=self.tree.root)]

    def test_matching_label_inside_heredoc_is_rejected(self):
        # The exact case a raw-line scanner accepts: a body line that matches the
        # strict form and sits after the last FROM.
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch', 'RUN <<EOF', BASE_LABEL, 'EOF')))

    def test_tab_and_quoted_heredocs(self):
        # The same body-line case, with the terminator spellings Docker allows.
        for opener, terminator in [('RUN <<-EOF', '\tEOF'), ("RUN <<'EOF'", 'EOF'),
                                   ('RUN <<"EOF"', 'EOF')]:
            with self.subTest(opener=opener):
                self.assertIn('unsupported form', self.problems_for(recipe(
                    'FROM scratch', opener, BASE_LABEL, terminator)))

    def test_two_heredocs_on_one_instruction(self):
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch', 'RUN <<A <<B', 'first', 'A', BASE_LABEL, 'B')))

    def test_heredoc_body_from_does_not_start_a_stage(self):
        self.assertEqual(self.lines_for(recipe(
            'FROM scratch', 'RUN <<EOF', 'FROM other', 'EOF', BASE_LABEL)), [5])

    def test_label_after_terminator_line(self):
        self.assertEqual(self.lines_for(recipe(
            'FROM scratch', 'RUN <<EOF', 'body', 'EOF', BASE_LABEL)), [5])

    def test_unterminated_heredoc(self):
        self.assertIn('never terminated',
                      self.problems_for(recipe('FROM scratch', 'RUN <<EOF', 'body')))

    def test_heredoc_after_an_escaped_quote(self):
        # Docker lexes `"a\"b"` as one word, so `<<EOF` opens a heredoc and the
        # LABEL below it is body text. Scanning the physical line on its own ends
        # the quote at the escaped `"` and accepts the label Docker never made.
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch', 'RUN echo "a\\"b" <<EOF', BASE_LABEL, 'EOF')))

    def test_heredoc_after_a_quote_across_a_continuation(self):
        # The same false acceptance with the quoted string split by a
        # continuation: Docker joins the lines before it lexes them.
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch', 'RUN echo "a \\', 'b" <<EOF', BASE_LABEL, 'EOF')))

    def test_arithmetic_shift_is_not_a_heredoc(self):
        # `((1<<2))` is ordinary shell arithmetic. Docker opens nothing and
        # creates the label, so refusing the recipe would refuse valid input.
        self.assertEqual(self.lines_for(recipe(
            'FROM scratch', 'RUN echo $((1<<2))', BASE_LABEL)), [3])

    def test_shift_in_the_middle_of_a_word_is_not_a_heredoc(self):
        # `a<<EOF` redirects stdin of `echo`, but the word does not begin with
        # `<<`, so Docker reads the next line as an instruction and the label
        # exists.
        self.assertEqual(self.lines_for(recipe(
            'FROM scratch', 'RUN echo a<<EOF', BASE_LABEL)), [3])

    def test_continued_run_with_key(self):
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch', 'RUN echo \\',
            '  && echo org.agent-vm.network-requests.image.base')))

    def test_key_split_across_a_continuation(self):
        # Docker removes the continuation mark, joins the two lines and creates
        # the label. Zero labels reported here would be a declaration nobody
        # checked; the form is not the single-line form R6 allows, so it is
        # refused instead.
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch', 'LABEL org.agent-vm.netwo\\', 'rk-requests.image.base=""')))

    def test_blank_line_inside_a_continuation_does_not_end_it(self):
        # Docker skips the blank line, so the LABEL is part of the RUN and the
        # image carries no label at all.
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch', 'RUN true \\', '', BASE_LABEL)))

    def test_heredoc_delimiter_is_not_truncated(self):
        # `<<END-MARKER` ends at `END-MARKER`, so the `END` line and the LABEL
        # are both body text that Docker never reads as instructions.
        self.assertIn('unsupported form', self.problems_for(recipe(
            'FROM scratch', 'RUN cat <<END-MARKER >/tmp/x', 'END', BASE_LABEL,
            'END-MARKER')))

    def test_valid_docker_syntax_that_opens_no_heredoc(self):
        # Each of these is an ordinary instruction, so a recipe unrelated to
        # request labels has to stay checkable.
        for text in [recipe('FROM scratch', 'RUN echo "1<<2"', BASE_LABEL),
                     recipe('FROM scratch', 'RUN echo 1<<2', '2', BASE_LABEL),
                     recipe('FROM scratch', 'RUN echo <<<here-string', BASE_LABEL),
                     recipe('FROM scratch', 'RUN <<EOF-X', 'echo ok', 'EOF-X', BASE_LABEL),
                     recipe('FROM scratch', 'RUN cat <<END-MARKER', 'body', 'END-MARKER',
                            BASE_LABEL),
                     recipe('FROM scratch', "RUN cat <<'END-MARKER'", 'body', 'END-MARKER',
                            BASE_LABEL),
                     recipe('FROM scratch', 'RUN cat <<-TAB', '\tbody', '\tTAB', BASE_LABEL)]:
            with self.subTest(text=text):
                self.assertEqual(self.lines_for(text), [len(text.splitlines())])

    def test_key_only_on_a_continuation_line(self):
        # The first physical line matches neither the strict form nor a
        # first-line-only guard, so this case fails if the guard reads `text`.
        problems = self.problems_for(recipe(
            'FROM scratch', 'LABEL \\',
            '  org.agent-vm.network-requests.agent.pi="pi.dev"'))
        self.assertIn('unsupported form', problems)
        self.assertIn('x/Dockerfile:2', problems)

    def test_non_default_escape_directive(self):
        self.assertIn('escape directive', self.problems_for(recipe(
            '# escape=`', 'FROM scratch', BASE_LABEL)))

    def test_default_escape_directive_is_fine(self):
        self.assertEqual(self.lines_for(recipe(
            '# escape=\\', 'FROM scratch', BASE_LABEL)), [3])


class InputBoundaries(ScratchCase):
    def assert_cli_error(self, arguments: list[str], fragments: list[str]) -> None:
        finished = run_cli(*arguments)
        self.assertEqual(finished.returncode, 1, finished.stderr)
        self.assertNotIn('Traceback', finished.stderr)
        for fragment in fragments:
            self.assertIn(fragment, finished.stderr)

    def test_no_from(self):
        path = self.tree.write('x/Dockerfile', BASE_LABEL)
        self.assert_cli_error(['source-labels', str(path)], [str(path), 'no FROM'])

    def test_not_utf8(self):
        path = self.tree.root / 'x' / 'Dockerfile'
        path.parent.mkdir(parents=True)
        path.write_bytes(b'FROM x\nLABEL a="\xff"\n')
        self.assert_cli_error(['source-labels', str(path)], [str(path), 'not UTF-8'])

    def test_check_sources_reports_the_same_file(self):
        path = self.tree.root / 'images' / 'Dockerfile'
        path.parent.mkdir(parents=True)
        path.write_bytes(b'LABEL a="\xff"\n')
        self.assert_cli_error(['--root', str(self.tree.root), 'check-sources'],
                              ['images/Dockerfile', 'not UTF-8'])

    def test_missing_file(self):
        self.assert_cli_error(['source-labels', str(self.tree.root / 'nope')],
                              ['nope', 'cannot read'])

    def test_inspect_shapes(self):
        cases = {
            'empty-array': ('[]', 'one-element array'),
            'object': ('{}', 'one-element array'),
            'two-images': ('[{}, {}]', 'one-element array'),
            'element': ('[1]', '.[0] is a number'),
            'no-config': ('[{}]', 'Config'),
            'config-array': ('[{"Config": []}]', 'Config'),
            'labels-array': ('[{"Config": {"Labels": []}}]', 'Labels'),
            'label-number': ('[{"Config": {"Labels": '
                             '{"org.agent-vm.network-requests.image.base": 1}}}]',
                             'is a number, expected a string'),
        }
        for name, (payload, fragment) in cases.items():
            path = self.tree.write(f'{name}.json', payload)
            with self.subTest(case=name):
                self.assert_cli_error(
                    ['check-image', '--inspect', str(path), '--dockerfile',
                     'images/Dockerfile'], [str(path), fragment])
                self.assert_cli_error(
                    ['egress-config', '--inspect', str(path), '--key',
                     nr.PREFIX + 'image.base'], [str(path), fragment])

    def test_inspect_not_json(self):
        path = self.tree.write('bad.json', 'not json')
        self.assert_cli_error(['check-image', '--inspect', str(path)],
                              [str(path), 'not JSON'])

    def test_inspect_not_utf8(self):
        # A byte that is not UTF-8 must be a contextual message on every verb,
        # not a `UnicodeDecodeError` traceback out of `json.loads`.
        bad = self.tree.root / 'bad-bytes.json'
        bad.write_bytes(b'\xff')
        good = self.tree.write('good.json', json.dumps([{'Config': {'Labels': {}}}]))
        key = nr.PREFIX + 'image.base'
        self.assert_cli_error(['check-image', '--inspect', str(bad)],
                              [str(bad), 'not UTF-8'])
        self.assert_cli_error(['check-image', '--inspect', str(good),
                               '--inherits', str(bad)], [str(bad), 'not UTF-8'])
        self.assert_cli_error(['egress-config', '--inspect', str(bad), '--key', key],
                              [str(bad), 'not UTF-8'])

    def test_inherits_shapes_are_checked(self):
        good = self.tree.write('good.json', json.dumps([{'Config': {'Labels': {}}}]))
        bad = self.tree.write('bad.json', '[]')
        self.assert_cli_error(
            ['check-image', '--inspect', str(good), '--inherits', str(bad)],
            [str(bad), 'one-element array'])

    def test_null_labels(self):
        path = self.tree.write('null-labels.json',
                               json.dumps([{'Config': {'Labels': None}}]))
        finished = run_cli('check-image', '--inspect', str(path))
        self.assertEqual(finished.returncode, 0, finished.stderr)

    def test_usage_error_exits_two(self):
        self.assertEqual(run_cli('check-image').returncode, 2)


class Ownership(ScratchCase):
    def tree_with(self, files: dict[str, str], tools: tuple[str, ...] = ('claude', 'pi',
                                                                       'dsh')) -> None:
        for name in tools:
            (self.tree.root / 'images' / 'tools' / name).mkdir(parents=True, exist_ok=True)
        for relative, text in files.items():
            self.tree.write(relative, text)

    def problems(self, files: dict[str, str]) -> list[str]:
        self.tree_with(files)
        return list(nr.check_sources(self.tree.root).problems)

    def test_clean_tree(self):
        problems = self.problems({
            'images/Dockerfile': recipe('FROM base', BASE_LABEL),
            'images/standard/Dockerfile': recipe(
                'FROM base', 'LABEL org.agent-vm.network-requests.agent.claude=""'),
            'examples/layers/rust-dev/Dockerfile': recipe(
                'FROM base',
                'LABEL org.agent-vm.network-requests.image.rust-dev="crates.io"'),
            'examples/base-extension/Dockerfile': recipe('FROM base'),
        })
        self.assertEqual(problems, [])

    def test_base_declaring_an_agent_key(self):
        self.assertIn('is not this component', ''.join(self.problems({
            'images/Dockerfile': recipe(
                'FROM base', BASE_LABEL,
                'LABEL org.agent-vm.network-requests.agent.claude=""')})))

    def test_standard_declaring_an_absent_tool(self):
        self.assertIn('is not this component', ''.join(self.problems({
            'images/standard/Dockerfile': recipe(
                'FROM base', 'LABEL org.agent-vm.network-requests.agent.nope=""')})))

    def test_standard_declaring_an_image_key(self):
        self.assertIn('is not this component', ''.join(self.problems({
            'images/standard/Dockerfile': recipe('FROM base', BASE_LABEL)})))

    def test_layer_declaring_another_layers_key(self):
        self.assertIn('is not this component', ''.join(self.problems({
            'examples/layers/rust-dev/Dockerfile': recipe(
                'FROM base', 'LABEL org.agent-vm.network-requests.image.go-dev=""')})))

    def test_layer_must_declare_its_key(self):
        self.assertIn('missing required key', ''.join(self.problems({
            'examples/layers/chrome-devtools/Dockerfile': recipe('FROM base')})))

    def test_extension_must_declare_nothing(self):
        self.assertIn('is not this component', ''.join(self.problems({
            'examples/base-extension/Dockerfile': recipe('FROM base', BASE_LABEL)})))

    def test_other_dockerfile_must_declare_nothing(self):
        self.assertIn('is not this component', ''.join(self.problems({
            'script/test/fixtures/thing/Dockerfile': recipe('FROM base', BASE_LABEL)})))

    def test_a_dockerfile_outside_the_known_roots_must_declare_nothing(self):
        # R5 is about every recipe in the repository, not only the three roots
        # that own a key today, so a walk over just those roots leaves a recipe
        # that can declare any component's key unchecked.
        self.assertIn('is not this component', ''.join(self.problems({
            'other/Dockerfile': recipe('FROM base', BASE_LABEL)})))
        self.assertIn('is not this component', ''.join(self.problems({
            'docs/example.Dockerfile': recipe('FROM base', BASE_LABEL)})))

    def test_a_container_actions_dockerfile_is_walked(self):
        # `.github` is hidden but it is repository content, and a Docker
        # container action keeps its recipe there. Pruning every hidden
        # directory would let that recipe declare anything.
        self.assertIn('is not this component', ''.join(self.problems({
            '.github/actions/x/Dockerfile': recipe('FROM base', BASE_LABEL)})))

    def test_scratch_and_hidden_directories_are_not_walked(self):
        # Build output, installed packages and developer/VCS scratch are not
        # repository content, so a Dockerfile there is neither checked nor owned.
        self.tree_with({'images/Dockerfile': recipe('FROM base', BASE_LABEL)})
        for relative in ('target/scratch/Dockerfile', '.pi/work/Dockerfile',
                         'node_modules/pkg/Dockerfile', 'images/__pycache__/Dockerfile'):
            self.tree.write(relative, recipe('FROM base', BASE_LABEL))
        report = nr.check_sources(self.tree.root)
        self.assertEqual(report.problems, ())
        self.assertEqual(report.dockerfiles, 1)

    def test_symlinked_dockerfile_is_refused(self):
        self.tree_with({'images/Dockerfile': recipe('FROM base', BASE_LABEL)})
        (self.tree.root / 'examples' / 'layers').mkdir(parents=True)
        (self.tree.root / 'examples' / 'layers' / 'x').mkdir()
        (self.tree.root / 'examples' / 'layers' / 'x' / 'Dockerfile').write_text(
            recipe('FROM base'))
        target = self.tree.root / 'examples' / 'layers' / 'x' / 'Dockerfile'
        real = self.tree.root / 'real.Dockerfile'
        real.write_text(target.read_text())
        target.unlink()
        target.symlink_to(real)
        self.assertIn('symlinked', ''.join(nr.check_sources(self.tree.root).problems))


class CheckImage(ScratchCase):
    def inspect(self, labels: dict[str, str] | None) -> nr.InspectedImage:
        return nr.read_inspect(self.tree.write('i.json', json.dumps(
            [{'Config': {'Labels': labels}}])))

    def dockerfile(self, *labels: str) -> Path:
        return self.tree.write('examples/layers/rust-dev/Dockerfile',
                              recipe('FROM base', *labels))

    def check(self, actual: dict[str, str] | None, *, source_labels=(),
              parent: dict[str, str] | None = None) -> list[str]:
        sources = [self.dockerfile(*source_labels)] if source_labels else []
        return nr.check_image(self.inspect(actual), sources=sources,
                              inherits=self.inspect(parent) if parent is not None else None,
                              root=self.tree.root)

    def test_equal(self):
        self.assertEqual(self.check({nr.PREFIX + 'image.rust-dev': 'crates.io'},
                                    source_labels=['LABEL ' + nr.PREFIX
                                                   + 'image.rust-dev="crates.io"']), [])

    def test_missing_source_key(self):
        problems = self.check({}, source_labels=[
            'LABEL ' + nr.PREFIX + 'image.rust-dev="crates.io"'])
        self.assertIn('did not reach the final stage', ''.join(problems))

    def test_extra_key(self):
        problems = self.check({nr.PREFIX + 'image.other': ''})
        self.assertIn('not declared by the sources', ''.join(problems))

    def test_changed_value(self):
        problems = self.check({nr.PREFIX + 'image.rust-dev': 'crates.io'},
                              source_labels=['LABEL ' + nr.PREFIX
                                             + 'image.rust-dev="index.crates.io"'])
        self.assertIn('but the sources and parent imply', ''.join(problems))

    def test_parent_keys_are_preserved(self):
        parent = {nr.PREFIX + 'image.base': 'pypi.org'}
        own = {'LABEL ' + nr.PREFIX + 'image.rust-dev': '"crates.io"'}
        self.assertEqual(self.check({nr.PREFIX + 'image.base': 'pypi.org',
                                     nr.PREFIX + 'image.rust-dev': 'crates.io'},
                                    source_labels=[
                                        'LABEL ' + nr.PREFIX
                                        + 'image.rust-dev="crates.io"'],
                                    parent=parent), [])

    def test_parent_key_overwritten(self):
        problems = self.check(
            {nr.PREFIX + 'image.base': 'api.github.com',
             nr.PREFIX + 'image.rust-dev': 'crates.io'},
            source_labels=['LABEL ' + nr.PREFIX + 'image.rust-dev="crates.io"'],
            parent={nr.PREFIX + 'image.base': 'pypi.org'})
        self.assertIn('but the sources and parent imply', ''.join(problems))

    def test_parent_key_missing(self):
        problems = self.check({nr.PREFIX + 'image.rust-dev': 'crates.io'},
                              source_labels=['LABEL ' + nr.PREFIX
                                             + 'image.rust-dev="crates.io"'],
                              parent={nr.PREFIX + 'image.base': 'pypi.org'})
        self.assertIn('did not reach the final stage', ''.join(problems))

    def test_invalid_parent_value(self):
        problems = self.check({nr.PREFIX + 'image.rust-dev': 'crates.io'},
                              source_labels=['LABEL ' + nr.PREFIX
                                             + 'image.rust-dev="crates.io"'],
                              parent={nr.PREFIX + 'image.base': 'pypi.org,'})
        self.assertIn('parent:', ''.join(problems))

    def test_source_and_parent_disagree(self):
        problems = self.check({nr.PREFIX + 'image.base': 'pypi.org'},
                              source_labels=['LABEL ' + nr.PREFIX + 'image.base="pypi.org"'],
                              parent={nr.PREFIX + 'image.base': 'pypi.org'})
        self.assertIn('would overwrite an inherited request', ''.join(problems))

    def test_null_labels(self):
        self.assertEqual(nr.check_image(self.inspect(None), sources=[],
                                        inherits=None, root=self.tree.root), [])

    def test_documented_check_needs_the_parent(self):
        # `examples/layers/README.md` passes `--inherits parent.json`. Without it a
        # correctly stacked image is rejected for the parent's own label, which is
        # the failure the documented command used to produce.
        actual = {nr.PREFIX + 'image.base': 'pypi.org',
                  nr.PREFIX + 'image.rust-dev': 'crates.io'}
        child = ['LABEL ' + nr.PREFIX + 'image.rust-dev="crates.io"']
        parent = {nr.PREFIX + 'image.base': 'pypi.org'}
        self.assertEqual(self.check(actual, source_labels=child, parent=parent), [])
        without_parent = nr.check_image(self.inspect(actual), sources=[self.dockerfile(*child)],
                                        inherits=None, root=self.tree.root)
        self.assertIn(nr.PREFIX + 'image.base is "pypi.org" and is not declared',
                      ''.join(without_parent))


class EgressConfig(ScratchCase):
    def image(self, labels: dict[str, str]) -> nr.InspectedImage:
        return nr.read_inspect(self.tree.write('i.json', json.dumps(
            [{'Config': {'Labels': labels}}])))

    def test_exact_config(self):
        key = nr.RequestKey(nr.Scope.IMAGE, 'rust-dev')
        config = nr.egress_config(self.image(
            {key.label(): 'crates.io,index.crates.io,static.crates.io'}), key)
        self.assertEqual(json.dumps(config), json.dumps({
            'allow': [{'host': 'crates.io', 'path': '^/'},
                      {'host': 'index.crates.io', 'path': '^/'},
                      {'host': 'static.crates.io', 'path': '^/'}],
            'https_only': True,
            'learn_redirects': False,
            'enforce_tls_destination': True,
        }))

    def test_refusals(self):
        key = nr.RequestKey(nr.Scope.IMAGE, 'rust-dev')
        for labels, fragment in [
                ({}, 'not inventoried'),
                ({key.label(): ''}, 'empty'),
                ({key.label(): 'h.example:8443'}, '443-only')]:
            with self.subTest(fragment=fragment):
                with self.assertRaises(nr.RequestError) as raised:
                    nr.egress_config(self.image(labels), key)
                self.assertIn(fragment, str(raised.exception))


class CommittedValues(unittest.TestCase):
    """The reviewed value table, duplicated here on purpose.

    ADR 0014's evidence table justifies each host; this literal copy turns a
    value change into a two-file reviewed edit (the recipe and this table), the
    way `selections.tsv` mirrors the version ARGs.
    """

    REVIEWED = {
        'images/Dockerfile': {
            'image.base': 'api.github.com,files.pythonhosted.org,github.com,'
                          'pypi.org,registry.npmjs.org'},
        'images/standard/Dockerfile': {
            'agent.agy': 'cloudcode-pa.googleapis.com,daily-cloudcode-pa.googleapis.com,'
                         'oauth2.googleapis.com,www.googleapis.com',
            'agent.claude': 'api.anthropic.com,mcp-proxy.anthropic.com,platform.claude.com',
            'agent.codex': 'api.openai.com,auth.openai.com,chatgpt.com',
            'agent.copilot': 'api.business.githubcopilot.com,api.enterprise.githubcopilot.com,'
                             'api.githubcopilot.com,api.individual.githubcopilot.com',
            'agent.dsh': '',
            'agent.opencode': 'models.opencode.ai',
            'agent.pi': 'pi.dev'},
        'examples/layers/rust-dev/Dockerfile': {
            'image.rust-dev': 'crates.io,index.crates.io,static.crates.io'},
        'examples/layers/go-dev/Dockerfile': {
            'image.go-dev': 'proxy.golang.org,sum.golang.org'},
        'examples/layers/chrome-devtools/Dockerfile': {'image.chrome-devtools': ''},
        'examples/layers/wirenboard-cpp/Dockerfile': {'image.wirenboard-cpp': ''},
    }

    def test_committed_values_match_reviewed_table(self):
        for relative, expected in self.REVIEWED.items():
            with self.subTest(recipe=relative):
                labels = nr.source_labels(REPO_ROOT / relative, root=REPO_ROOT)
                self.assertEqual(
                    {label.key.label(): str(label.value) for label in labels},
                    {nr.PREFIX + name: value for name, value in expected.items()})

    def test_copilot_and_agy_declare_a_named_host_list(self):
        # An empty value means "inventoried, contacts nothing", which is true of
        # dsh and false of these two. #143 made copilot and agy inventoried, so a
        # later edit that dropped either one's hosts to "" would turn a review
        # into a silent claim of no traffic. Only these two are asserted here;
        # test_committed_values_match_reviewed_table owns every agent's value.
        labels = nr.source_labels(REPO_ROOT / 'images/standard/Dockerfile',
                                 root=REPO_ROOT)
        declared = {label.key: str(label.value) for label in labels
                    if label.key.scope is nr.Scope.AGENT}
        for component in ('copilot', 'agy'):
            with self.subTest(component=component):
                key = nr.RequestKey(nr.Scope.AGENT, component)
                self.assertIn(key, declared)
                self.assertNotEqual(declared[key], '')

    def test_real_repository_passes(self):
        report = nr.check_sources(REPO_ROOT)
        self.assertEqual(report.problems, ())
        self.assertEqual(report.summary(),
                         f'network requests: {sum(len(v) for v in self.REVIEWED.values())} '
                         f'labels in {report.dockerfiles} Dockerfiles ok')

    def test_the_documented_layer_check_passes_the_parent(self):
        # The README's check has to carry `--inherits`: the exact-equality form
        # rejects a correct layer as soon as its parent ships a request label.
        readme = (REPO_ROOT / 'examples' / 'layers' / 'README.md').read_text()
        block = next((part.split('```', 1)[0] for part in readme.split('```sh')[1:]
                      if 'check-image' in part), None)
        self.assertIsNotNone(block)
        lines = block.splitlines()
        start = next(index for index, line in enumerate(lines) if 'check-image' in line)
        command = []
        for line in lines[start:]:
            command.append(line)
            if not line.rstrip().endswith('\\'):
                break
        self.assertIn('--inherits', ' '.join(command))
        # The block has to say which image the parent is: `$PARENT_IMAGE` alone
        # names nothing a reader could run.
        self.assertIn('PARENT_IMAGE=', block)


if __name__ == '__main__':
    unittest.main(verbosity=2)
