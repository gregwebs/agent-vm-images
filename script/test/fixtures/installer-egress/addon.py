"""mitmproxy addon for the shipped-installer restricted-egress gate.

The gate runs the REAL patched vendored installers in a container whose only
route off the network is this proxy. The addon denies by default: a request is
forwarded only when it matches a committed exact-selection rule. GitHub release
downloads redirect to a signed, short-lived CDN URL, so a redirect target is
learned ONLY from the HTTP response to an already-allowed request -- a whole CDN
namespace is never opened up.

Deny patterns (latest/channel/dist-tag/installer bootstrap) are matched first
and logged, so a genuine "no hidden latest" claim can be checked by reading the
request log, not merely by the final installed version.

Config (mounted JSON, path in EGRESS_CONFIG):
  {"allow": [{"host": H, "path": REGEX, "method": M?}], "deny": [REGEX],
   "https_only": true?, "learn_hosts": [HOST...]?,
   "enforce_tls_destination": true?}
`EGRESS_MODE=observe` logs and forwards everything (capture mode); the default
`strict` enforces the allowlist. Every decision is appended to EGRESS_LOG.

`enforce_tls_destination` is an OPT-IN mode for the runtime network-request
smoke (ADR 0014), which is the only caller that sets it. The rules above match
the HTTP Host, which is not the TLS destination: a client can name an allowed
host and connect elsewhere, and mitmproxy's default eager strategy contacts the
CONNECT authority before any request hook runs. In this mode the CONNECT
authority, the SNI and the Host/`:authority` must all name the same declared
host, checked in `http_connect`, in `request` before the rules above, and in
`server_connect` before the upstream connection exists. The proxy must also run
with `connection_strategy=lazy`; otherwise every request is refused. The
installer gate does not set the key, and its policy is byte-for-byte unchanged.

A learned target is the signed CDN URL an already-allowed HTTPS response pointed
at. Scheme, port and credentials are enforced for it exactly as for any other
request -- only its query is exempt -- and `learn_hosts`, when present, names the
hosts such a response may hand over. An unlearnable redirect target is refused
and logged rather than parked in the allowlist.
"""

import json
import os
import re
from urllib.parse import urljoin, urlsplit, urlunsplit

from mitmproxy import ctx, http

CONFIG = os.environ["EGRESS_CONFIG"]
LOG = os.environ["EGRESS_LOG"]
MODE = os.environ.get("EGRESS_MODE", "strict")

with open(CONFIG, encoding="utf-8") as handle:
    _CFG = json.load(handle)
ALLOW = _CFG["allow"]
DENY = [re.compile(pattern) for pattern in _CFG.get("deny", [])]
ALLOW_HOSTS = frozenset(rule["host"] for rule in ALLOW)

# Off unless the config asks for it: the installer gate never sets the key.
ENFORCE_TLS_DESTINATION = _CFG.get("enforce_tls_destination") is True
_eager_strategy = False

_allowed_urls = set()
_learned = set()


def _redact(url):
    """Log the scheme/host/path only, dropping every query value.

    A learned CDN target is normally a short-lived signed URL whose query
    carries a bearer-style secret; logging it verbatim would leak it. Full URLs
    are retained only privately in the in-memory allowlist (review F5).
    """
    parts = urlsplit(url)
    host = parts.hostname or ""
    if parts.port:
        host += ":" + str(parts.port)
    return urlunsplit((parts.scheme, host, parts.path, "<redacted>" if parts.query else "", ""))


def _log(entry):
    with open(LOG, "a", encoding="utf-8") as handle:
        handle.write(entry + "\n")


def _block(flow, reason, url):
    _log("DENY %s %s" % (reason, _redact(url)))
    flow.response = http.Response.make(
        403, b"blocked by installer-egress allowlist\n",
        {"Content-Type": "text/plain"},
    )


def _shape_reason(parts):
    """Why a request's URL shape is unacceptable, or None.

    Scheme, port and credentials are checked for EVERY request, learned or not:
    an allowed response carrying `Location: http://…` or `https://user:pw@…`
    must not buy that target an exemption. Only the signed query is exempt, and
    only for a URL this proxy was handed by an already-allowed HTTPS response.
    """
    if parts.scheme != "https":
        return "https-required"
    if parts.port not in (None, 443):
        return "port-443-required"
    if parts.username or parts.password:
        return "no-credentials-required"
    if parts.fragment:
        return "no-fragment-required"
    return None


