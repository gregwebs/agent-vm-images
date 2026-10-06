#!/usr/bin/env bash
# Host-only local file replacement; never registry publication.
# Usage: publish_transactional BACKUP_DIR SRC DEST [SRC DEST ...]
# BACKUP_DIR must be a new, invocation-owned path. Status: 0 success, 1 fully
# restored failure, 2 usage, 3 incomplete recovery (caller MUST retain backups).
publish_transactional() {
    if [ "$#" -lt 3 ] || [ $(( ($# - 1) % 2 )) -ne 0 ]; then
        echo 'publish_transactional: usage: BACKUP_DIR SRC DEST [SRC DEST ...]' >&2
        return 2
    fi
    local backup_dir=$1
    shift
    local -a srcs=() dests=() existed=()
    local i j
    while [ "$#" -gt 0 ]; do
        for i in ${dests[@]+"${dests[@]}"}; do
            if [ "$i" = "$2" ]; then
                echo "publish_transactional: duplicate destination: $2" >&2
                return 2
            fi
        done
        srcs+=("$1"); dests+=("$2")
        shift 2
    done
    # Refuse reusing recovery material from a prior invocation.
    if ! mkdir "$backup_dir"; then
        echo "publish_transactional: cannot create owned backup directory: $backup_dir" >&2
        return 1
    fi
    for i in "${!srcs[@]}"; do
        if [ -e "${dests[$i]}" ] || [ -L "${dests[$i]}" ]; then
            existed+=(yes)
            if ! cp -p "${dests[$i]}" "$backup_dir/$i"; then
                echo "publish_transactional: backup failed: ${dests[$i]} -> $backup_dir/$i" >&2
                return 1
            fi
        else
            existed+=(no)
        fi
    done
    for i in "${!srcs[@]}"; do
        if ! cp "${srcs[$i]}" "${dests[$i]}"; then
            echo "error: failed to publish ${dests[$i]}; restoring attempted files inclusively" >&2
            local status=1
            # A failed cp may already have truncated destination i.
            for ((j=i; j>=0; j--)); do
                if [ "${existed[$j]}" = yes ]; then
                    if ! cp -p "$backup_dir/$j" "${dests[$j]}"; then
                        echo "error: rollback failed: ${dests[$j]}; recovery backup: $backup_dir/$j" >&2
                        status=3
                    fi
                elif ! rm -f "${dests[$j]}"; then
                    echo "error: rollback removal failed: ${dests[$j]}; originally absent; backups: $backup_dir" >&2
                    status=3
                fi
            done
            return "$status"
        fi
    done
    return 0
}
