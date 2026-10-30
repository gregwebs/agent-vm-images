# Return the permission mode of one path using either GNU or BSD stat.
# Keep the dialect probes separate: GNU stat -f can print filesystem data before
# it fails, and that output must never be mistaken for a permission mode.
file_mode() { # $1 = path
    local mode
    if mode="$(stat -c '%a' "$1" 2>/dev/null)" && [ -n "$mode" ]; then
        printf '%s\n' "$mode"
        return 0
    fi
    if mode="$(stat -f '%Lp' "$1" 2>/dev/null)" && [ -n "$mode" ]; then
        printf '%s\n' "$mode"
        return 0
    fi
    printf 'cannot read the mode of %s with stat -c or stat -f\n' "$1" >&2
    return 1
}
