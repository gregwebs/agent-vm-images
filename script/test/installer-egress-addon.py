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


class StubOptions:
    """The one option the addon reads: the connection strategy.

    The smoke passes `--set connection_strategy=lazy`, and that is the strategy a
    declarative-destination gate needs; the eager case sets it explicitly.
    """

    connection_strategy = 'lazy'


def install_stub() -> None:
    http = types.ModuleType('mitmproxy.http')
    http.Response = StubResponse
    package = types.ModuleType('mitmproxy')
    package.http = http
    ctx = types.ModuleType('mitmproxy.ctx')
    ctx.options = StubOptions()
    package.ctx = ctx
    sys.modules['mitmproxy'] = package
    sys.modules['mitmproxy.http'] = http
    sys.modules['mitmproxy.ctx'] = ctx


class StubConn:
    """The parts of a client or server connection the addon reads."""

    def __init__(self, address=None, sni=None) -> None:
        self.address = address
        self.sni = sni
        self.error = None


class StubServerConnectionHookData:
    """The shape mitmproxy 12.2.3 hands `server_connect`/`server_connected`."""

    def __init__(self, address, sni) -> None:
        self.server = StubConn(address, sni)


# --- flows, in the shape the addon reads -------------------------------------

class StubRequest:
    def __init__(self, url: str, method: str = 'GET') -> None:
        parts = urlsplit(url)
        self.pretty_url = url
        self.pretty_host = parts.hostname
        # `host`/`port` are the CONNECT authority in mitmproxy; the stub derives
        # them from the URL, which is the same thing for a correctly built flow.
        self.host = parts.hostname
        self.port = parts.port or 443
        self.path = parts.path + (f'?{parts.query}' if parts.query else '')
        self.method = method


class StubFlow:
    """A flow with the TLS facts the opt-in destination mode compares.

    `sni` and `address` default to the URL's own host, which is what a correct
    client produces: the CONNECT authority, the SNI and the Host all agree.
    """

    def __init__(self, url: str, method: str = 'GET', sni: str | None = None,
                 address: tuple | None = None) -> None:
        self.request = StubRequest(url, method)
        self.response = None
        host = urlsplit(url).hostname
        self.client_conn = StubConn(sni=sni if sni is not None else host)
        self.server_conn = StubConn(address=address if address is not None else (host, 443))
        self.client_conn.address = ('10.0.0.2', 51000)

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


def claude_payload(config: dict) -> str:
    """The one URL the claude config allows, read off the config itself."""
    rules = config.get('allow') or []
    if len(rules) != 1:
        raise SystemExit(f'the claude allowlist must have exactly one rule, '
                         f'found {len(rules)}')
    rule = rules[0]
    path = rule['path'].replace('^', '').replace('$', '')
    return 'https://' + rule['host'] + path.replace('\\.', '.')


