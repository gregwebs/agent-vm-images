#!/usr/bin/env bash
# Host build-CA boundary (#95): the real installer, real hashes and base64.
#
# apt-get, update-ca-certificates and openssl are stubs because the suite runs on
# a maintainer host; the certificate bytes, SHA-256 comparison, PEM structure
# rules, migration tripwire and stamp/trust writes are the production ones.
# BUILD_CA_TRUST_DIR, AGENT_VM_BASE_STAMP_DIR and APT_LISTS_DIR keep every write
# inside this invocation's scratch directory.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
INSTALLER="$ROOT/images/install-build-ca.sh"
CERT="$ROOT/script/test/fixtures/build-ca/host-ca.pem"
work=$(mktemp -d "${TMPDIR:-/tmp}/install-build-ca.XXXXXX")
trap 'rm -rf "$work"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}
command -v sha256sum >/dev/null || fail 'sha256sum required (GNU coreutils)'
hash=$(sha256sum "$CERT" | cut -d ' ' -f1)

mkdir "$work/bin"
cat >"$work/bin/apt-get" <<'SH'
#!/usr/bin/env bash
printf 'apt-get %s\n' "$*" >>"$LOG"
SH
cat >"$work/bin/update-ca-certificates" <<'SH'
#!/usr/bin/env bash
printf 'update-ca-certificates %s\n' "$*" >>"$LOG"
SH
cat >"$work/bin/openssl" <<'SH'
#!/usr/bin/env bash
printf 'openssl %s\n' "$*" >>"$LOG"
[ "${OPENSSL_FAIL:-}" != 1 ] || exit 1
SH
chmod 0755 "$work/bin/apt-get" "$work/bin/update-ca-certificates" "$work/bin/openssl"

case_dir='' trust='' stamp='' lists='' log='' secret=''
# Each case starts with no stamp directory and an empty trust directory, so a
# success branch has to create what it uses and a failure branch cannot inherit
# evidence from an earlier case.
fresh() {
    case_dir=$work/$1
    trust=$case_dir/trust
    stamp=$case_dir/stamp
    lists=$case_dir/lists
    log=$case_dir/calls.log
    secret=$case_dir/hostca.crt
    mkdir -p "$trust" "$lists"
    : >"$lists/lists-entry"
    : >"$log"
}
run() {
    PATH="$work/bin:$PATH" LOG="$log" BUILD_CA_TRUST_DIR="$trust" \
        AGENT_VM_BASE_STAMP_DIR="$stamp" APT_LISTS_DIR="$lists" \
        REMOVED_CA_SHIM_CACHEBUST=__removed_use_BUILD_CA_SHA256__ \
        bash "$INSTALLER" "$@" >"$case_dir/out" 2>&1
}
# The installer is run through `env` so a case can drop a variable entirely.
run_without_hash() {
    PATH="$work/bin:$PATH" LOG="$log" BUILD_CA_TRUST_DIR="$trust" \
        AGENT_VM_BASE_STAMP_DIR="$stamp" APT_LISTS_DIR="$lists" \
        bash "$INSTALLER" "$@" >"$case_dir/out" 2>&1
}
accepts() {
    local label=$1
    run "$secret" || fail "$label: $(cat "$case_dir/out")"
    echo "ok: $label"
}
refuses() {
    local label=$1 needle=$2 status=0
    run "$secret" || status=$?
    [ "$status" = 1 ] || fail "$label: status $status, wanted 1: $(cat "$case_dir/out")"
    grep -Fq -- "$needle" "$case_dir/out" ||
        fail "$label: missing '$needle' in: $(cat "$case_dir/out")"
    echo "ok: $label"
}
no_apt() {
    ! grep -q '^apt-get' "$log" || fail "$1: apt ran: $(cat "$log")"
    ! grep -q '^openssl' "$log" || fail "$1: openssl ran: $(cat "$log")"
}
no_trust_or_stamp() {
    [ ! -e "$trust/agent-vm-build.crt" ] || fail "$1: trust material was installed"
    [ ! -e "$stamp/build-ca-sha256" ] || fail "$1: a stamp was written"
}

fresh no-secret
accepts 'no secret and no hash trusts nothing'
[ "$(cat "$stamp/build-ca-sha256")" = none ] || fail 'the no-secret stamp must be none'
no_apt 'no secret'
[ ! -e "$trust/agent-vm-build.crt" ] || fail 'no-secret branch installed a certificate'

fresh no-secret-existing-stamp
mkdir -p "$stamp"
printf 'sha256:%s\n' "$hash" >"$stamp/build-ca-sha256"
accepts "no secret over an existing stamp"
[ "$(cat "$stamp/build-ca-sha256")" = none ] || fail 'a stale stamp survived the no-secret branch'

