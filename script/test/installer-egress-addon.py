#!/usr/bin/env python3
"""Hermetic replay of the restricted-egress addon policy (#97 review S8).

`script/test/shipped-installer-network.sh` runs the real addon inside the pinned
mitmproxy image, which needs Docker and the network. That is the acceptance run.
This table is the fast one: it loads the SAME `addon.py`, with a stub `mitmproxy`
package standing in for the framework, and replays request/response pairs. The
allowlist config is not copied here either -- it is asked of the gate itself,
`shipped-installer-network.sh --emit-config codex`, so a hermetic refusal is a
refusal of the configuration the proxy would actually be handed.

The cases that matter are the redirect ones. A learned target used to skip the
whole HTTPS shape check, so an allowed response pointing at `http://`, at a
credentialed URL, at another port or at an unrelated host bought that target an
exemption from the policy. Every one of those is a case below.

usage: python3 script/test/installer-egress-addon.py [--platform linux/arm64]
"""
from __future__ import annotations

import argparse
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import types
from pathlib import Path
from urllib.parse import urlsplit

REPO_ROOT = Path(__file__).resolve().parents[2]
GATE = REPO_ROOT / 'script' / 'test' / 'shipped-installer-network.sh'
ADDON = REPO_ROOT / 'script' / 'test' / 'fixtures' / 'installer-egress' / 'addon.py'

RELEASE = 'rust-v0.159.3'
CDN = ('https://release-assets.githubusercontent.com/github-production-release-asset'
       '/x/y.tar.gz?sig=REVIEW-SIGNED-SECRET&token=REVIEW-TOKEN')
PAYLOAD = ''          # derived from the production config, see payload_for

failures: list[str] = []


def report(label: str) -> None:
    print(f'ok: {label}')


def fail(label: str, detail: object) -> None:
    failures.append(f'{label}: {detail}')
    print(f'FAIL: {label}: {detail}', file=sys.stderr)


# --- a stub `mitmproxy` package ----------------------------------------------

class StubResponse:
    def __init__(self, status_code: int, content: bytes = b'', headers=None) -> None:
        self.status_code = status_code
        self.content = content
        self.headers = dict(headers or {})

    @classmethod
    def make(cls, status_code, content=b'', headers=None):
        return cls(status_code, content, headers)


def install_stub() -> None:
    http = types.ModuleType('mitmproxy.http')
    http.Response = StubResponse
    package = types.ModuleType('mitmproxy')
    package.http = http
    sys.modules['mitmproxy'] = package
    sys.modules['mitmproxy.http'] = http


# --- flows, in the shape the addon reads -------------------------------------

class StubRequest:
    def __init__(self, url: str, method: str = 'GET') -> None:
        parts = urlsplit(url)
        self.pretty_url = url
        self.pretty_host = parts.hostname
        self.path = parts.path + (f'?{parts.query}' if parts.query else '')
        self.method = method


class StubFlow:
    def __init__(self, url: str, method: str = 'GET') -> None:
        self.request = StubRequest(url, method)
        self.response = None

    def redirect(self, location: str, status: int = 302) -> None:
        self.response = StubResponse.make(status, b'', {'location': location})


_loaded = [0]


def load_addon(config: Path, log: Path) -> object:
    _loaded[0] += 1
    os.environ['EGRESS_CONFIG'] = str(config)
    os.environ['EGRESS_LOG'] = str(log)
    os.environ['EGRESS_MODE'] = 'strict'
    spec = importlib.util.spec_from_file_location(f'egress_addon_{_loaded[0]}', ADDON)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    name = spec.name
    sys.modules[name] = module
    try:
        spec.loader.exec_module(module)
    except BaseException:
        sys.modules.pop(name, None)
        raise
    return module


def payload_for(config: dict) -> str:
    """The exact URL the production config allows, read off the config itself.

    Deriving it here is what keeps this table honest across platforms: the
    allowlist path is a regex over the arch and the release, so a hard-coded
    x86_64 URL would be testing the table, not the policy.
    """
    for rule in config['allow']:
        if rule['host'] == 'github.com':
            path = rule['path'].replace('^', '').replace('$', '')
            return 'https://github.com' + path.replace('\\.', '.')
    raise SystemExit('the codex allowlist has no github.com rule')


def gate_config(platform: str) -> dict:
    finished = subprocess.run(['bash', str(GATE), '--emit-config', 'codex',
                               '--platform', platform],
                              capture_output=True, text=True)
    if finished.returncode != 0:
        raise SystemExit(f'shipped-installer-network.sh --emit-config failed: '
                         f'{finished.stderr.strip()}')
    return json.loads(finished.stdout)


# --- cases -------------------------------------------------------------------

def case_payload_is_allowed(addon, work: Path) -> None:
    label = 'payload_request_allowed'
    flow = StubFlow(PAYLOAD)
    addon.request(flow)
    if flow.response is not None:
        fail(label, f'refused with {flow.response.status_code}')
        return
    log = (work / 'egress.log').read_text()
    expected = f'ALLOW https://github.com/openai/codex/releases/download/{RELEASE}/'
    if expected not in log:
        fail(label, f'the log line is not the format the stream assertion reads: {log!r}')
    else:
        report(label)


