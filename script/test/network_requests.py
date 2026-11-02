#!/usr/bin/env python3
"""The runtime network-request label contract (ADR 0014).

One image tells the launcher which TLS destinations its own software contacts
by carrying one OCI label per declaring component:

    org.agent-vm.network-requests.image.<component>=<value>
    org.agent-vm.network-requests.agent.<agent>=<value>

This module is the only owner of that contract: the key scheme, the value
grammar, the canonical order, the strict Dockerfile source form, which recipe
may declare which key, and the comparison of a built image's labels against
what its sources and its parent imply. Everything else in the repository calls
one of four verbs (`check-sources`, `source-labels`, `check-image`,
`egress-config`) and passes paths, never parsed data.

The grammar deliberately matches the launcher's hostname oracle
(agent-vm `crates/agent-vm/src/network_requests.rs`,
`crates/agent-vm/src/egress_hostname.rs`): the launcher has to accept every
value this module emits, and the one place they might drift is here.

Why a Dockerfile source check instead of a Dockerfile parser: the source form is
restricted to one single-line `LABEL key="literal"` in the final stage (R6), so
an instruction scanner that knows continuations and heredoc boundaries settles
it in milliseconds without Docker. Any other use of the key prefix is refused
rather than guessed at, which is why this fails closed.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from types import MappingProxyType
from typing import Callable, Iterable, Mapping, Sequence

PREFIX = 'org.agent-vm.network-requests.'
# Any key starting with this must use PREFIX exactly: it catches the singular
# `network-request.` typo, which would otherwise be an ignored label.
TYPO_GUARD = 'org.agent-vm.network'
MAX_ENTRIES = 64
SANDBOX_HOST = 'host.microsandbox.internal'
FORBIDDEN_PORTS = frozenset({53, 853})
DEFAULT_PORT = 443
NAME_RE = re.compile(r'[a-z0-9][a-z0-9-]*')
PORT_RE = re.compile(r'[1-9][0-9]{0,4}')
DTYPE_RE = re.compile(r'0x[0-9a-f]*')
LHD_RE = re.compile(r'[a-z0-9-]+')
# The accepted source form (R6). `fullmatch` everywhere: `match` with a `$`
# anchor accepts one trailing newline.
LABEL_RE = re.compile(r'LABEL (org\.agent-vm\.network-requests\.[^=\s"]+)="([^"$\\]*)"')
ESCAPE_DIRECTIVE_RE = re.compile(r'#\s*escape\s*=\s*(\S+)\s*')

# Not repository content: build output, installed packages, VCS metadata and
# developer scratch. These are names, not a rule about hidden directories:
# `.github` holds a container action's recipe and has to be checked like any
# other Dockerfile outside the three roots that own a key.
SCRATCH_DIRECTORIES = frozenset({'target', 'node_modules', '__pycache__', '.git', '.pi'})


class RequestError(Exception):
    """One already-contextualised message: a bad input, never a bug."""


class Scope(str, Enum):
    IMAGE = 'image'
    AGENT = 'agent'


@dataclass(frozen=True, order=True)
class RequestKey:
    """A parsed request key. The component is validated on construction."""

    scope: Scope
    component: str

    def __post_init__(self) -> None:
        if not NAME_RE.fullmatch(self.component):
            raise RequestError(f'{self.label()}: component name {self.component!r} '
                               f'must match {NAME_RE.pattern}')

    def label(self) -> str:
        return f'{PREFIX}{self.scope.value}.{self.component}'

    def __str__(self) -> str:
        return self.label()


@dataclass(frozen=True)
class NetworkRequest:
    """One TLS destination: a bare host (443) or `host:port`.

    There is deliberately no generated ordering: `canonical_key` is the only
    order, because the generated one compares `None` against an int for the
    same host and raises. A key-less `sorted()` of these therefore fails
    loudly instead of silently using a third order.
    """

    host: str
    port: int | None

    def __post_init__(self) -> None:
        reason = _host_reason(self.host)
        if reason is not None:
            raise RequestError(f'{self.host!r}: {reason}')
        reason = _port_reason(self.port)
        if reason is not None:
            raise RequestError(f'{self.host}: {reason}')

    def __str__(self) -> str:
        return self.host if self.port is None else f'{self.host}:{self.port}'


def canonical_key(entry: NetworkRequest) -> tuple[str, int]:
    """Ascending host, and the bare (443) entry before any numbered port."""
    return (entry.host, entry.port or 0)


@dataclass(frozen=True)
class RequestList:
    """A validated, canonically ordered, duplicate-free value of at most 64."""

    entries: tuple[NetworkRequest, ...]

    def __str__(self) -> str:
        return ','.join(str(entry) for entry in self.entries)


@dataclass(frozen=True)
class SourceLabel:
    key: RequestKey
    value: RequestList
    path: str
    line: int


Declarations = Mapping[RequestKey, RequestList]


@dataclass(frozen=True)
class Instruction:
    """One logical Dockerfile instruction, with every physical line it owns.

    `text` is the first physical line and the only input to strict LABEL
    matching; `joined` is Docker's own view of the instruction, with the
    continuation marks removed and the blank and comment lines Docker skips
    contributing nothing, and it is what the fail-closed guard searches, so a key
    split across a continuation cannot slip past. `lines` is every physical line
    (the first, each continuation and each heredoc body/terminator).
    `heredoc_body` is the body content of the heredocs this instruction opened,
    in order.
    """

    keyword: str
    text: str
    line: int
    lines: tuple[str, ...]
    joined: str
    heredoc_body: tuple[str, ...]


@dataclass(frozen=True)
class InspectedImage:
    """The only shape `check-image` and `egress-config` accept."""

    labels: Mapping[str, str]


@dataclass(frozen=True)
class Ownership:
    """Which keys one recipe may declare, and which it must."""

    allowed: Callable[[RequestKey], bool]
    required: tuple[RequestKey, ...]


# --- value grammar ------------------------------------------------------------
def _host_reason(host: str) -> str | None:
    if not 1 <= len(host) <= 253:
        return 'hostname must be 1..253 characters'
    labels = host.split('.')
    for label in labels:
        if not label or len(label) > 63:
            return 'not an exact lowercase hostname (empty or over-long label)'
        if not LHD_RE.fullmatch(label):
            return 'not an exact lowercase hostname (letters, digits and "-" only)'
        if label.startswith('-') or label.endswith('-'):
            return "hostname label starts or ends with '-'"
    final = labels[-1]
    # Launcher parity: a numeric final label is an IPv4 address in disguise.
    if final.isdigit() or DTYPE_RE.fullmatch(final):
        return 'the final label is numeric, so this is an address, not a hostname'
    if host == SANDBOX_HOST:
        return 'the sandbox host is never a request'
    return None


def _port_reason(port: int | None) -> str | None:
    if port is None:
        return None
    if not 1 <= port <= 65535:
        return f'port {port} must be 1..65535'
    if port in FORBIDDEN_PORTS:
        return f'port {port} is the DNS path, never a TLS destination'
    if port == DEFAULT_PORT:
        return 'port 443 is the default; write the host bare'
    return None


def _parse_entry(piece: str, index: int) -> NetworkRequest:
    host, separator, port = piece.partition(':')
    if not host:
        raise RequestError(f'entry {index} is empty')
    if separator and not port:
        raise RequestError(f'entry {index}: ":" without a port')
    if ':' in port:
        raise RequestError(f'entry {index}: more than one ":"')
    parsed_port = None
    if port:
        if not PORT_RE.fullmatch(port):
            raise RequestError(f'entry {index}: port {port!r} must be 1..65535 '
                               f'with no leading zero')
        parsed_port = int(port)
        reason = _port_reason(parsed_port)
        if reason is not None:
            raise RequestError(f'entry {index}: {reason}')
    reason = _host_reason(host)
    if reason is not None:
        raise RequestError(f'entry {index}: {reason}')
    return NetworkRequest(host, parsed_port)


def parse_value(raw: str) -> RequestList:
    """Parse one label value. The raw string is never re-validated elsewhere."""
    if not isinstance(raw, str):
        raise RequestError(f'value is a {_json_type(raw)}, expected a string')
    if raw == '':
        # Inventoried, contacts nothing. This is not the same as an absent key.
        return RequestList(())
    for position, character in enumerate(raw):
        if character.isspace():
            return _reject_whitespace(position)
    entries = tuple(_parse_entry(piece, index) for index, piece in enumerate(raw.split(',')))
    if len(entries) > MAX_ENTRIES:
        raise RequestError(f'{len(entries)} entries; at most {MAX_ENTRIES} are allowed')
    seen: set[NetworkRequest] = set()
    for entry in entries:
        if entry in seen:
            raise RequestError(f'duplicate entry {entry}')
        seen.add(entry)
    ordered = tuple(sorted(entries, key=canonical_key))
    if entries != ordered:
        expected = ','.join(str(entry) for entry in ordered)
        raise RequestError(f'not in canonical order; expected: {expected}')
    return RequestList(entries)


def _reject_whitespace(position: int) -> None:
    raise RequestError(f'whitespace is not allowed (position {position})')


def parse_key(raw: str) -> RequestKey:
    if not raw.startswith(PREFIX):
        raise RequestError(f'{raw!r} is not a network-request key (expected the prefix {PREFIX})')
    scope_text, dot, component = raw[len(PREFIX):].partition('.')
    if not dot:
        raise RequestError(f'{raw!r}: missing ".image." or ".agent." scope')
    try:
        scope = Scope(scope_text)
    except ValueError:
        raise RequestError(f'{raw!r}: scope {scope_text!r} is neither "image" nor "agent"')
    return RequestKey(scope, component)


def labels_from_mapping(labels: Mapping[str, str]) -> Declarations:
    """Every key that looks like ours must parse; unrelated labels are ignored."""
    declarations: dict[RequestKey, RequestList] = {}
    for raw_key, raw_value in labels.items():
        if not raw_key.startswith(TYPO_GUARD):
            continue
        key = parse_key(raw_key)
        declarations[key] = parse_value(raw_value)
    return MappingProxyType(declarations)


# --- Dockerfile source form ---------------------------------------------------
def read_source(dockerfile: Path, *, path: str | None = None) -> str:
    display = path or str(dockerfile)
    try:
        raw = dockerfile.read_bytes()
    except OSError as error:
        raise RequestError(f'{display}: cannot read: {error.strerror or error}')
    try:
        return raw.decode('utf-8')
    except UnicodeDecodeError as error:
        raise RequestError(f'{display}: not UTF-8 at byte {error.start} '
                           f'({error.reason})')


def _check_escape_directive(lines: Sequence[str], path: str) -> None:
    """A non-default `# escape=` changes what a continuation even is."""
    for number, line in enumerate(lines, start=1):
        if not line.strip():
            continue
        if not line.lstrip().startswith('#'):
            return
        match = ESCAPE_DIRECTIVE_RE.fullmatch(line.lstrip())
        if match is not None and match.group(1) != '\\':
            raise RequestError(f'{path}:{number}: unsupported escape directive '
                               f'{match.group(1)!r}')


def _continues(lines: Sequence[str]) -> bool:
    """Docker's rule: blank and comment lines inside an instruction do not end it."""
    for line in reversed(lines):
        stripped = line.strip()
        if not stripped or stripped.startswith('#'):
            continue
        return line.rstrip().endswith('\\')
    return False