fresh matching
cp "$CERT" "$secret"
BUILD_CA_SHA256=$hash accepts 'matching reviewed certificate'
[ "$(cat "$stamp/build-ca-sha256")" = "sha256:$hash" ] || fail 'the stamp must record sha256:<hash>'
cmp -s "$secret" "$trust/agent-vm-build.crt" || fail 'the installed certificate differs'
[ -n "$(find "$trust/agent-vm-build.crt" -maxdepth 0 -type f -perm 0644)" ] ||
    fail 'the installed certificate must be 0644'
grep -q '^apt-get update$' "$log" || fail 'the apt bootstrap did not run'
grep -q '^apt-get install -y --no-install-recommends ca-certificates$' "$log" ||
    fail 'ca-certificates was not installed'
grep -q "^openssl x509 -noout -in $secret$" "$log" || fail 'the certificate was not parsed'
grep -q '^update-ca-certificates' "$log" || fail 'the bundle was not refreshed'
[ -z "$(ls -A "$lists")" ] || fail 'the apt lists were not removed'

fresh missing-secret
BUILD_CA_SHA256=$hash refuses 'hash without a secret' 'BUILD_CA_SHA256 set but no hostca secret supplied'
no_apt 'hash without a secret'
no_trust_or_stamp 'hash without a secret'

fresh empty-secret
: >"$secret"
BUILD_CA_SHA256=$hash refuses 'hash with an empty secret' 'BUILD_CA_SHA256 set but no hostca secret supplied'
no_apt 'hash with an empty secret'
no_trust_or_stamp 'hash with an empty secret'

fresh secret-without-hash
cp "$CERT" "$secret"
status=0
run_without_hash "$secret" || status=$?
[ "$status" = 1 ] || fail "secret without a hash exited $status"
grep -Fq 'BUILD_CA_SHA256 is required with --secret id=hostca' "$case_dir/out" ||
    fail 'the missing-hash refusal is not reported'
no_apt 'secret without a hash'
no_trust_or_stamp 'secret without a hash'

for value in 12345 "$(printf '%063d' 0)" "$(printf '%065d' 0)" \
    "ABCDEF$(printf '%058d' 0)" "g$(printf '%063d' 0)"; do
    fresh "bad-hash-${#value}-$RANDOM"
    cp "$CERT" "$secret"
    BUILD_CA_SHA256=$value refuses "invalid hash $value" \
        'BUILD_CA_SHA256 must be 64 lowercase hexadecimal characters'
    no_apt "invalid hash $value"
    no_trust_or_stamp "invalid hash $value"
done

fresh mismatch
cp "$CERT" "$secret"
wrong=$(printf 'b%.0s' $(seq 1 64))
BUILD_CA_SHA256=$wrong refuses 'mismatched certificate hash' \
    "hostca secret is not the reviewed certificate: expected $wrong, got $hash"
no_apt 'mismatched certificate hash'
no_trust_or_stamp 'mismatched certificate hash'
[ ! -e "$stamp" ] || fail 'a mismatched secret created the stamp directory'

fresh not-a-certificate
printf 'this is not a certificate\n' >"$secret"
mismatched=$(sha256sum "$secret" | cut -d ' ' -f1)
BUILD_CA_SHA256=$mismatched refuses 'malformed PEM' \
    'hostca must contain exactly one PEM certificate and no private key'
no_apt 'malformed PEM'
no_trust_or_stamp 'malformed PEM'

fresh private-key
cat >"$secret" <<'PEM'
-----BEGIN PRIVATE KEY-----
MIIBVQIBADANBgkqhkiG9w0BAQEFAASCAT8wggE7AgEAAkEAwA==
-----END PRIVATE KEY-----
PEM
keyhash=$(sha256sum "$secret" | cut -d ' ' -f1)
BUILD_CA_SHA256=$keyhash refuses 'private key instead of a certificate' \
    'hostca must contain exactly one PEM certificate and no private key'
no_apt 'private key'
no_trust_or_stamp 'private key'

fresh bundle
cat "$CERT" "$CERT" >"$secret"
bundlehash=$(sha256sum "$secret" | cut -d ' ' -f1)
BUILD_CA_SHA256=$bundlehash refuses 'certificate bundle' \
    'hostca must contain exactly one PEM certificate and no private key'
no_apt 'certificate bundle'
no_trust_or_stamp 'certificate bundle'

fresh bad-base64
sed '2s/^./*/' "$CERT" >"$secret"
badhash=$(sha256sum "$secret" | cut -d ' ' -f1)
BUILD_CA_SHA256=$badhash refuses 'non-base64 certificate body' \
    'hostca must contain exactly one PEM certificate and no private key'
