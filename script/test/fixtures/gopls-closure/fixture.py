#!/usr/bin/env python3
"""Synthetic module closure for the gopls-closure fixtures.

The modules here exist only for the hermetic suites. They are served by
``proxy.py`` and the canonical candidate record is produced by the *real*
maintenance stager, so the checker, the stager and the build agree on one model
instead of on a hand-written expectation.

Two deliberate fixture-only properties, both labelled everywhere they appear:

* resolution goes through a loopback proxy and ``sumdb='off'`` because synthetic
  modules are not in the real transparency log, and
* the emitted record still names the production proxy and verifier key, so the
  strict checker consumes exactly what production emits. These records are not
  evidence of real checksum-database authentication and are never published.
"""
from __future__ import annotations

import importlib.util
import io
import sys
import zipfile
from dataclasses import dataclass
from pathlib import Path

sys.dont_write_bytecode = True
REPOSITORY_ROOT = Path(__file__).resolve().parents[4]
MODEL_DIRECTORY = REPOSITORY_ROOT / 'examples' / 'layers' / 'go-dev'
for _path in (Path(__file__).resolve().parent, MODEL_DIRECTORY):
    if str(_path) not in sys.path:
        sys.path.insert(0, str(_path))

import gopls_closure as model  # noqa: E402
import proxy as proxy_module  # noqa: E402
from executables import go_executables  # noqa: E402,F401 - re-exported for the suites

PINNED_GO = '1.27.3'
# The language version every synthetic module declares. It is not decoration: a
# main module's ``go`` line decides the ``DefaultGODEBUG`` the compiler records, so
# a suite that probes build settings has to carry the same directive rather than a
# copy of the number. It matches the committed closure's own ``go`` line.
ROOT_GO_LANGUAGE = '1.26.0'
ROOT = 'golang.org/x/tools/gopls'
DEP = 'example.test/dep'
LEAF = 'example.test/leaf'
TESTONLY = 'example.test/testonly'
# A genuinely uppercase module path: Go escapes it in the module cache and in
# proxy URLs, so only this spelling exercises that rule.
UPPER = 'example.test/UPPER'
EVIL = 'example.test/evil'
REVIEWED_ON = '2026-01-02'
REVIEWER = 'fixture-reviewer'


def load_module(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    previous = sys.dont_write_bytecode
    sys.dont_write_bytecode = True
    try:
        spec.loader.exec_module(module)
    finally:
        sys.dont_write_bytecode = previous
    return module


updater = load_module('gopls_closure_update', REPOSITORY_ROOT / 'script' / 'dev'
                      / 'gopls-closure-update.py')


def module_zip(path: str, version: str, files: dict[str, bytes]) -> bytes:
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, 'w', zipfile.ZIP_STORED) as archive:
        for name in sorted(files):
            archive.writestr(f'{path}@{version}/{name}', files[name])
    return buffer.getvalue()


def go_mod(path: str, requires: dict[str, str] | None = None, *,
           extra: str = '') -> bytes:
    lines = [f'module {path}', '', f'go {ROOT_GO_LANGUAGE}']
    if requires:
        lines.extend(['', 'require ('])
        for required in sorted(requires):
            lines.append(f'\t{required} {requires[required]}')
        lines.append(')')
    text = '\n'.join(lines) + '\n' + extra
    return text.encode()


def dep_source() -> bytes:
    return (f'package dep\n\nimport "{LEAF}"\n\n'
            f'func Name() string {{ return leaf.Name() }}\n').encode()


def root_source() -> bytes:
    return (f'package main\n\nimport (\n\t"fmt"\n\n\t"{DEP}"\n\t"{UPPER}"\n)\n\n'
            f'func main() {{ fmt.Println(dep.Name(), uppercase.Name()) }}\n').encode()