def _joined_text(lines: Sequence[str]) -> str:
    """Docker's logical instruction text: the continuation mark is removed.

    The backslash and the newline are both dropped, so a key split across two
    physical lines appears whole here, which is where the guard has to see it.
    """
    joined = ''
    for line in lines:
        stripped = line.strip()
        if not stripped or stripped.startswith('#'):
            continue
        text = line.rstrip()
        joined += text[:-1] if text.endswith('\\') else text
    return joined


def _shell_words(text: str) -> list[str]:
    """One instruction split into words the way a shell does.

    Docker lexes the joined instruction before it looks for heredocs, so `"1<<2"`
    is a single word and `a<<EOF` is a word that does not begin with `<<`. Only
    word boundaries and quoting matter here, so a word keeps its quotes and
    escapes: the caller inspects its first characters.
    """
    words: list[str] = []
    word: list[str] = []
    quote = ''
    index = 0
    while index < len(text):
        character = text[index]
        if not quote and character.isspace():
            if word:
                words.append(''.join(word))
                word = []
            index += 1
            continue
        if character == '\\' and quote != "'" and index + 1 < len(text):
            # Outside quotes, and inside double quotes, a backslash escapes the
            # next character so that `\"` cannot close a quoted string.
            word.append(text[index:index + 2])
            index += 2
            continue
        if quote:
            if character == quote:
                quote = ''
        elif character in '"\'':
            quote = character
        word.append(character)
        index += 1
    if word:
        words.append(''.join(word))
    return words


