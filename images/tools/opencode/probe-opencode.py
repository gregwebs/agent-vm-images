#!/usr/bin/env python3
"""Private OpenCode transport and strict, credential-free activation evidence."""
import argparse
import base64
from dataclasses import dataclass
import json
import os
from pathlib import Path
import secrets
import resource
import selectors
import signal
import subprocess
import sys
import tempfile
import time
from typing import Literal
import urllib.error
import urllib.parse
import urllib.request

MAX_BODY = 4 * 1024 * 1024
SCHEMA = 1


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def strict_json(raw):
    return json.loads(raw, object_pairs_hook=unique_object,
                      parse_constant=lambda value: (_ for _ in ()).throw(ValueError(value)))


def loopback(url, expected_path):
    if not isinstance(url, str):
        raise ValueError("loopback URL must be a string")
    parsed = urllib.parse.urlsplit(url)
    if (parsed.scheme != "http" or parsed.hostname != "127.0.0.1"
            or parsed.username or parsed.password or parsed.query or parsed.fragment
            or parsed.path != expected_path or not parsed.port or not 1 <= parsed.port <= 65535):
        raise ValueError("unsafe loopback URL")
    return url


@dataclass(frozen=True)
class LocatedRecords:
    directory: str
    records: list


@dataclass(frozen=True)
class Activation:
    plugin_id: Literal["openchamber.claude-code"]
    provider_id: Literal["claude-code"]
    proxy: str
    model_ids: tuple[str, ...]


def located(raw, directory):
    record = strict_json(raw)
    if (not isinstance(record, dict) or record.get("location") != {"directory": directory}
            or not isinstance(record.get("data"), list)
            or not all(isinstance(item, dict) for item in record["data"])):
        raise ValueError("invalid or wrong-location response envelope")
    return LocatedRecords(directory, record["data"])


def activation(model_raw, plugin_raw, provider_raw, directory, plugin_entry):
    models = located(model_raw, directory)
    plugins = located(plugin_raw, directory)
    providers = located(provider_raw, directory)
    selected = [p for p in plugins.records if p.get("id") == "openchamber.claude-code"]
    if (len(selected) != 1 or selected[0].get("state") != {"status": "active"}
            or selected[0].get("source") != {"type": "local", "path": plugin_entry}):
        raise ValueError("expected one active local Claude plugin")
    selected = [p for p in providers.records if p.get("id") == "claude-code"]
    if len(selected) != 1 or selected[0].get("activation") != "enabled":
        raise ValueError("expected one enabled Claude provider")
    settings = selected[0].get("settings")
    if not isinstance(settings, dict):
        raise ValueError("provider settings must be an object")
    proxy = loopback(settings.get("baseURL", ""), "/v1")
    ids = [m.get("id") for m in models.records if m.get("providerID") == "claude-code"]
    if not ids or not all(isinstance(value, str) and value.strip() for value in ids) or len(ids) != len(set(ids)):
        raise ValueError("expected positive unique Claude model IDs")
    return Activation("openchamber.claude-code", "claude-code", proxy, tuple(ids))


def decode(envelope, key):
    raw = base64.b64decode(envelope[key], validate=True)
    if len(raw) > MAX_BODY:
        raise ValueError("oversized evidence body")
    return raw


def check(evidence, plugin_entry, directory):
    envelope = strict_json(Path(evidence).read_bytes())
    if not isinstance(envelope, dict) or type(envelope.get("schema")) is not int or envelope["schema"] != SCHEMA:
        raise ValueError("unsupported evidence schema")
    result = activation(*(decode(envelope, key) for key in ("model_b64", "plugin_b64", "provider_b64")),
                        directory, plugin_entry)
    health = strict_json(decode(envelope, "health_b64"))
    if not isinstance(health, dict) or health.get("ok") is not True or health.get("provider") != result.provider_id:
        raise ValueError("Claude proxy is not healthy")
    return result


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError("redirect rejected")