def case_shape_negatives(addon, work: Path) -> None:
    for name, url in [
            ('http', PAYLOAD.replace('https://', 'http://')),
            ('port', PAYLOAD.replace('github.com', 'github.com:444')),
            ('query', PAYLOAD + '?sig=REVIEW-SIGNED-SECRET'),
            ('credentials', PAYLOAD.replace('https://', 'https://u:p@'))]:
        label = f'payload_request_refused/{name}'
        flow = StubFlow(url)
        addon.request(flow)
        if flow.response is None or flow.response.status_code != 403:
            fail(label, 'accepted')
        else:
            report(label)


def case_redirect_followed(addon, work: Path) -> None:
    label = 'learned_redirect_is_followed'
    redirect = StubFlow(PAYLOAD)
    redirect.redirect(CDN)
    addon.request(redirect)
    addon.response(redirect)
    follow = StubFlow(CDN)
    addon.request(follow)
    log = (work / 'egress.log').read_text()
    if redirect.response is not None and redirect.response.status_code == 403:
        fail(label, 'the allowed response to the payload was refused')
    elif 'LEARN https://release-assets.githubusercontent.com/' not in log:
        fail(label, f'the CDN target was not learned: {log!r}')
    elif follow.response is not None:
        fail(label, 'the learned target was refused')
    elif 'ALLOW-LEARNED https://release-assets.githubusercontent.com/' not in log:
        fail(label, f'the follow-up is not logged as learned: {log!r}')
    elif 'REVIEW-SIGNED-SECRET' in log or 'REVIEW-TOKEN' in log:
        fail(label, 'the log leaked a signed query value')
    else:
        report(label)


def case_redirect_negatives(addon, work: Path) -> None:
    """Only the signed query is exempt; every other shape rule still applies."""
    for name, location in [
            ('http', CDN.replace('https://', 'http://')),
            ('credentials', CDN.replace('https://', 'https://u:p@')),
            ('port', CDN.replace('release-assets.githubusercontent.com',
                                 'release-assets.githubusercontent.com:444')),
            ('other_origin', CDN.replace('release-assets.githubusercontent.com',
                                         'evil.example')),
            ('repo_origin', CDN.replace('release-assets.githubusercontent.com',
                                        'github.com'))]:
        label = f'redirect_target_refused/{name}'
        # A fresh addon per case: learning is per-proxy-process state.
        log = work / f'redirect-{name}.log'
        scenario = load_addon(work / 'config.json', log)
        redirect = StubFlow(PAYLOAD)
        redirect.redirect(location)
        scenario.request(redirect)
        scenario.response(redirect)
        follow = StubFlow(location)
        scenario.request(follow)
        text = log.read_text()
        if redirect.response is None or redirect.response.status_code != 403:
            fail(label, 'an unlearnable redirect target was accepted')
        elif 'DENY redirect-target-refused' not in text:
            fail(label, f'no refusal was logged: {text!r}')
        elif follow.response is None or follow.response.status_code != 403:
            fail(label, 'the refused target was still requestable')
        else:
            report(label)


def case_deny_patterns_apply_to_learned(addon, work: Path) -> None:
    label = 'deny_pattern_wins_over_learned'
    log = work / 'deny-learned.log'
    scenario = load_addon(work / 'config.json', log)
    target = CDN.replace('/x/y.tar.gz', '/releases/latest/y.tar.gz')
    redirect = StubFlow(PAYLOAD)
    redirect.redirect(target)
    scenario.request(redirect)
    scenario.response(redirect)
    follow = StubFlow(target)
    scenario.request(follow)
    text = log.read_text()
    if follow.response is None or follow.response.status_code != 403:
        fail(label, 'a denied path was requestable after the redirect')
    elif 'DENY deny-pattern' not in text:
        fail(label, f'the deny pattern was not applied to the learned target: {text!r}')
    elif 'ALLOW-LEARNED' in text:
        fail(label, f'a learned URL bypassed a deny pattern: {text!r}')
    else:
        report(label)


def case_unlisted_is_denied(addon, work: Path) -> None:
    label = 'unlisted_request_denied'
    flow = StubFlow('https://api.github.com/repos/openai/codex/releases/latest')
    addon.request(flow)
    if flow.response is None or flow.response.status_code != 403:
        fail(label, 'an unlisted URL was accepted')
    else:
        report(label)


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument('--platform', default='linux/arm64')
    arguments = parser.parse_args(argv)
    global PAYLOAD
    config = gate_config(arguments.platform)
    PAYLOAD = payload_for(config)
    if config.get('https_only') is not True:
        print('FAIL: the codex allowlist config is not https_only', file=sys.stderr)
        return 1
    if not config.get('learn_hosts'):
        print('FAIL: the codex allowlist config declares no learn_hosts',
              file=sys.stderr)
        return 1
    install_stub()
    with tempfile.TemporaryDirectory(prefix='installer-egress-addon.') as scratch:
        work = Path(scratch)
        (work / 'config.json').write_text(json.dumps(config) + '\n')
        log = work / 'egress.log'
        addon = load_addon(work / 'config.json', log)
        case_payload_is_allowed(addon, work)
        case_shape_negatives(addon, work)
        case_redirect_followed(addon, work)
        case_redirect_negatives(addon, work)
        case_deny_patterns_apply_to_learned(addon, work)
        case_unlisted_is_denied(addon, work)
    if failures:
        print(f'installer-egress-addon: {len(failures)} case(s) failed', file=sys.stderr)
        return 1
    print('installer-egress-addon: all cases passed')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