def _unquote(text: str) -> str:
    """A shell word without its surrounding quotes, for a terminator comparison."""
    if len(text) >= 2 and text[0] in '"\'' and text[-1] == text[0]:
        return text[1:-1]
    return text


HEREDOC_WORD_RE = re.compile(r'\d*<<(-?)(.+)')


def _heredocs(joined: str) -> list[tuple[bool, str]]:
    """The heredocs one logical instruction opens, with Docker's full delimiters.

    Docker opens a heredoc only for a word that starts with `<<` (an optional
    file-descriptor number may precede it), so `1<<2`, `$((1<<2))` and
    `a<<EOF` open nothing, while `<<END-MARKER` reads its delimiter to the end:
    truncating it to `END` would end the body early and turn a body line into an
    instruction. `<<<` is a here-string, not a heredoc.
    """
    found: list[tuple[bool, str]] = []
    for word in _shell_words(joined):
        match = HEREDOC_WORD_RE.fullmatch(word)
        if match is None:
            continue
        delimiter = _unquote(match.group(2))
        if not delimiter or delimiter.startswith('<'):
            continue
        found.append((match.group(1) == '-', delimiter))
    return found


def scan_instructions(text: str, *, path: str) -> tuple[Instruction, ...]:
    """Split a Dockerfile into instructions, honouring continuations and heredocs.

    Not a general Dockerfile parser: it only has to be right about where one
    instruction ends, because the request form is one single-line LABEL.
    """
    lines = text.split('\n')
    _check_escape_directive(lines, path)
    instructions: list[Instruction] = []
    index = 0
    while index < len(lines):
        stripped = lines[index].strip()
        if not stripped or stripped.startswith('#'):
            index += 1
            continue
        start = index
        own = [lines[index]]
        while _continues(own):
            index += 1
            if index >= len(lines):
                raise RequestError(f'{path}:{start + 1}: instruction continued past '
                                   f'the end of the file')
            own.append(lines[index])
        joined = _joined_text(own)
        # Docker lexes the logical instruction, not each physical line, so a
        # delimiter split across a continuation and a `<<` inside a quote that
        # spans one are both read the way the daemon reads them.
        heredocs = _heredocs(joined)
        body: list[str] = []
        for strip_tabs, terminator in heredocs:
            while True:
                index += 1
                if index >= len(lines):
                    raise RequestError(f'{path}:{start + 1}: heredoc {terminator} is '
                                       f'never terminated')
                candidate = lines[index]
                own.append(candidate)
                comparison = candidate.lstrip('\t') if strip_tabs else candidate
                if comparison.rstrip('\r') == terminator:
                    break
                body.append(candidate)
        instructions.append(Instruction(keyword=stripped.split(None, 1)[0].upper(),
                                       text=lines[start],
                                       line=start + 1,
                                       lines=tuple(own),
                                       joined=joined,
                                       heredoc_body=tuple(body)))
        index += 1
    return tuple(instructions)


