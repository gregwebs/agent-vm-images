#!/usr/bin/env python3
"""The maintainer writers' HTTP transport: one exact URL, one deadline, one ceiling.

This is the block `script/dev/pin-codex.py` exposed while the Codex writer was
Python. The Codex writer is Go now (ADR 0017, `internal/fetch`), and the agy writer
is the last caller: it loads this module for `https_fetch` and imports nothing else
from it. It is deleted with that writer (#182), so it is deliberately a copy of the
reviewed transport rather than a new design.

A caller passes its own approved origins: the transport is shared, the approved set
is the owner's, and the two must not drift into one another.
"""
from __future__ import annotations

import signal
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

READ_CHUNK_BYTES = 1 << 16
# One 300-second budget per request. The deadline is what keeps a slow or
# truncated response from holding a maintainer terminal open, and the ceiling is
# what keeps a rewritten upstream asset from filling the host's disk.
FETCH_TIMEOUT_SECONDS = 300
# A release asset redirect chain is one hop today (github.com -> the signed CDN
# host). The bound exists so a server cannot hold the budget open with hops.
MAX_REDIRECTS = 5
# The only origins this transport will follow a redirect to. An owner whose
# requests start elsewhere still lands on GitHub's signed CDN, and nowhere else.
REDIRECT_ORIGINS = frozenset({'release-assets.githubusercontent.com',
                              'objects.githubusercontent.com'})
# The default approved origins. Every caller passes its own explicit set.
ORIGINS = frozenset({'api.github.com', 'github.com'})


class Refusal(Exception):
    """A prerequisite, an upstream response or a staged proposal is unacceptable."""


# --- transport ---------------------------------------------------------------

class _Deadline:
    """One absolute per-request budget, shared by every hop and every read."""

    def __init__(self, seconds: float) -> None:
        self.seconds = seconds
        self._end = time.monotonic() + seconds

    def remaining(self) -> float:
        left = self._end - time.monotonic()
        if left <= 0:
            raise Refusal(f'the request deadline expired after {self.seconds:g}s')
        return left


class _RequestWatchdog:
    """One alarm for the whole request: connect, headers, redirects and reads.

    Re-clamping the socket timeout bounds a read that eventually returns; it does
    not bound a read that never returns at all, and `HTTPResponse.read1` parses
    status lines, header lines and chunk framing *inside* the call it would
    otherwise bound. A peer that keeps the pipe busy a byte at a time, below the
    inactivity timeout but never completing a line, therefore holds the whole
    request open. Only an independent timer can end that, and one `SIGALRM` armed
    for the whole of `https_fetch` covers every one of those steps.

    The caller's own handler and its remaining timer are saved and restored, so
    this does not steal an alarm it did not install; if the handler or the timer
    cannot be installed the fetch fails closed instead of running unbounded.
    """

    def __init__(self, seconds: float) -> None:
        self.seconds = seconds
        self._handler = None
        self._timer: tuple[float, float] | None = None

    def _expired(self, _signum, _frame):
        raise Refusal(f'the request deadline expired after {self.seconds:g}s')

    def __enter__(self) -> '_RequestWatchdog':
        self._handler = signal.getsignal(signal.SIGALRM)
        try:
            signal.signal(signal.SIGALRM, self._expired)
            self._timer = signal.setitimer(signal.ITIMER_REAL, self.seconds)
        except (OSError, ValueError) as error:
            # Nothing is left armed: `setitimer` either never ran or the handler
            # install failed first, and a half-installed watchdog is exactly the
            # unbounded state this exists to prevent.
            try:
                signal.setitimer(signal.ITIMER_REAL, 0)
            except (OSError, ValueError):
                pass
            try:
                if self._handler is not None:
                    signal.signal(signal.SIGALRM, self._handler)
            except (OSError, ValueError):
                self._handler = None
            raise Refusal('the whole-request deadline cannot be enforced in this '
                          f'process ({error}); refusing an unbounded fetch') from None
        return self

    def __exit__(self, *_exception) -> None:
        # Disarm first, then hand the handler back, then let the caller's own
        # alarm continue where it would have been: a timer restored under the
        # wrong handler would fire into this module.
        try:
            signal.setitimer(signal.ITIMER_REAL, 0)
        finally:
            if self._handler is not None:
                try:
                    signal.signal(signal.SIGALRM, self._handler)
                except (OSError, ValueError):
                    pass
            if self._timer is not None and (self._timer[0] > 0 or self._timer[1] > 0):
                try:
                    signal.setitimer(signal.ITIMER_REAL, *self._timer)
                except (OSError, ValueError):
                    pass
        return None


class _Follow(Exception):
    """An approved redirect target that the fetch loop opens itself."""

    def __init__(self, url: str) -> None:
        super().__init__(url)
        self.url = url


