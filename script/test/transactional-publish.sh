#!/usr/bin/env bash
# Failed cp can truncate its destination. Verify bytes AND mode, not just status.
set -euo pipefail
ROOT="$(cd "${BASH_SOURCE[0]%/*}/../.." && pwd)"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/bin"
cat >"$work/bin/cp" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
src=${@: -2:1}
dest=${@: -1}
if [[ "$src" == "$CASE/backup/"* ]]; then
    printf '%s\n' "$dest" >>"$CASE/restores"
    if [[ "${ROLLBACK_FAIL:-}" == "$dest" ]]; then exit 9; fi
elif [[ "$dest" == "${FAIL_DEST:-}" ]]; then
    printf partial >"$dest"
    chmod 600 "$dest"
    exit 8
elif [[ "$dest" == "${BACKUP_FAIL:-}" ]]; then
    exit 7
fi
exec /bin/cp "$@"
SH
chmod +x "$work/bin/cp"
# shellcheck source=/dev/null
. "$ROOT/script/build/transactional-publish.sh"
fail() { echo "FAIL: $*" >&2; exit 1; }
for count in 1 3; do
    for ((position=0; position<count; position++)); do
        for kind in existing absent rollback backup; do
            CASE="$work/$count-$position-$kind"
            mkdir "$CASE"
            pairs=()
            for ((i=0; i<count; i++)); do
                printf 'original %s\n' "$i" >"$CASE/d$i"
                chmod 751 "$CASE/d$i"
                /bin/cp -p "$CASE/d$i" "$CASE/orig$i"
                printf 'new %s\n' "$i" >"$CASE/s$i"
                pairs+=("$CASE/s$i" "$CASE/d$i")
            done
            FAIL_DEST="$CASE/d$position"; ROLLBACK_FAIL=; BACKUP_FAIL=
            expected=1
            case "$kind" in
                absent) rm "$CASE/d$position" ;;
                rollback) ROLLBACK_FAIL="$CASE/d$position"; expected=3 ;;
                backup) BACKUP_FAIL="$CASE/backup/0"; FAIL_DEST= ;;
            esac
            export CASE FAIL_DEST ROLLBACK_FAIL BACKUP_FAIL
            if PATH="$work/bin:$PATH" publish_transactional "$CASE/backup" "${pairs[@]}" 2>"$CASE/stderr"; then
                status=0
            else status=$?; fi
            [[ "$status" == "$expected" ]] || fail "$kind status $status, wanted $expected"
            for ((i=0; i<count; i++)); do
                if [[ "$kind" == absent && "$i" == "$position" ]]; then
                    [[ ! -e "$CASE/d$i" ]] || fail 'absent destination was not removed'
                elif [[ "$kind" != rollback || "$i" != "$position" ]]; then
                    cmp "$CASE/d$i" "$CASE/orig$i" || fail "$kind destination $i not restored"
                    python3 - "$CASE/d$i" "$CASE/orig$i" <<'PYMODE'
import os, stat, sys
assert stat.S_IMODE(os.stat(sys.argv[1]).st_mode) == stat.S_IMODE(os.stat(sys.argv[2]).st_mode)
PYMODE
                fi
            done
            if [[ "$kind" == rollback ]]; then
                grep -F "$CASE/backup/$position" "$CASE/stderr" >/dev/null || fail 'backup path missing'
                cmp "$CASE/backup/$position" "$CASE/orig$position" || fail 'recovery bytes lost'
                [[ $(wc -l <"$CASE/restores") -eq $((position+1)) ]] || fail 'not every rollback attempted'
            fi
        done
    done
done
if publish_transactional 2>/dev/null; then fail 'empty arguments accepted'; else [[ $? == 2 ]]; fi
if publish_transactional "$work/duplicate" a b c b 2>/dev/null; then fail 'duplicate accepted'; else [[ $? == 2 ]]; fi
[[ ! -e "$work/duplicate" ]] || fail 'usage modified filesystem'
echo 'transactional publication controls passed'