def _display_path(dockerfile: Path, root: Path) -> str:
    try:
        return dockerfile.resolve().relative_to(root.resolve()).as_posix()
    except ValueError:
        return str(dockerfile)


def source_labels(dockerfile: Path, *, root: Path) -> tuple[SourceLabel, ...]:
    """The request labels one recipe declares, in file order."""
    display = _display_path(dockerfile, root)
    instructions = scan_instructions(read_source(dockerfile), path=display)
    last_from = None
    for position, instruction in enumerate(instructions):
        if instruction.keyword == 'FROM':
            last_from = position
    if last_from is None:
        raise RequestError(f'{display}: no FROM instruction')
    problems: list[str] = []
    found: list[SourceLabel] = []
    seen: dict[RequestKey, int] = {}
    for position, instruction in enumerate(instructions):
        strict = (instruction.text if instruction.keyword == 'LABEL'
                  and len(instruction.lines) == 1 else None)
        match = LABEL_RE.fullmatch(strict) if strict is not None else None
        if match is not None:
            if position < last_from:
                problems.append(f'{display}:{instruction.line}: request label in a '
                                f'builder stage; it must be after the last FROM')
                continue
            try:
                key = parse_key(match.group(1))
                value = parse_value(match.group(2))
            except RequestError as error:
                problems.append(f'{display}:{instruction.line}: {error}')
                continue
            if key in seen:
                problems.append(f'{display}:{instruction.line}: duplicate key '
                                f'{key.label()} (first at line {seen[key]})')
                continue
            seen[key] = instruction.line
            found.append(SourceLabel(key, value, display, instruction.line))
            continue
        culprit = next((line for line in (instruction.joined,) + instruction.lines
                        if TYPO_GUARD in line), None)
        if culprit is not None:
            problems.append(f'{display}:{instruction.line}: unsupported form for a '
                            f'network-request key: {culprit.strip()}')
    if problems:
        raise RequestError('\n'.join(problems))
    return tuple(found)