no_apt 'non-base64 certificate body'
no_trust_or_stamp 'non-base64 certificate body'

fresh trailing-text
{ cat "$CERT"; printf 'trailing text\n'; } >"$secret"
trailinghash=$(sha256sum "$secret" | cut -d ' ' -f1)
BUILD_CA_SHA256=$trailinghash refuses 'text outside the certificate block' \
    'hostca must contain exactly one PEM certificate and no private key'
no_apt 'text outside the certificate block'
no_trust_or_stamp 'text outside the certificate block'

fresh openssl-failure
cp "$CERT" "$secret"
status=0
PATH="$work/bin:$PATH" LOG="$log" OPENSSL_FAIL=1 BUILD_CA_TRUST_DIR="$trust" \
    AGENT_VM_BASE_STAMP_DIR="$stamp" APT_LISTS_DIR="$lists" BUILD_CA_SHA256=$hash \
    bash "$INSTALLER" "$secret" >"$case_dir/out" 2>&1 || status=$?
[ "$status" = 1 ] || fail "openssl failure exited $status"
grep -Fq 'hostca is not a parseable X.509 certificate' "$case_dir/out" ||
    fail 'the openssl refusal is not reported'
no_trust_or_stamp 'openssl failure'
[ ! -e "$trust/agent-vm-build.crt" ] || fail 'a parse failure installed the certificate'
echo 'ok: openssl parse failure installs nothing'

fresh legacy-empty
cp "$CERT" "$secret"
status=0
PATH="$work/bin:$PATH" LOG="$log" BUILD_CA_TRUST_DIR="$trust" \
    AGENT_VM_BASE_STAMP_DIR="$stamp" APT_LISTS_DIR="$lists" BUILD_CA_SHA256=$hash \
    REMOVED_CA_SHIM_CACHEBUST='' bash "$INSTALLER" "$secret" >"$case_dir/out" 2>&1 || status=$?
[ "$status" = 1 ] || fail "an empty legacy argument exited $status"
grep -Fq 'CA_SHIM_CACHEBUST was removed; use BUILD_CA_SHA256=' "$case_dir/out" ||
    fail 'the migration tripwire is not reported for an empty legacy value'
no_apt 'empty legacy argument'
no_trust_or_stamp 'empty legacy argument'
echo 'ok: the migration tripwire rejects an empty legacy argument'

fresh legacy-nonempty
cp "$CERT" "$secret"
status=0
PATH="$work/bin:$PATH" LOG="$log" BUILD_CA_TRUST_DIR="$trust" \
    AGENT_VM_BASE_STAMP_DIR="$stamp" APT_LISTS_DIR="$lists" BUILD_CA_SHA256=$hash \
    REMOVED_CA_SHIM_CACHEBUST=$hash bash "$INSTALLER" "$secret" >"$case_dir/out" 2>&1 || status=$?
[ "$status" = 1 ] || fail "a legacy argument exited $status"
grep -Fq 'CA_SHIM_CACHEBUST was removed; use BUILD_CA_SHA256=' "$case_dir/out" ||
    fail 'the migration tripwire is not reported for a legacy value'
no_apt 'legacy argument'
no_trust_or_stamp 'legacy argument'
echo 'ok: the migration tripwire rejects any legacy value'

fresh unsafe-lists-directory
cp "$CERT" "$secret"
status=0
PATH="$work/bin:$PATH" LOG="$log" BUILD_CA_TRUST_DIR="$trust" \
    AGENT_VM_BASE_STAMP_DIR="$stamp" APT_LISTS_DIR="$case_dir" BUILD_CA_SHA256=$hash \
    bash "$INSTALLER" "$secret" >"$case_dir/out" 2>&1 || status=$?
[ "$status" = 1 ] || fail "a non-lists APT_LISTS_DIR exited $status"
grep -Fq 'APT_LISTS_DIR must name an apt lists directory' "$case_dir/out" ||
    fail 'the unsafe lists directory is not named'
no_trust_or_stamp 'unsafe lists directory'
echo 'ok: a non-lists APT_LISTS_DIR is refused before any write'

fresh usage
status=0
PATH="$work/bin:$PATH" LOG="$log" bash "$INSTALLER" >"$case_dir/out" 2>&1 || status=$?
[ "$status" = 2 ] || fail "no secret argument exited $status, wanted 2"
status=0
PATH="$work/bin:$PATH" LOG="$log" bash "$INSTALLER" a b >"$case_dir/out" 2>&1 || status=$?
[ "$status" = 2 ] || fail "two arguments exited $status, wanted 2"
echo 'ok: usage errors exit 2'

echo 'install-build-ca: all cases passed'
