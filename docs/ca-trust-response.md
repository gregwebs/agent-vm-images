# Root-store advisory response

Runbook for an advisory of the shape "trust in CA X or root Y must be
withdrawn". It exists because the 2026-10-20 eMudhra intermediate compromise was
answered ad hoc in [#71](https://github.com/gregwebs/agent-vm-images/pull/71);
the next advisory should not require re-deriving which stores these images
actually depend on.

## Response sequence

1. Read the advisory's fixed-version list (package revision, runtime release,
   toolchain release) and keep its test chains.
2. **Rebuild before believing anything.** The base parents are digest-pinned,
   so a rebuild resolves the current `ca-certificates` revision inside the
   reviewed Debian parent and the current Node release from NodeSource
   `setup_22.x`. A parent-layer fix needs a reviewed parent refresh and a
   rebuild: rebuilding cannot change a frozen parent, and a floating
   `apt-get install` needs `--no-cache` to requery the archive. A pre-advisory
   tag carries pre-advisory stores; a clean check against it is not evidence.
3. Check each store below offline (see [Verify offline](#verify-offline)).
4. Bump only what a rebuild cannot fix — in practice an example-layer pin.
   Example layers are built cold in CI on a change to the layer
   ([ADR 0010](adr/0010-layer-build-ci.md)), but only `chrome-devtools` has
   runtime coverage there, so check the rebuilt layer's trust store by hand.
5. Record what was checked and what stayed unchecked in the issue, and add any
   new gap to [Blind spots](#blind-spots) here.

## What the images trust

```text
withdrawn root R
├── /etc/ssl/certs/ca-certificates.crt   system bundle, owned by Debian
│   ├── Node                 ✗ ignores it: roots are compiled into the binary
│   ├── pip / certifi        ✓ Debian patch redirects certifi to this file
│   ├── Go (go-dev)          ✓ reads it on Linux; no bundled Mozilla roots
│   └── Rust (rust-dev)      ✓ reads the system bundle (static OpenSSL)
└── /usr/local/share/ca-certificates/agent-vm-build.crt
                             host CA shim, merged into the same bundle when a
                             build supplies --secret id=hostca
```

| Store | Comes from | Who fixes it | Check in a built image |
|---|---|---|---|
| System bundle `/etc/ssl/certs/ca-certificates.crt` | `ca-certificates` apt package in the pinned `debian:13-slim` parent; no version pin in this repo | a reviewed parent refresh for a parent-layer fix, or a cache-invalidated rebuild that lands the fixed package revision | `dpkg -s ca-certificates` |
| Host CA shim `/usr/local/share/ca-certificates/agent-vm-build.crt` | the builder's `--secret id=hostca` plus the reviewed `BUILD_CA_SHA256` ([optional build trust](../README.md#optional-build-trust)); the installer verifies the hash and the PEM structure, `update-ca-certificates` merges it into the system bundle, and the image records `sha256:<hash>` and a label that the public release refuses | the builder — a distro bump never removes it, and it is indistinguishable inside the bundle | `ls /usr/local/share/ca-certificates` and the `org.agent-vm.build-ca.sha256` label |
| pip's vendored certifi (`pip/_vendor/certifi`) | `python3-pip` from Debian, which patches `certifi.where()` to `DEBIAN_CA_CERTS_PATH` | nobody: it resolves to the system bundle | `python3 -c 'from pip._vendor import certifi; print(certifi.where())'` |
| Node bundled roots | NodeSource `setup_22.x`; Node does not read the system store | the Node release, then a rebuild | `tls.getCACertificates("default")` |
| Go toolchain (`go-dev` example pin) | go.dev tarball, digest-pinned in `examples/layers/go-dev/Dockerfile` | nothing in the trust path: `GOROOT` ships no Mozilla bundle (only `platform_root_*.pem` for macOS/Windows pinning), so Linux reads the system bundle | `find "$(go env GOROOT)/src/crypto/x509" -maxdepth 1 -name '*.pem'` |
| Rust toolchain (`rust-dev` example pin) | rustup channel pinned in `examples/layers/rust-dev/Dockerfile` | nothing in the trust path: no bundled roots; cargo's statically linked OpenSSL reads Debian's `/usr/lib/ssl` (`/etc/ssl`) store and honours `SSL_CERT_FILE` | `find /opt/rustup /opt/cargo -name '*.pem'` (a bundled store would appear here) |

The vendored `pip/_vendor/certifi/cacert.pem` file still exists on disk and is
the unfixed upstream bundle; its bytes are not the live store. A `certifi`
installed from PyPI into a venv does ship its own bundle and does not inherit
the Debian patch.

## Verify offline

An advisory normally publishes its test chain. `openssl verify` answers "does
this store still trust the withdrawn root" without touching the network:

```bash
openssl verify -CAfile /etc/ssl/certs/ca-certificates.crt \
  -untrusted chain.pem chain.pem   # target is the chain's own leaf
```

"OK" means the store still trusts the withdrawn root and the response is not
done. `-show_chain` names the root it accepted.

List what a bundle holds by splitting it and reading subjects:

```bash
docker run --rm IMAGE bash -c '
  set -eu
  csplit -s -z -f /tmp/root- /etc/ssl/certs/ca-certificates.crt \
    "/BEGIN CERTIFICATE/" "{*}"
  for f in /tmp/root-*; do openssl x509 -in "$f" -noout -subject; done'
```

For Debian's package, `dpkg -L ca-certificates` and the
`/usr/share/ca-certificates/mozilla/` filenames name every root, so a
`20250419` vs `20250419+deb13u1` file-list diff is the exact answer to "what did
the fixed revision remove".

Node needs its own dump because it never reads the bundle:

```bash
docker run --rm IMAGE node -e '
  const fs = require("fs");
  require("tls").getCACertificates("default")
    .forEach((pem, i) => fs.writeFileSync(`/tmp/node-${i}.pem`, pem))'
# then openssl x509 -noout -subject over /tmp/node-*.pem
```

Match the advisory's **exact** root name. The eMudhra fix removed two roots and
kept three other eMudhra roots in the same distro package, so a search for
"emSign" over-reports and a search for whatever string the advisory used in
prose under-reports.

Do not probe the advisory's own hosts. They rate-limit, and a timeout or DNS
failure is not evidence about a trust store.

## Blind spots

- **No gate asserts bundle contents.** Deliberately not added in #71: a
  build-time assertion would fail on unrelated distro churn and would still
  miss the next advisory's root. Nothing here fails when a store regresses.
- **Cold example-layer builds do not certify trust stores.** `example-layers.yml`
  now builds every layer on both native architectures, so a withdrawn pin or a
  broken install fails there instead of at the next manual build. Nothing in
  that build reads a CA bundle: store contents still need the check above.
- **Standard certification checks tool identity, not trust stores.**
  `images/standard/verify-standard.sh` proves installation records, PATH
  identity, versions and upgrade paths. It never reads a CA bundle.
- **Local tags are stale by construction.** On 2026-10-21 the development
  host's `agent-vm-base:local` still held `ca-certificates 20250419` (without
  the security revision) and Node 22.23.3; both still carried the withdrawn
  root. Rebuild before checking.
- **Host stores are out of scope.** macOS `/etc/ssl/cert.pem`, host Node
  installs and browser stores follow the OS/vendor update path, not this
  repository.

## Worked example: 2026-10-20 eMudhra

Advisory: private keys for eight eMudhra intermediates are recoverable from the
certificates themselves; withdraw "Comodo AAA Services root" and "emSign ECC
Root CA - G3". Fixed versions named: `ca-certificates 20250419+deb13u1`, Node
22.23.4, Go 1.27.3 / 1.26.10.

Observed 2026-10-21 with the checks above:

| Store | Observation | Outcome |
|---|---|---|
| System bundle | installed `20250419` contains both withdrawn roots; `20250419+deb13u1` is the trixie-security candidate and its file list drops exactly `Comodo_AAA_Services_root.crt` and `emSign_ECC_Root_CA_-_G3.crt`, keeping `emSign_ECC_Root_CA_-_C3`, `emSign_Root_CA_-_C1`, `emSign_Root_CA_-_G1` | rebuild; no pin to change |
| pip / certifi | `certifi.where()` → `/etc/ssl/certs/ca-certificates.crt` | inherits the fix; no action |
| Node | 22.23.3, the version in the local base, carried `emSign ECC Root CA - G3`; 22.23.4 (latest 22.x from NodeSource) drops `emSign ECC Root CA - G3`, `emSign ECC Root CA - C3` and `emSign Root CA - C1`, keeping `emSign Root CA - G1` | rebuild |
| Go (`go-dev` pin) | `1.27.1` was pinned, `GOROOT` carries no root bundle, Linux reads the system bundle | bump to `1.27.3` with both per-architecture digests (#71) to match the releases that drop the roots where Go does bundle them |
| Rust (`rust-dev` pin) | no `*.pem` roots under `/opt/rustup` or `/opt/cargo`; cargo's statically linked OpenSSL resolves the system store | unchanged |
| Host CA shim | not supplied in the affected builds | nothing to do |

The Go bump is the one edit that a rebuild could not produce, and it is the
reason the example pin, its two digests and the layer's README version move
together: `install-go.sh` verifies the tarball against the digest declared
beside the pin, so a stale digest fails the build instead of shipping a
different toolchain.