# --- image inspection --------------------------------------------------------
def _json_type(value: object) -> str:
    if isinstance(value, bool):
        return 'boolean'
    if isinstance(value, (int, float)):
        return 'number'
    if isinstance(value, str):
        return 'string'
    if isinstance(value, list):
        return 'array'
    if isinstance(value, dict):
        return 'object'
    return 'null'


def read_inspect(path: Path, *, display: str | None = None) -> InspectedImage:
    """`docker image inspect` output, converted to a type at the boundary."""
    shown = display or str(path)
    try:
        raw = path.read_bytes()
    except OSError as error:
        raise RequestError(f'{shown}: cannot read: {error.strerror or error}')
    try:
        data = json.loads(raw)
    except UnicodeDecodeError as error:
        raise RequestError(f'{shown}: not UTF-8 at byte {error.start} ({error.reason})')
    except json.JSONDecodeError as error:
        raise RequestError(f'{shown}: not JSON: {error.msg} at line {error.lineno} '
                           f'column {error.colno}')
    if not isinstance(data, list) or len(data) != 1:
        raise RequestError(f'{shown}: expected a one-element array from docker image '
                           f'inspect, got {_json_type(data)}'
                           + (f' of {len(data)}' if isinstance(data, list) else ''))
    element = data[0]
    if not isinstance(element, dict):
        raise RequestError(f'{shown}: .[0] is a {_json_type(element)}, expected an object')
    config = element.get('Config')
    if not isinstance(config, dict):
        raise RequestError(f'{shown}: .[0].Config is missing or a '
                           f'{_json_type(config)}, expected an object')
    labels = config.get('Labels')
    if labels is None:
        return InspectedImage(MappingProxyType({}))
    if not isinstance(labels, dict):
        raise RequestError(f'{shown}: .[0].Config.Labels is a {_json_type(labels)}, '
                           f'expected an object or null')
    converted: dict[str, str] = {}
    for key, value in labels.items():
        if not isinstance(key, str):
            raise RequestError(f'{shown}: .[0].Config.Labels has a non-string key')
        if not isinstance(value, str):
            raise RequestError(f'{shown}: .[0].Config.Labels[{json.dumps(key)}] is a '
                               f'{_json_type(value)}, expected a string')
        converted[key] = value
    return InspectedImage(MappingProxyType(converted))


# --- ownership ----------------------------------------------------------------
def _only(allowed_key: RequestKey) -> Callable[[RequestKey], bool]:
    return lambda key: key == allowed_key