def _tls_destination(host, port):
    return port == 443 and host in ALLOW_HOSTS


def running():
    """A declared-destination gate is only a gate with lazy connections.

    With the default eager strategy mitmproxy opens the upstream connection at
    CONNECT time, before any request hook can compare the Host with it. A runner
    that forgot `--set connection_strategy=lazy` therefore fails closed here
    instead of silently checking only the Host header.
    """
    global _eager_strategy
    if not ENFORCE_TLS_DESTINATION:
        return
    if ctx.options.connection_strategy != "lazy":
        _eager_strategy = True
        _log("DENY eager-connection-strategy")


def http_connect(flow):
    """Refuse an unlisted CONNECT authority before any tunnel or upstream exists."""
    if not ENFORCE_TLS_DESTINATION:
        return
    host = flow.request.host
    if _eager_strategy or not _tls_destination(host, flow.request.port):
        _block(flow, "connect-unlisted", "https://%s/" % host)


def server_connect(data):
    """Final check, independent of the HTTP layer: the address it is dialling."""
    if not ENFORCE_TLS_DESTINATION:
        return
    address = data.server.address or ("", None)
    host, port = address[0], address[1]
    if _eager_strategy or not _tls_destination(host, port) or data.server.sni != host:
        data.server.error = "network-request destination refused"
        _log("DENY server-unlisted https://%s/" % host)


def server_connected(data):
    """Evidence: this is the TLS origin the proxy actually reached."""
    if not ENFORCE_TLS_DESTINATION:
        return
    _log("UPSTREAM https://%s/" % data.server.address[0])


def request(flow):
    url = flow.request.pretty_url
    if ENFORCE_TLS_DESTINATION:
        if _eager_strategy:
            _block(flow, "eager-connection-strategy", url)
            return
        destination = flow.server_conn.address[0] if flow.server_conn.address else None
        if flow.client_conn.sni != destination:
            _block(flow, "sni-mismatch", "https://%s/" % (flow.client_conn.sni or "-"))
            return
        if flow.request.pretty_host != destination:
            _block(flow, "authority-mismatch", url)
            return
    parts = urlsplit(url)
    learned = url in _learned
    if _CFG.get("https_only"):
        reason = _shape_reason(parts)
        if reason is not None:
            _block(flow, reason, url)
            return
        if parts.query and not learned:
            _block(flow, "no-query-required", url)
            return
    for pattern in DENY:
        if pattern.search(url):
            _block(flow, "deny-pattern", url)
            return
    if MODE == "observe":
        _log("ALLOW-OBSERVE %s" % _redact(url))
        return
    host = flow.request.pretty_host
    path = flow.request.path
    for rule in ALLOW:
        if rule["host"] != host:
            continue
        if not re.search(rule["path"], path):
            continue
        if rule.get("method") and rule["method"] != flow.request.method:
            continue
        _allowed_urls.add(url)
        _log("ALLOW %s" % _redact(url))
        return
    if learned:
        _log("ALLOW-LEARNED %s" % _redact(url))
        return
    _block(flow, "unlisted", url)


def _learnable(target):
    """A redirect target may be learned only if it is safe to spend the budget on.

    The origin is checked here as well as at request time: a learned target that
    could never be requested is not recovery material, it is an unvalidated URL
    sitting in the allowlist. `learn_hosts`, when configured, names the CDN hosts
    an already-allowed response is allowed to hand over.
    """
    parts = urlsplit(target)
    if _shape_reason(parts) is not None:
        return False
    hosts = _CFG.get("learn_hosts")
    if hosts is not None and parts.hostname not in hosts:
        return False
    return True


def response(flow):
    if MODE == "observe":
        return
    url = flow.request.pretty_url
    if _CFG.get("deny_redirects") and flow.response.status_code in (301, 302, 303, 307, 308):
        _block(flow, "redirect", url)
        return
    if not _CFG.get("learn_redirects", True):
        return
    if url not in _allowed_urls:
        return
    status = flow.response.status_code
    location = flow.response.headers.get("location")
    if status in (301, 302, 303, 307, 308) and location:
        target = urljoin(url, location)
        if not _learnable(target):
            _block(flow, "redirect-target-refused", target)
            return
        _learned.add(target)
        _log("LEARN %s" % _redact(target))