def sanitized_environment(state_root, password):
    # Preserve image PATH and OPENCODE_CONFIG; never repair a missing image ENV.
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(("ANTHROPIC_", "CLAUDE_", "OPENCODE_", "AWS_", "GOOGLE_", "AZURE_", "XDG_"))
           and not key.endswith(("_API_KEY", "_AUTH_TOKEN", "_OAUTH_TOKEN"))
           and key not in ("HOME", "NODE_OPTIONS", "NODE_PATH", "BUN_OPTIONS", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
                           "http_proxy", "https_proxy", "all_proxy", "no_proxy")}
    if "OPENCODE_CONFIG" in os.environ:
        env["OPENCODE_CONFIG"] = os.environ["OPENCODE_CONFIG"]
    for key, suffix in (("HOME", "home"), ("XDG_CONFIG_HOME", "config"), ("XDG_CACHE_HOME", "cache"),
                        ("XDG_DATA_HOME", "data"), ("XDG_STATE_HOME", "state")):
        target = state_root / suffix
        target.mkdir()
        env[key] = str(target)
    env.update(OPENCODE_PASSWORD=password, OPENCODE_SERVER_PASSWORD=password,
               OPENCODE_DISABLE_AUTOUPDATE="true", OPENCODE_DISABLE_MODELS_FETCH="true",
               CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1")
    return env


def stop_owned_group(process):
    # stdio EOF is the normal shutdown contract. Cleanup has a separate 10s bound.
    process.stdin.close()
    for sig, seconds in ((None, 2), (signal.SIGTERM, 3), (signal.SIGKILL, 5)):
        if sig is not None:
            try:
                os.killpg(process.pid, sig)
            except ProcessLookupError:
                pass
        try:
            process.wait(timeout=seconds)
        except subprocess.TimeoutExpired:
            continue
        # A parent may exit while the proxy or CLI remains in its owned group.
        if sig is None:
            continue
        if sig == signal.SIGKILL:
            return
    if process.poll() is None:
        raise ValueError("owned process did not terminate")


def deadline_expired(_signal, _frame):
    raise TimeoutError("capture deadline exceeded")


def bound_child_output():
    resource.setrlimit(resource.RLIMIT_FSIZE, (8 * 1024 * 1024, 8 * 1024 * 1024))


def capture(args):
    deadline = time.monotonic() + args.timeout
    directory = str(Path(args.directory or os.getcwd()).resolve(strict=True))
    diagnostics = []
    envelope = {"schema": SCHEMA, "directory": directory}
    password = secrets.token_urlsafe(32)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    auth = base64.b64encode(f"opencode:{password}".encode()).decode()

    def request(url, authenticated=False):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("capture deadline exceeded")
        headers = {"x-opencode-directory": directory}
        if authenticated:
            headers["Authorization"] = f"Basic {auth}"
        req = urllib.request.Request(url, headers=headers)
        try:
            with opener.open(req, timeout=min(8, remaining)) as response:
                body = response.read(MAX_BODY + 1)
                if len(body) > MAX_BODY:
                    raise ValueError("oversized HTTP response")
                return body
        except urllib.error.HTTPError as error:
            body = error.read(8192)
            diagnostics.append({"endpoint": urllib.parse.urlsplit(url).path, "status": error.code,
                                "body": body.decode("utf-8", "replace")})
            # Only the upstream location activation timeout is transient.
            if error.code == 503:
                failure = strict_json(body)
                if (isinstance(failure, dict) and failure.get("message") == "Location plugins did not finish activating"
                        and failure.get("service") == "plugin"):
                    return None
            raise ValueError(f"HTTP {error.code}") from error

    Path(args.state_root).mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="probe-", dir=args.state_root) as private:
        root = Path(private)
        env = sanitized_environment(root, password)
        with tempfile.TemporaryFile() as stderr:
            process = subprocess.Popen([args.binary, "serve", "--stdio", "--hostname", "127.0.0.1", "--port", "0"],
                                       stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=stderr,
                                       cwd=directory, env=env, start_new_session=True, preexec_fn=bound_child_output)
            previous_alarm = signal.signal(signal.SIGALRM, deadline_expired)
            signal.setitimer(signal.ITIMER_REAL, max(0.001, deadline - time.monotonic()))
            try:
                selector = selectors.DefaultSelector()
                selector.register(process.stdout, selectors.EVENT_READ)
                startup = bytearray()
                while b"\n" not in startup:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0 or not selector.select(remaining):
                        raise TimeoutError("startup deadline exceeded")
                    chunk = os.read(process.stdout.fileno(), 4096)
                    if not chunk:
                        raise ValueError("server exited before startup")
                    startup.extend(chunk)
                    if len(startup) > 8192:
                        raise ValueError("oversized startup URL")
                selector.close()
                url = strict_json(startup.split(b"\n", 1)[0])["url"]
                loopback(url, "")
                model = None
                while model is None:
                    model = request(url + "/api/model", True)
                    if model is None:
                        time.sleep(min(0.1, max(0, deadline - time.monotonic())))
                plugin = request(url + "/api/plugin", True)
                provider = request(url + "/api/provider", True)
                # Transport validates the destination without deciding certification.
                providers = located(provider, directory).records
                candidates = [p for p in providers if p.get("id") == "claude-code"]
                if len(candidates) != 1:
                    raise ValueError("missing/duplicate proxy destination")
                settings = candidates[0].get("settings")
                if not isinstance(settings, dict):
                    raise ValueError("proxy settings must be an object")
                proxy = loopback(settings.get("baseURL", ""), "/v1")
                health = request(proxy + "/health")
                for key, raw in (("model_b64", model), ("plugin_b64", plugin), ("provider_b64", provider), ("health_b64", health)):
                    envelope[key] = base64.b64encode(raw).decode()
            finally:
                signal.setitimer(signal.ITIMER_REAL, 0)
                signal.signal(signal.SIGALRM, previous_alarm)
                stop_owned_group(process)
                stderr.seek(0, os.SEEK_END)
                size = stderr.tell()
                stderr.seek(max(0, size - 8192))
                diagnostics.append({"startup_stderr": stderr.read().decode("utf-8", "replace")})
                # Diagnostics never feed predicates and never contain probe authority.
                diagnostic_text = json.dumps(diagnostics).replace(password, "[redacted]").replace(auth, "[redacted]")
                print(diagnostic_text, file=sys.stderr)
    return envelope


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    modes = parser.add_subparsers(dest="mode", required=True)
    cap = modes.add_parser("capture")
    cap.add_argument("--binary", required=True)
    cap.add_argument("--state-root", required=True)
    cap.add_argument("--timeout", type=int, default=60, choices=range(1, 61))
    cap.add_argument("--directory")
    check_parser = modes.add_parser("check")
    check_parser.add_argument("--evidence", required=True)
    check_parser.add_argument("--plugin-entry", required=True)
    check_parser.add_argument("--directory", default=os.getcwd())
    args = parser.parse_args()
    try:
        if args.mode == "capture":
            print(json.dumps(capture(args)))
        else:
            result = check(args.evidence, args.plugin_entry, str(Path(args.directory).resolve(strict=True)))
            print(json.dumps({"models": len(result.model_ids), "provider": result.provider_id}))
    except (OSError, ValueError, KeyError, TypeError, TimeoutError) as error:
        print(f"probe-opencode: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