def _layer_name(relative: str) -> str | None:
    parts = relative.split('/')
    if len(parts) != 4 or parts[0] != 'examples' or parts[1] != 'layers':
        return None
    if parts[3] != 'Dockerfile' or not NAME_RE.fullmatch(parts[2]):
        return None
    return parts[2]


def owner_rule(relative: str, root: Path) -> Ownership:
    """Which keys this recipe owns. A path with no row may declare nothing."""
    if relative == 'images/Dockerfile':
        base = RequestKey(Scope.IMAGE, 'base')
        return Ownership(_only(base), (base,))
    if relative == 'images/standard/Dockerfile':
        tools = root / 'images' / 'tools'

        def allowed(key: RequestKey) -> bool:
            # An agent exists only where its own context directory does, so a
            # stale key cannot outlive a removed agent.
            return key.scope is Scope.AGENT and (tools / key.component).is_dir()

        return Ownership(allowed, ())
    layer = _layer_name(relative)
    if layer is not None:
        own = RequestKey(Scope.IMAGE, layer)
        return Ownership(_only(own), (own,))
    return Ownership(lambda key: False, ())


def _is_dockerfile_name(name: str) -> bool:
    return name == 'Dockerfile' or name.endswith('.Dockerfile')


def _walk_dockerfiles(root: Path) -> Iterable[Path]:
    """Every recipe in the tree, not just the three roots that own keys today.

    Ownership is a rule about all recipes -- one outside the known roots may
    declare nothing -- so a recipe the walk misses is a recipe that can declare
    anything. `os.walk` prunes scratch directories as it descends, which also
    keeps it away from `target/`'s build output.
    """
    for directory, subdirectories, filenames in os.walk(root):
        subdirectories[:] = sorted(name for name in subdirectories
                                   if name not in SCRATCH_DIRECTORIES)
        for name in sorted(filenames):
            if _is_dockerfile_name(name):
                yield Path(directory) / name


@dataclass(frozen=True)
class SourceReport:
    problems: tuple[str, ...]
    labels: int
    dockerfiles: int

    def summary(self) -> str:
        return (f'network requests: {self.labels} labels in {self.dockerfiles} '
                f'Dockerfiles ok')


def check_sources(root: Path) -> SourceReport:
    """Every problem with the recipes' request labels, and the counts for a log line."""
    problems: list[str] = []
    count = 0
    files = 0
    for path in _walk_dockerfiles(root):
        relative = path.relative_to(root).as_posix()
        files += 1
        if path.is_symlink():
            problems.append(f'{relative}: symlinked Dockerfile is not checked')
            continue
        ownership = owner_rule(relative, root)
        try:
            labels = source_labels(path, root=root)
        except RequestError as error:
            problems.extend(str(error).splitlines())
            continue
        declared: set[RequestKey] = set()
        for label in labels:
            count += 1
            declared.add(label.key)
            if not ownership.allowed(label.key):
                problems.append(f'{relative}:{label.line}: {label.key.label()} is not '
                                f'this component\'s key')
        for required in ownership.required:
            if required not in declared:
                problems.append(f'{relative}: missing required key {required.label()}')
    return SourceReport(tuple(problems), count, files)


# --- built images -------------------------------------------------------------
def _format(key: RequestKey, entries: RequestList | None) -> str:
    return '<absent>' if entries is None else f'"{entries}"'


def check_image(actual: InspectedImage, *, sources: Sequence[Path],
                inherits: InspectedImage | None, root: Path) -> list[str]:
    """Actual labels must equal exactly what the sources and the parent imply."""
    problems: list[str] = []
    try:
        actual_declared: dict[RequestKey, RequestList] = dict(labels_from_mapping(actual.labels))
    except RequestError as error:
        problems.extend(f'image: {line}' for line in str(error).splitlines())
        actual_declared = {}
    expected: dict[RequestKey, RequestList] = {}
    if inherits is not None:
        try:
            expected.update(labels_from_mapping(inherits.labels))
        except RequestError as error:
            problems.extend(f'parent: {line}' for line in str(error).splitlines())
    for path in sources:
        try:
            labels = source_labels(path, root=root)
        except RequestError as error:
            problems.extend(str(error).splitlines())
            continue
        for label in labels:
            if label.key in expected:
                problems.append(f'{label.path}:{label.line}: a layer would overwrite '
                                f'an inherited request for {label.key.label()}')
                continue
            expected[label.key] = label.value
    for key, value in expected.items():
        if key not in actual_declared:
            problems.append(f'{key.label()} did not reach the final stage '
                            f'(expected "{value}")')
        elif actual_declared[key] != value:
            problems.append(f'{key.label()} is "{actual_declared[key]}" but the sources '
                            f'and parent imply "{value}"')
    for key, value in actual_declared.items():
        if key not in expected:
            problems.append(f'{key.label()} is "{value}" and is not declared by the '
                            f'sources or the parent')
    return problems


