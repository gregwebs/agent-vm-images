# 0002: pnpm terminal failures remain hard

Status: accepted. The hard-only rule is retained and the bridge's npm-only
degraded producer has been removed, with the required acknowledgement recorded
on #42: https://github.com/gregwebs/agent-vm-images/issues/42#issuecomment-6281925524
(maintainer comment, 2026-10-20; #42 is closed). The acknowledged narrowing is
now implemented: the producer is gone, and curl's low-level transport
classification remains exactly as it was.

## Decision

Every pnpm failure remains hard, with soft mode unset, 1 or 0. There is no npm
retry, exit-75 receipt, absence record or classifier based on retry logs. Curl's
independent low-level transport classifier remains unchanged. A new classifier
would require a separately reviewed contract with terminal-structured evidence
and mixed policy/integrity negatives.

Dropping the bridge's old npm-only degraded classification was an explicit spec
narrowing, not a claim that #42 never required transport fidelity. The evidence
and deviation are recorded on #42 and the narrowing is acknowledged (link above).
Standard builds still require every agent with soft mode unset, and the final
standard image still rejects any absent or pending record.

Current state, not a proposal:

* Pi and the bridge are pnpm projects installed through the base manager; they
  are no longer `run-npm.sh` callers, and `images/recipe-contract/run-npm.sh` is
  deleted. A Pi pnpm/policy/preparation/byte failure is hard in every soft mode.
* The receipt and status grammar accept **download only**
  (`transport download CODE`). Retired npm codes are ordinary invalid records,
  pinned by parser negatives (unknown, NUL, trailing, multiline, no newline).
* Pi's *own* independent archive download keeps its receipt-proved low-level
  transport soft branch for the curl failures the download contract actually
  classifies, under soft 1 **or** 0. A checksum, byte, extraction, shrinkwrap,
  preparation or manager failure can never mint that removal authority.
* The bridge has no pnpm absence producer. Its only absence record is the
  derivative `absent-pi` written after a genuine Pi curl absence retires the
  feature; a present Pi with an absent bridge is hard.
* Historical transfer inventories keep the retired path explicitly historical;
  nothing rewrote them as if the adapter had never existed.

Do not clear a caller receipt before pnpm. No supported path has a stale curl
receipt there: child receipt isolation plus a fresh run-install envelope already
protect callers. Existing stale-file-unchanged, timeout/nonzero75-hard, redaction,
capture-failure and child-termination tests stay intact. No RLIMIT_FSIZE is added.

## Method and evidence (planning experiment, not proposed-SHA acceptance)

On 2026-10-16, authenticated stock pnpm 11.11.0 generated a frozen v9 one-package
`is-number@7.0.0` lock with all five image defaults, npmjs registry, scripts off
and isolated HOME/XDG/store. Dependency SRI:

`sha512-41Cifkg6e8TylSpdtTpeLVMqvSBEVzTttHvERD741+pnZ8ANv0004MRL43QKPDlK9cGvNp6NZWZUBlbGXYxxng==`

Small disposable containers on the existing native linux/arm64 base ran Node
22.23.3 and the actual `/usr/local/bin/pnpm` launcher, not host pnpm 12. Each case
used fresh project/state/store, unset upper/lower HTTP(S)/ALL proxy variables and
this cold frozen install:

```sh
timeout --kill-after=2 30 /usr/local/bin/pnpm install --prod --frozen-lockfile \
  --ignore-scripts --ignore-pnpmfile --pm-on-fail=error \
  --config.update-notifier=false --package-import-method=copy \
  --store-dir=/experiment/state-CASE/store --registry=FAULT_URL \
  --fetch-retries=0 --fetch-timeout=5000 --reporter=ndjson
```

Zero retries and 5s requests only bound the diagnostic. No policy was weakened
or lock trusted. Failures occurred during metadata verification before payload
installation. Loopback HTTP is a test-only fault registry, not production policy.
An unused TCP port, `.invalid` DNS, Node `socket.resetAndDestroy()`, self-signed
HTTPS (verification **on**), HTTP503 and malformed200 JSON provided the faults.
Four network/TLS cases were repeated; two controls were added in the second run.

| Fault / registry | Child exit | Terminal code |
|---|---:|---|
| refused `http://127.0.0.1:18080` | 1 twice | ERR_PNPM_META_FETCH_FAIL |
| DNS `https://avm42-does-not-exist.invalid` | 1 twice | ERR_PNPM_META_FETCH_FAIL |
| reset `http://127.0.0.1:18081` | 1 twice | ERR_PNPM_META_FETCH_FAIL |
| TLS `https://127.0.0.1:18082` | 1 twice | ERR_PNPM_META_FETCH_FAIL |
| HTTP503 `http://127.0.0.1:18083` | 1 | ERR_PNPM_FETCH_503 |
| malformed JSON `http://127.0.0.1:18084` | 1 | ERR_PNPM_BROKEN_METADATA_JSON |

Independent same-container fetch probes confirmed ECONNREFUSED, ENOTFOUND,
ECONNRESET and DEPTH_ZERO_SELF_SIGNED_CERT respectively. pnpm's terminal NDJSON
instead had top-level and `err.code` META_FETCH_FAIL and `err.message`
`GET <URL>/is-number: fetch failed`, with no nested errno/cause. For example:

```json
{"code":"ERR_PNPM_META_FETCH_FAIL","attempts":1,"err":{"name":"pnpm","message":"GET https://127.0.0.1:18082/is-number: fetch failed","code":"ERR_PNPM_META_FETCH_FAIL"}}
```

This is a projection of the terminal record, not its entire raw stack. Warnings
can follow the error, so last-line parsing is not a terminal classifier. Neither
host/path text nor an independent second probe proves the install failed only
from transport. 503 is distinguishable but HTTP errors were already hard; do not
broaden scope to server responses. The experiment does not exhaust every pnpm
phase/platform.

## Provenance and limits

Baseline source: `0205cf83e61b820b0241fabf31607fa60f072fec`.
Base ID: `sha256:0d13df85b0925bdf7e2b09d90e7f69bc5c0875686685a95aa6a01b162d735300`,
linux/arm64, created `2026-10-16T09:55:59.186156692-05:00`.
Source/image launcher SHA256:
`357f24931e36c4f0603494d9abc23647c12914e05e58e835fcfc398c19322cc1`.
Source/image pin.json SHA256:
`568f6bcff2e3ab2e459045c3fce55d0d6a56153c773c08b0d5c15e12f5ae7521`.
Image and authenticated host stock bundle SHA256 (also pin.bundleSha256):
`e8abc02d5fbffb8f9d8817d44d6eee84c379ea8d5006dd5aa8694bb5e4428d8a`.

Matching bytes support this bounded observation, not whole-base provenance.
The image had source/title/license/pnpm labels but no revision/build-source
attestation; the Dockerfile is absent from the image. No local base rebuild,
image pruning/deletion or payload installation was used. Clean native CI at each
proposed SHA supplies acceptance provenance; this planning experiment does not.
