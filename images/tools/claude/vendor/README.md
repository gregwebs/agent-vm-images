# claude — vendored installer

`install.upstream.sh` is a verbatim snapshot of Anthropic's published installer
(`https://claude.ai/install.sh`, retrieved 2026-09-30). It is provenance, not an
executable guard target, and is never run.

`install.sh` is the runnable patched copy. `install.patch` is the reviewable
patch; applying it to the snapshot reproduces `install.sh` byte-for-byte
(`script/test/claude-installer.sh` §1). `git apply` it from a directory holding
the snapshot renamed `install.sh`.

## Trust boundary

The selection authority is the committed closure in `images/tools/claude/`:
`pin.json`, the signed `evidence/manifest.json`, its detached
`evidence/manifest.json.sig`, the release signing key and `claude_pin.py`. The
signature is verified over the committed manifest with that key at pin time and on
every build, under the reviewed primary fingerprint
(`docs/adr/0013-claude-native-payload-pin.md`). The installer fetches exactly one
URL — the pinned executable for the local machine — checks its SHA-256 and size,
and refuses anything that is not an ELF object for that machine before placing it.

`manifest.json.raw-sig.json` is not fetched and not committed: it is an
RSA-PKCS1-v1_5-SHA512 record over an undocumented payload that does not verify
over the manifest bytes under any tried canonicalization.

The opaque `claude install <v>` step is not run here. It has a hardcoded CDN base
and re-downloads a secondary payload no hook can verify before use, so the verified
executable is placed in the native layout and `setup-claude.sh` runs the native
step in a separate `RUN --network=none`. The committed digest therefore also pins
the installer implementation that performs the offline setup.

## Patch hunks

1. **Exact TARGET only.** Rejects empty, `stable`, `latest`, ranges and URLs;
   requires `AGENT_VM_CONTRACT_DIR` and `CLAUDE_PIN_DIR`.
2. **curl required up front.** wget is not a download policy here.
3. **`download_file` routes through the shared classified helper**
   (`images/recipe-contract/download.sh`), so a transport failure is a receipt + 75
   and every other failure is hard.
4. **No live manifest, jq or zstd.** The checksum, size and URL come from
   `claude_pin.py select`, which verifies the committed evidence, the signature and
   the signed manifest; the selection is then re-checked here and the URL recomputed,
   so the pin can name bytes but never another location.
5. **No channel fetch.** `version="$TARGET"`; there is no `/latest` request.
6. **Fresh-install precondition.** An existing launcher or a non-empty `versions/`
   is refused instead of adopted without the content check.
7. **The single download propagates the classified 75** with `|| exit $?`.
8. **SHA-256 plus the ELF preflight before anything is placed.** A hash-consistent
   payload that is not this machine's executable (a shell script, the other
   architecture) is removed and nothing is placed.
9. **Placement into the native layout.** `versions/<v>` at mode 0755 and an absolute
   launcher symlink, then the layout is re-checked. A failure after the move exits 1
   (hard in every soft-fail mode) and the soft-fail cleanup never touches `versions/`.
10. **The native `claude install` is removed here.** `setup-claude.sh` runs it under
    `RUN --network=none`, after re-verifying the placed bytes.

The owning recipe is `images/standard/Dockerfile`, with read-only canonical helper
mounts. Suites: `script/test/claude-installer.sh` (hook, gate, patch
reproduction), `script/test/vendored-installers.sh` (the real installer and the
offline setup step, including before-execution controls),
`script/test/claude-pins.py` (the closure and the writer) and
`script/test/shipped-installer-network.sh` (single-URL egress).
