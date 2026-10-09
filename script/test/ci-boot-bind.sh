#!/usr/bin/env bash
# Exercise the actual workflow bind block, including output-injection rejection.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# No YAML dependency: extract the literal run block belonging to id: bind.
python3 - "$ROOT/.github/workflows/verify-release-boot.yml" "$work/bind.sh" <<'PY'
import sys
from pathlib import Path
lines = Path(sys.argv[1]).read_text().splitlines(keepends=True)
assert lines.count('        id: bind\n') == 1
start = lines.index('        id: bind\n') + 1
while lines[start] != '        run: |\n':
    assert not lines[start].startswith('      - '), 'bind has no literal run block'
    start += 1
body = []
for line in lines[start + 1:]:
    if line.strip() and not line.startswith('          '):
        break
    body.append(line[10:] if line.strip() else line)
assert body, 'empty bind block'
Path(sys.argv[2]).write_text(''.join(body))
PY
# A real clean checkout satisfies the harness guard without stubbing git.
git init -q "$work/harness"
git -C "$work/harness" -c user.name=Test -c user.email=test@example.invalid commit -qm fixture --allow-empty
GITHUB_SHA="$(git -C "$work/harness" rev-parse HEAD)"
export GITHUB_SHA
export GITHUB_REPOSITORY=gregwebs/agent-vm-images
fail() { echo "bind regression: $*" >&2; exit 1; }
check() {
    local name=$1 input=$2 ref=$3 expected=$4 status=0
    mkdir -p "$work/$name/tmp"
    (cd "$work"
     export VERSION_INPUT="$input" GITHUB_REF="$ref" RUNNER_TEMP="$work/$name/tmp"
     export GITHUB_ENV="$work/$name/env" GITHUB_OUTPUT="$work/$name/out"
     bash "$work/bind.sh") > "$work/$name/log" 2>&1 || status=$?
    if [ -n "$expected" ]; then
        [ "$status" = 0 ] || fail "$name rejected valid version"
        printf 'version=%s\n' "$expected" > "$work/$name/expected"
        cmp "$work/$name/expected" "$work/$name/out" || fail "$name wrong output"
    else
        [ "$status" != 0 ] || fail "$name unexpectedly passed"
        [ ! -s "$work/$name/out" ] || fail "$name published version output"
    fi
    echo "bind case $name passed"
}
check dispatch 0.1.3 refs/heads/main 0.1.3
check call 12.34.56 refs/heads/main 12.34.56
check precedence 0.1.3 refs/heads/test/release-boot/0.1.4 0.1.3
check branch '' refs/heads/test/release-boot/0.1.3 0.1.3
check branch-suffix '' refs/heads/test/release-boot/0.1.3/try-2 0.1.3
check lf $'0.1.3\n' refs/heads/main ''
check cr $'0.1.3\r' refs/heads/main ''
check embedded-lf $'0.1.3\n0.1.4' refs/heads/main ''
check embedded-cr $'0.1.3\r0.1.4' refs/heads/main ''
check duplicate-version $'0.1.3\nversion=0.1.4' refs/heads/main ''
check leading-junk x0.1.3 refs/heads/main ''
check trailing-junk 0.1.3x refs/heads/main ''
check leading-line $'junk\n0.1.3' refs/heads/main ''
check trailing-line $'0.1.3\njunk' refs/heads/main ''
check branch-invalid '' refs/heads/test/release-boot/latest ''
check empty-nonbranch '' refs/heads/main ''
echo 'ci boot bind controls passed'