def gate_config(platform: str, tool: str = 'codex') -> dict:
    finished = subprocess.run(['bash', str(GATE), '--emit-config', tool,
                               '--platform', platform],
                              capture_output=True, text=True)
    if finished.returncode != 0:
        raise SystemExit(f'shipped-installer-network.sh --emit-config {tool} failed: '
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


def case_agy_profile(platform: str, work: Path) -> None:
    """agy's whole build egress is one committed release asset.

    Its pin-time channels -- the GitHub tag API and Google's updater manifest --
    are inputs a build must never fetch, so they are not in the allowlist at all;
    this row is the hermetic proof that the production config says exactly that.
    """
    label = 'agy_profile'
    config = gate_config(platform, 'agy')
    problems = []
    if config.get('https_only') is not True:
        problems.append('not https_only')
    expected_hosts = ['release-assets.githubusercontent.com',
                      'objects.githubusercontent.com']
    if config.get('learn_hosts') != expected_hosts:
        problems.append(f'learn_hosts {config.get("learn_hosts")!r}')
    payload = payload_for(config)
    if '/google-antigravity/antigravity-cli/releases/download/' not in payload \
            or '/agy_cli_linux_' not in payload:
        problems.append(f'the allowed path is not the agy asset: {payload}')
    if problems:
        fail(label, '; '.join(problems))
        return
    log = work / 'agy.log'
    scenario = load_addon(_write_config(work, 'agy-config.json', config), log)
    allowed = StubFlow(payload)
    scenario.request(allowed)
    if allowed.response is not None:
        fail(label, f'the pinned asset was refused with {allowed.response.status_code}')
        return
    for name, url in [
            ('tag_api', 'https://api.github.com/repos/google-antigravity/antigravity-cli'
                        '/releases/tags/1.4.11'),
            ('manifest', 'https://antigravity-cli-auto-updater-974169037036.us-central1.run.app'
                         '/manifests/linux_amd64.json'),
            ('gcs', 'https://storage.googleapis.com/antigravity-cli/agy.tar.gz'),
            ('musl', payload.replace('.tar.gz', '_musl.tar.gz'))]:
        denied = StubFlow(url)
        scenario.request(denied)
        if denied.response is None or denied.response.status_code != 403:
            fail(f'{label}/{name}', 'accepted')
            return
    report(label)


def case_claude_profile(platform: str, work: Path) -> None:
    """claude's whole build egress is one committed payload.

    The signed manifest, its detached signature and the release key are committed
    evidence read offline, and the native step runs with no network, so none of
    them is in the allowlist at all. `downloads.claude.ai` answers the asset
    directly, so no redirect is learned and every redirect is denied (ADR 0013).
    """
    label = 'claude_profile'
    config = gate_config(platform, 'claude')
    problems = []
    if config.get('https_only') is not True:
        problems.append('not https_only')
    if config.get('learn_redirects') is not False:
        problems.append(f'learn_redirects {config.get("learn_redirects")!r}')
    if config.get('deny_redirects') is not True:
        problems.append(f'deny_redirects {config.get("deny_redirects")!r}')
    if 'learn_hosts' in config:
        problems.append(f'learn_hosts {config.get("learn_hosts")!r} is route learning')
    rules = config.get('allow') or []
    if len(rules) != 1:
        problems.append(f'{len(rules)} allow rules')
    else:
        rule = rules[0]
        if rule.get('host') != 'downloads.claude.ai' or rule.get('method') != 'GET':
            problems.append(f'the allow rule is {rule!r}')
    payload = claude_payload(config)
    if '/claude-code-releases/' not in payload or '/claude' != payload[-7:] \
            or '/manifest' in payload:
        problems.append(f'the allowed path is not the payload: {payload}')
    if problems:
        fail(label, '; '.join(problems))
        return
    log = work / 'claude.log'
    scenario = load_addon(_write_config(work, 'claude-config.json', config), log)
    allowed = StubFlow(payload)
    scenario.request(allowed)
    if allowed.response is not None:
        fail(label, f'the pinned payload was refused with {allowed.response.status_code}')
        return
    release = payload.rsplit('/', 2)[0]
    token = payload.rsplit('/', 2)[1]
    other = 'linux-arm64' if token == 'linux-x64' else 'linux-x64'
    negatives = [
        ('manifest', f'{release}/manifest.json'),
        ('signature', f'{release}/manifest.json.sig'),
        ('zst_manifest', f'{release}/manifest.zst.json'),
        ('raw_sig', f'{release}/manifest.json.raw-sig.json'),
        ('zst_payload', f'{release}/{token}/claude.zst'),
        ('key', 'https://downloads.claude.ai/keys/claude-code.asc'),
        ('other_arch', f'{release}/{other}/claude'),
        ('musl', f'{release}/{token}-musl/claude'),
        ('npm', 'https://registry.npmjs.org/@anthropic-ai/claude-code'),
        ('telemetry', 'https://api.anthropic.com/api/event_logging/v2/batch'),
    ]
    for name, url in negatives:
        denied = StubFlow(url)
        scenario.request(denied)
        if denied.response is None or denied.response.status_code != 403:
            fail(f'{label}/{name}', f'accepted {url}')
            return
    # A redirect on the allowed response is a route change, not a target to learn.
    redirect = StubFlow(payload)
    redirect.redirect('https://release-assets.githubusercontent.com/x/claude')
    scenario.request(redirect)
    scenario.response(redirect)
    text = (work / 'claude.log').read_text()
    if redirect.response is None or redirect.response.status_code != 403:
        fail(f'{label}/redirect', 'an allowed response with a redirect was not blocked')
        return
    if 'DENY redirect' not in text:
        fail(f'{label}/redirect', f'no redirect refusal was logged: {text!r}')
        return
    if 'LEARN' in text:
        fail(f'{label}/redirect', f'a redirect target was learned: {text!r}')
        return
    report(label)


# --- the runtime network-request smoke's config (ADR 0014) --------------------
# The smoke asks `network_requests.py egress-config` for its addon config from the
# built image's label, so this table asks the same module for the config of a
# synthetic inspect file and replays the real addon against it. Every hook the
# opt-in `enforce_tls_destination` mode adds is covered here as policy logic; the
# real-proxy behavior is the runner's DESTINATIONS step.

NETWORK_REQUESTS = REPO_ROOT / 'script' / 'test' / 'network_requests.py'
SMOKE_LABEL_KEY = 'org.agent-vm.network-requests.image.rust-dev'
SMOKE_HOSTS = ['crates.io', 'index.crates.io', 'static.crates.io']


def smoke_config(work: Path) -> dict:
    """The config the smoke would be handed for a rust-dev-shaped image."""
    inspect = work / 'smoke-inspect.json'
    inspect.write_text(json.dumps([{'Config': {'Labels': {
        SMOKE_LABEL_KEY: ','.join(SMOKE_HOSTS)}}}]) + '\n')
    finished = subprocess.run([sys.executable, str(NETWORK_REQUESTS), 'egress-config',
                               '--inspect', str(inspect), '--key', SMOKE_LABEL_KEY],
                              capture_output=True, text=True)
    if finished.returncode != 0:
        raise SystemExit(f'network_requests.py egress-config failed: '
                         f'{finished.stderr.strip()}')
    return json.loads(finished.stdout)


def case_smoke_config(config: dict, work: Path) -> None:
    label = 'network_request_config'
    problems = []
    if config.get('https_only') is not True:
        problems.append('not https_only')
    if config.get('learn_redirects') is not False:
        problems.append(f'learn_redirects {config.get("learn_redirects")!r} '
                        'would let an undeclared CDN through')
    if config.get('enforce_tls_destination') is not True:
        problems.append('enforce_tls_destination is not set')
    if [rule.get('host') for rule in config.get('allow') or []] != SMOKE_HOSTS:
        problems.append(f'allow rules {config.get("allow")!r}')
    if any(rule.get('path') != '^/' for rule in config.get('allow') or []):
        problems.append(f'allow paths {config.get("allow")!r}')
    if problems:
        fail(label, '; '.join(problems))
        return
    report(label)


def case_smoke_policy(addon, work: Path, log: Path) -> None:
    """The declared/undeclared policy the smoke relies on, under the new mode."""
    label = 'network_request_policy'
    allowed = StubFlow('https://index.crates.io/config.json')
    addon.request(allowed)
    if allowed.response is not None:
        fail(f'{label}/declared', f'refused with {allowed.response.status_code}')
        return
    if 'ALLOW https://index.crates.io/config.json' not in log.read_text():
        fail(f'{label}/declared', f'no ALLOW line: {log.read_text()!r}')
        return
    report(f'{label}/declared')
    for name, url, reason in [
            ('unlisted', 'https://example.com/', 'unlisted'),
            ('http', 'http://index.crates.io/', 'https-required'),
            ('port', 'https://index.crates.io:8443/', 'port-443-required'),
            ('query', 'https://index.crates.io/api?q=1', 'no-query-required')]:
        flow = StubFlow(url)
        addon.request(flow)
        if flow.response is None or flow.response.status_code != 403:
            fail(f'{label}/{name}', f'accepted {url}')
        elif f'DENY {reason}' not in log.read_text():
            fail(f'{label}/{name}', f'no DENY {reason}: {log.read_text()!r}')
        else:
            report(f'{label}/{name}')
    # Learning is off in this config: an undeclared redirect target stays
    # unlisted, which is the case the mode exists for.
    redirect = StubFlow('https://index.crates.io/config.json')
    redirect.redirect('https://cdn.example/crates/itoa/1.0.15/download')
    addon.request(redirect)
    addon.response(redirect)
    follow = StubFlow('https://cdn.example/crates/itoa/1.0.15/download')
    addon.request(follow)
    text = log.read_text()
    if 'LEARN' in text or 'ALLOW-LEARNED' in text:
        fail(f'{label}/redirect', f'an undeclared target was learned: {text!r}')
    elif follow.response is None or follow.response.status_code != 403:
        fail(f'{label}/redirect', 'the undeclared redirect target was requestable')
    else:
        report(f'{label}/redirect')


def logged(log: Path) -> str:
    """The log's text; an addon that logged nothing writes no file."""
    return log.read_text() if log.exists() else ''


def case_tls_destination_hooks(work: Path) -> None:
    """Every hook of the opt-in mode, in the shape mitmproxy 12.2.3 calls it."""
    label = 'tls_destination'
    log = work / 'tls-destination.log'
    config = work / 'smoke-config.json'
    addon = load_addon(config, log)
    addon.running()
    if 'DENY' in logged(log):
        fail(f'{label}/running', f'a lazy proxy was refused: {logged(log)!r}')
        return
    report(f'{label}/running')

    listed = StubFlow('https://index.crates.io/config.json')
    addon.http_connect(listed)
    if listed.response is not None:
        fail(f'{label}/connect-declared', 'a declared CONNECT authority was refused')
    else:
        report(f'{label}/connect-declared')

    for name, url, host_reason in [
            ('connect-unlisted', 'https://example.com/', 'connect-unlisted'),
            ('connect-port', 'https://index.crates.io:8443/', 'connect-unlisted')]:
        flow = StubFlow(url)
        addon.http_connect(flow)
        if flow.response is None or flow.response.status_code != 403:
            fail(f'{label}/{name}', f'accepted {url}')
        elif f'DENY {host_reason}' not in logged(log):
            fail(f'{label}/{name}', f'no DENY {host_reason}: {logged(log)!r}')
        else:
            report(f'{label}/{name}')

    # Host, SNI and CONNECT authority must name the same declared host.
    for name, flow, reason in [
            ('authority-mismatch',
             StubFlow('https://example.com/', sni='index.crates.io',
                      address=('index.crates.io', 443)), 'authority-mismatch'),
            ('sni-mismatch', StubFlow('https://index.crates.io/', sni='example.com'),
             'sni-mismatch')]:
        addon.request(flow)
        if flow.response is None or flow.response.status_code != 403:
            fail(f'{label}/{name}', 'accepted')
        elif f'DENY {reason}' not in logged(log):
            fail(f'{label}/{name}', f'no DENY {reason}: {logged(log)!r}')
        else:
            report(f'{label}/{name}')

    for name, address, sni in [
            ('server-unlisted', ('example.com', 443), 'example.com'),
            ('server-port', ('index.crates.io', 8443), 'index.crates.io'),
            ('server-sni', ('index.crates.io', 443), 'example.com')]:
        data = StubServerConnectionHookData(address, sni)
        addon.server_connect(data)
        if data.server.error is None:
            fail(f'{label}/{name}', 'the upstream connection was allowed')
        elif 'DENY server-unlisted' not in logged(log):
            fail(f'{label}/{name}', f'no DENY server-unlisted: {logged(log)!r}')
        else:
            report(f'{label}/{name}')

    connected = StubServerConnectionHookData(('index.crates.io', 443), 'index.crates.io')
    addon.server_connect(connected)
    if connected.server.error is not None:
        fail(f'{label}/server-ok', 'a declared destination was refused')
    else:
        addon.server_connected(connected)
        if 'UPSTREAM https://index.crates.io/' not in logged(log):
            fail(f'{label}/upstream', f'no UPSTREAM line: {logged(log)!r}')
        else:
            report(f'{label}/upstream')

    # The default eager strategy contacts the origin at CONNECT time, before any
    # request hook can compare the Host with it: fail closed instead.
    eager_log = work / 'tls-destination-eager.log'
    eager = load_addon(config, eager_log)
    addon_ctx = sys.modules['mitmproxy.ctx']
    addon_ctx.options.connection_strategy = 'eager'
    try:
        eager.running()
    finally:
        addon_ctx.options.connection_strategy = 'lazy'
    request_flow = StubFlow('https://index.crates.io/config.json')
    eager.request(request_flow)
    data = StubServerConnectionHookData(('index.crates.io', 443), 'index.crates.io')
    eager.server_connect(data)
    text = logged(eager_log)
    if 'DENY eager-connection-strategy' not in text:
        fail(f'{label}/eager', f'no eager refusal was logged: {text!r}')
    elif request_flow.response is None or data.server.error is None:
        fail(f'{label}/eager', 'an eager proxy still allowed a request or a connection')
    else:
        report(f'{label}/eager')


def case_installer_policy_unchanged(platform: str, work: Path) -> None:
    """The gate's own configs never opt in, and its hooks behave as before."""
    for tool in ('codex', 'claude', 'agy'):
        label = f'installer_policy_unchanged/{tool}'
        config = gate_config(platform, tool)
        if 'enforce_tls_destination' in config:
            fail(label, 'the installer config opted into the runtime gate')
            continue
        log = work / f'installer-{tool}.log'
        addon = load_addon(_write_config(work, f'installer-{tool}.json', config), log)
        addon.running()
        flow = StubFlow('https://example.com/', sni='index.crates.io',
                        address=('index.crates.io', 443))
        addon.http_connect(flow)
        data = StubServerConnectionHookData(('example.com', 443), 'example.com')
        addon.server_connect(data)
        addon.server_connected(data)
        text = logged(log)
        if flow.response is not None:
            fail(label, 'http_connect refused a CONNECT for the installer gate')
        elif data.server.error is not None:
            fail(label, 'server_connect killed an upstream for the installer gate')
        elif 'UPSTREAM' in text:
            fail(label, f'the installer gate now logs destinations: {text!r}')
        else:
            report(label)


def _write_config(work: Path, name: str, config: dict) -> Path:
    path = work / name
    path.write_text(json.dumps(config) + '\n')
    return path


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
        case_agy_profile(arguments.platform, work)
        case_claude_profile(arguments.platform, work)
        smoke = smoke_config(work)
        case_smoke_config(smoke, work)
        smoke_path = _write_config(work, 'smoke-config.json', smoke)
        smoke_log = work / 'smoke.log'
        case_smoke_policy(load_addon(smoke_path, smoke_log), work, smoke_log)
        case_tls_destination_hooks(work)
        case_installer_policy_unchanged(arguments.platform, work)
    if failures:
        print(f'installer-egress-addon: {len(failures)} case(s) failed', file=sys.stderr)
        return 1
    print('installer-egress-addon: all cases passed')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