def modules() -> dict[tuple[str, str], dict[str, bytes]]:
    return {
        (LEAF, 'v1.0.0'): {
            'go.mod': go_mod(LEAF),
            'leaf.go': b'package leaf\n\nfunc Name() string { return "leaf" }\n',
            'LICENSE': b'leaf license text\n',
        },
        (DEP, 'v1.0.0'): {
            'go.mod': go_mod(DEP, {LEAF: 'v1.0.0'}),
            'dep.go': dep_source(),
            'LICENSE': b'dep license text\n',
        },
        (DEP, 'v1.1.0'): {
            'go.mod': go_mod(DEP, {LEAF: 'v1.0.0'}),
            'dep.go': dep_source(),
            'LICENSE': b'dep license text\n',
        },
        # A dependency's own toolchain/replace directives are ignored by the main
        # module's resolution; the closure must still select dep and leaf only.
        (DEP, 'v1.2.0'): {
            'go.mod': go_mod(DEP, {LEAF: 'v1.0.0'},
                             extra=f'\ntoolchain go1.99.0\n\nreplace {LEAF} => {EVIL} v1.0.0\n'),
            'dep.go': dep_source(),
            'LICENSE': b'dep license text\n',
        },
        # An uppercase module path exercises the module-cache escaping rules.
        (UPPER, 'v1.0.0'): {
            'go.mod': go_mod(UPPER),
            'upper.go': b'package uppercase\n\nfunc Name() string { return "upper" }\n',
            'LICENSE': b'uppercase license text\n',
        },
        (TESTONLY, 'v1.0.0'): {
            'go.mod': go_mod(TESTONLY),
            'doc.go': b'package testonly\n',
            'LICENSE': b'testonly license text\n',
        },
        (ROOT, 'v0.0.1'): {
            'go.mod': go_mod(ROOT, {DEP: 'v1.0.0', LEAF: 'v1.0.0', UPPER: 'v1.0.0',
                                    TESTONLY: 'v1.0.0'}),
            'main.go': root_source(),
            'LICENSE': b'root license text\n',
        },
        # The root does not require leaf, though Go still selects it through dep:
        # the reviewed current-pin adoption restriction must refuse this.
        (ROOT, 'v0.0.2'): {
            'go.mod': go_mod(ROOT, {DEP: 'v1.0.0', UPPER: 'v1.0.0'}),
            'main.go': root_source(),
            'LICENSE': b'root license text\n',
        },
        # A coherent alternative selection: the graph-change control sources its
        # dep v1.1.0 sums from a stage that legitimately selects them.
        (ROOT, 'v0.0.4'): {
            'go.mod': go_mod(ROOT, {DEP: 'v1.1.0', LEAF: 'v1.0.0', UPPER: 'v1.0.0'}),
            'main.go': root_source(),
            'LICENSE': b'root license text\n',
        },
        # A dep with no root notice: evidence transport refuses an unfamiliar
        # licence layout instead of blessing it.
        (DEP, 'v1.3.0'): {
            'go.mod': go_mod(DEP, {LEAF: 'v1.0.0'}),
            'dep.go': dep_source(),
        },
        (ROOT, 'v0.0.5'): {
            'go.mod': go_mod(ROOT, {DEP: 'v1.3.0', LEAF: 'v1.0.0', UPPER: 'v1.0.0'}),
            'main.go': root_source(),
            'LICENSE': b'root license text\n',
        },
        # A dep whose only notice is nested: an unreviewed nested layout.
        (DEP, 'v1.4.0'): {
            'go.mod': go_mod(DEP, {LEAF: 'v1.0.0'}),
            'dep.go': dep_source(),
            'sub/LICENSE': b'nested dep license text\n',
        },
        (ROOT, 'v0.0.6'): {
            'go.mod': go_mod(ROOT, {DEP: 'v1.4.0', LEAF: 'v1.0.0', UPPER: 'v1.0.0'}),
            'main.go': root_source(),
            'LICENSE': b'root license text\n',
        },
        # A selected module whose path is in the reviewed signature inventory at
        # another version: the inventory needs review rather than being dropped.
        ('github.com/fsnotify/fsnotify', 'v1.8.0'): {
            'go.mod': go_mod('github.com/fsnotify/fsnotify'),
            'fsnotify.go': b'package fsnotify\n\nfunc Name() string { return "fsnotify" }\n',
            'LICENSE': b'fsnotify license text\n',
        },
        (ROOT, 'v0.0.7'): {
            'go.mod': go_mod(ROOT, {'github.com/fsnotify/fsnotify': 'v1.8.0',
                                    LEAF: 'v1.0.0', UPPER: 'v1.0.0'}),
            'main.go': (f'package main\n\nimport (\n\t"fmt"\n\n'
                        f'\t"github.com/fsnotify/fsnotify"\n)\n\n'
                        f'func main() {{ fmt.Println(fsnotify.Name()) }}\n').encode(),
            'LICENSE': b'root license text\n',
        },
        (ROOT, 'v0.0.3'): {
            'go.mod': go_mod(ROOT, {DEP: 'v1.2.0', LEAF: 'v1.0.0', UPPER: 'v1.0.0'}),
            'main.go': root_source(),
            'LICENSE': b'root license text\n',
        },
    }


@dataclass
class Fixture:
    workspace: Path
    go_root: Path
    proxy: proxy_module.Proxy
    dockerfile: Path

    def environment(self) -> 'updater.MaintenanceEnvironment':
        return updater.MaintenanceEnvironment(
            proxy=self.proxy.url(), sumdb='off', toolchain=f'go{PINNED_GO}',
            host_go=self.go_root / 'bin' / 'go', transport={}, in_github_actions=False)

    def build_environment(self) -> model.GoEnvironment:
        return model.GoEnvironment(self.proxy.url(), 'off')

    def stage(self, gopls: str = 'v0.0.1', *, name: str | None = None,
              current: Path | None = None, reviewed_on: str | None = REVIEWED_ON,
              reviewer: str | None = REVIEWER) -> Path:
        """Stage a candidate through the real stager; return its data directory."""
        out = self.workspace / (name or f'stage-{gopls}')
        request = updater.StageRequest(
            gopls=model.ReleaseVersion(gopls), go=model.GoVersion(PINNED_GO),
            current=current or (self.workspace / 'no-current'), out=out,
            reviewed_on=(model.IsoDate(reviewed_on) if reviewed_on else None),
            reviewer=reviewer)
        updater.stage(request, self.environment(), dockerfile=self.dockerfile)
        return out / 'gopls-closure'


def build(workspace: Path, go_root: Path) -> Fixture:
    workspace.mkdir(parents=True, exist_ok=True)
    proxy = proxy_module.Proxy()
    for (path, version), files in modules().items():
        # The proxy speaks escaped paths, exactly as the Go command asks for them.
        proxy.add_module(model.escape_module(path), version,
                         zip_bytes=module_zip(path, version, files),
                         mod_bytes=files['go.mod'])
    proxy.add_list(model.escape_module(ROOT),
                   ['v0.0.1', 'v0.0.2', 'v0.0.3', 'v0.0.4', 'v0.0.5', 'v0.0.6', 'v0.0.7'])
    dockerfile = workspace / 'Dockerfile'
    dockerfile.write_text(f'ARG GO_VERSION={PINNED_GO}\nARG GOPLS_VERSION=v0.0.1\n'
                          'FROM fixture\n')
    return Fixture(workspace=workspace, go_root=go_root, proxy=proxy, dockerfile=dockerfile)


def digest_tree(directory: Path) -> dict[str, str]:
    import hashlib

    if not directory.exists():
        return {}
    return {path.relative_to(directory).as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in sorted(directory.rglob('*')) if path.is_file()}