class _GithubRedirects(urllib.request.HTTPRedirectHandler):
    """Approve a GitHub release-asset redirect, and only to a GitHub asset host.

    Approval leaves the hop to `https_fetch` (`_Follow`) instead of handing the
    new request back to urllib: urllib reuses the FIRST hop's socket timeout for
    every later hop, so a chain of slow hops could outlive the request deadline.
    Following here means each hop is opened under whatever budget is left.
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        target = urllib.parse.urlsplit(newurl)
        if target.scheme != 'https':
            raise Refusal(f'refusing a non-HTTPS redirect to {newurl}')
        if target.hostname not in REDIRECT_ORIGINS:
            raise Refusal(f'refusing a redirect to {target.hostname}')
        if target.username or target.password or target.port not in (None, 443):
            raise Refusal(f'refusing a redirect to {newurl}')
        # The redirect response itself is never streamed, so its body is closed
        # here rather than left to urllib (which would do it after this call).
        try:
            fp.close()
        except (AttributeError, OSError):
            pass
        raise _Follow(newurl)


def _require_origin(url: str, origins: frozenset[str] = ORIGINS) -> urllib.parse.SplitResult:
    """The URL's origin, or a refusal. `origins` is a keyword so an owner with a
    different approved set (`script/dev/pin-agy.py`) reuses this transport without
    a second copy of its bounds."""
    target = urllib.parse.urlsplit(url)
    if target.scheme != 'https':
        raise Refusal(f'{url} is not an HTTPS URL')
    if target.hostname not in origins:
        raise Refusal(f'{url} is not on an approved origin')
    if target.username or target.password or target.port not in (None, 443):
        raise Refusal(f'{url} carries credentials or a non-default port')
    if target.query or target.fragment:
        raise Refusal(f'{url} carries a query or fragment')
    return target


def _bound_transport(response, seconds: float) -> None:
    """Clamp the live socket timeout to the budget that is actually left.

    `HTTPResponse.read(n)` loops over the socket until it has `n` bytes, so the
    inactivity timeout set when the connection was opened does not bound a read
    at all: a peer dripping one byte just under that timeout can hold one read
    far past the deadline. The timeout is re-clamped to the remaining budget
    before every read and the loop reads at most one socket chunk. That bounds
    the read itself; it cannot bound a read that never returns, which is what
    `_RequestWatchdog` is for (`read1` also parses chunk framing inside the call).
    """
    try:
        sock = response.fp.raw._sock
    except (AttributeError, ValueError):
        return
    try:
        sock.settimeout(max(seconds, 0.001))
    except OSError:
        pass


def _read_chunk(response, size: int) -> bytes:
    """One socket-sized read; `read` would block until `size` bytes arrived."""
    read1 = getattr(response, 'read1', None)
    if read1 is not None:
        return read1(size)
    return response.read(size)


def https_fetch(url: str, output: Path, limit: int, *,
                origins: frozenset[str] = ORIGINS) -> None:
    """Stream one exact URL to `output` under a total deadline and a byte ceiling.

    The budget is enforced twice over: the independent watchdog can interrupt a
    step that never returns (connect, status line, headers, chunk framing), and
    the per-read deadline check turns the ordinary slow-body case into the same
    refusal before the alarm has to fire. Both are absolute across the redirect
    chain, and the deadline is checked again at EOF -- a response that ends after
    the budget is spent is a refusal, not a small file.
    """
    with _RequestWatchdog(FETCH_TIMEOUT_SECONDS):
        _fetch_under_deadline(url, output, limit, origins)


def _fetch_under_deadline(url: str, output: Path, limit: int,
                          origins: frozenset[str]) -> None:
    deadline = _Deadline(FETCH_TIMEOUT_SECONDS)
    target = _require_origin(url, origins)
    headers = {}
    if target.hostname == 'api.github.com':
        headers['Accept'] = 'application/vnd.github+json'
    opener = urllib.request.build_opener(_GithubRedirects())
    response = None
    for _hop in range(MAX_REDIRECTS + 1):
        request = urllib.request.Request(url, headers=headers)
        try:
            response = opener.open(request, timeout=deadline.remaining())
            break
        except _Follow as follow:
            url = follow.url
            continue
        except urllib.error.HTTPError as error:
            raise Refusal(f'cannot fetch {url}: HTTP {error.code}') from None
        except (urllib.error.URLError, OSError) as error:
            raise Refusal(f'cannot fetch {url}: {error}') from None
    if response is None:
        raise Refusal(f'{url} redirected more than {MAX_REDIRECTS} times')
    received = 0
    try:
        with response, open(output, 'wb') as handle:
            while True:
                _bound_transport(response, deadline.remaining())
                chunk = _read_chunk(response, min(READ_CHUNK_BYTES, limit + 1 - received))
                if not chunk:
                    break
                received += len(chunk)
                if received > limit:
                    raise Refusal(f'{url} is larger than the {limit} byte ceiling')
                handle.write(chunk)
            deadline.remaining()
    except Refusal:
        raise
    except TimeoutError:
        raise Refusal(f'the request deadline expired while reading {url}') from None
    except (urllib.error.URLError, OSError) as error:
        raise Refusal(f'cannot read {url}: {error}') from None