def egress_config(image: InspectedImage, key: RequestKey) -> dict:
    """The addon config for a smoke whose egress is limited to one label."""
    declared = labels_from_mapping(image.labels)
    if key not in declared:
        raise RequestError(f'{key.label()}: not inventoried; there is nothing to smoke')
    entries = declared[key].entries
    if not entries:
        raise RequestError(f'{key.label()}: the value is empty; there is nothing to smoke')
    for entry in entries:
        if entry.port is not None:
            raise RequestError(f'{key.label()}: the smoke supports 443-only requests, '
                               f'but {entry} names a port')
    return {
        'allow': [{'host': entry.host, 'path': '^/'} for entry in entries],
        'https_only': True,
        'learn_redirects': False,
        # Opt-in addon mode (ADR 0014): CONNECT authority, SNI, Host and the
        # upstream address must all name the same declared host.
        'enforce_tls_destination': True,
    }


# --- CLI ----------------------------------------------------------------------
def _repository_root() -> Path:
    return Path(__file__).resolve().parents[2]


def _run(arguments: argparse.Namespace) -> int:
    if arguments.verb == 'check-sources':
        report = check_sources(Path(arguments.root).resolve())
        if report.problems:
            for problem in report.problems:
                sys.stderr.write(f'{problem}\n')
            return 1
        print(report.summary())
        return 0
    if arguments.verb == 'source-labels':
        root = Path(arguments.root).resolve()
        labels = source_labels(Path(arguments.dockerfile), root=root)
        print(json.dumps({label.key.label(): str(label.value) for label in labels},
                         indent=2, sort_keys=True))
        return 0
    if arguments.verb == 'check-image':
        root = Path(arguments.root).resolve()
        actual = read_inspect(Path(arguments.inspect))
        inherits = read_inspect(Path(arguments.inherits)) if arguments.inherits else None
        problems = check_image(actual, sources=[Path(path) for path in arguments.dockerfile],
                               inherits=inherits, root=root)
        if problems:
            for problem in problems:
                sys.stderr.write(f'{problem}\n')
            return 1
        print('network requests: image labels match the sources and the parent')
        return 0
    if arguments.verb == 'egress-config':
        config = egress_config(read_inspect(Path(arguments.inspect)), parse_key(arguments.key))
        print(json.dumps(config, indent=2))
        return 0
    raise AssertionError(f'unhandled verb {arguments.verb}')


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog='network_requests.py', description=__doc__.splitlines()[0])
    parser.add_argument('--root', default=str(_repository_root()),
                        help='repository root the paths are relative to')
    subparsers = parser.add_subparsers(dest='verb', required=True)
    subparsers.add_parser('check-sources', help='check every recipe in the tree')
    source = subparsers.add_parser('source-labels', help='print one recipe\'s labels')
    source.add_argument('dockerfile')
    image = subparsers.add_parser('check-image', help='compare a built image')
    image.add_argument('--inspect', required=True)
    image.add_argument('--dockerfile', action='append', default=[])
    image.add_argument('--inherits')
    config = subparsers.add_parser('egress-config', help='addon config for one label')
    config.add_argument('--inspect', required=True)
    config.add_argument('--key', required=True)
    arguments = parser.parse_args(argv)
    try:
        return _run(arguments)
    except RequestError as error:
        for line in str(error).splitlines():
            sys.stderr.write(f'network-requests: {line}\n')
        return 1


if __name__ == '__main__':
    sys.exit(main())
