#!/bin/sh
# The only code that talks to a package manager for the Chrome example (#41).
#
# The image preinstalls one reviewed selection, so the build must install it and
# first use must never resolve or fetch anything. Four verbs, one job each:
#
#   check DIR     pure and offline: the directory holds exactly the reviewed
#                 input set, at the reviewed bytes, with a single-package lock.
#   install       build only: check, install into the fixed prefix with the
#                 reviewed policy, then assert the installed tree.
#   verify        build only / finished image: assert that tree's permission,
#                 link and content invariant.
#   resolve DIR   maintenance only: re-resolve the lock inside the parent image
#                 for a new selection. It never writes pins or a manifest.
#
# Pins are written by script/dev/upgrade-chrome-devtools-mcp.sh on a maintainer
# host and by nothing else: a build can never re-pin what it verifies.
set -eu

PREFIX=/opt/agent-vm/chrome-devtools-mcp
MANAGER=/usr/local/bin/pnpm
PIN_FILE=pnpm-inputs.sha256
INPUTS='package.json pnpm-lock.yaml pnpm-workspace.yaml'
REVIEWED_SET='package.json pnpm-inputs.sha256 pnpm-lock.yaml pnpm-workspace.yaml'
REGISTRY=https://registry.npmjs.org/
# One canonical exact semver, verbatim from
# images/tools/opencode/plugins/prepare-lock.sh:19.
SEMVER='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'

die() { echo "$*" >&2; exit 1; }

# Deliberate `ls -A`: an exact-set assertion over known space-free names, in
# byte order. `find` would not give the same single-line comparison.
# shellcheck disable=SC2012
listing() { LC_ALL=C ls -A "$1" | tr '\n' ' '; }
usage() {
    echo 'usage: chrome-devtools-mcp.sh check DIR | install | verify | resolve DIR' >&2
    exit 2
}

# --- reviewed inputs -------------------------------------------------------

check_manifest() {
    file=$1/package.json
    [ -f "$file" ] || die "missing input: package.json"
    jq -e '
        (keys | sort) == ["dependencies", "name", "packageManager", "private", "version"]
        and (.dependencies | type) == "object"
        and (.dependencies | keys) == ["chrome-devtools-mcp"]
        and (.packageManager | type) == "string"
    ' "$file" >/dev/null ||
        die 'manifest must carry exactly name, version, private, packageManager, dependencies'
    selected=$(jq -r '.dependencies["chrome-devtools-mcp"] | strings' "$file")
    printf '%s' "$selected" | grep -Eqx "$SEMVER" ||
        die 'selection must be one canonical exact version'
    jq -r '.packageManager' "$file" | grep -Eqx 'pnpm@[0-9]+\.[0-9]+\.[0-9]+' ||
        die 'packageManager must be pnpm@EXACT'
    printf '%s\n' "$selected"
}

check_workspace() {
    file=$1/pnpm-workspace.yaml
    [ -f "$file" ] || die 'missing input: pnpm-workspace.yaml'
    # Lexical, for a specific message: the byte pin is the real guard.
    escape=$(awk '
        /^[[:space:]]*(minimumReleaseAge[A-Za-z]*|trustPolicy[A-Za-z]*|trustLockfile|ignoreScripts|onlyBuiltDependencies|neverBuiltDependencies|dangerouslyAllowAllBuilds|overrides|pnpmfile|registry|packageExtensions|patchedDependencies)[[:space:]]*:/ {
            line = $0
            sub(/^[[:space:]]*/, "", line)
            sub(/[[:space:]]*:.*/, "", line)
            print line
            exit
        }
    ' "$file")
    [ -z "$escape" ] || die "policy escape in pnpm-workspace.yaml: $escape"
    grep -qx 'allowBuilds: {}' "$file" || die 'pnpm-workspace.yaml must keep allowBuilds: {}'
}

check_pins() {
    dir=$1
    pins=$dir/$PIN_FILE
    [ -f "$pins" ] || die "missing input: $PIN_FILE"
    awk 'NF && $0 !~ /^[0-9a-f]{64}  (package\.json|pnpm-lock\.yaml|pnpm-workspace\.yaml)$/ { exit 1 }' "$pins" ||
        die 'pin file must name exactly package.json pnpm-lock.yaml pnpm-workspace.yaml'
    names=$(awk 'NF { print $2 }' "$pins" | LC_ALL=C sort)
    [ "$names" = 'package.json
pnpm-lock.yaml
pnpm-workspace.yaml' ] ||
        die 'pin file must name exactly package.json pnpm-lock.yaml pnpm-workspace.yaml'
    for name in $INPUTS; do
        [ -f "$dir/$name" ] || die "missing input: $name"
        want=$(awk -v name="$name" '$2 == name { print $1 }' "$pins")
        got=$(sha256sum "$dir/$name" | awk '{ print $1 }')
        [ "$got" = "$want" ] || die "$name: bytes differ from the reviewed pin"
    done
}

check_closure() {
    dir=$1
    want=$2
    lock=$dir/pnpm-lock.yaml
    shape() { die 'closure shape changed; review required'; }
    [ -f "$lock" ] || die 'missing input: pnpm-lock.yaml'
    [ "$(sed -n 1p "$lock")" = "lockfileVersion: '9.0'" ] || shape
    top=$(grep -E '^[A-Za-z]' "$lock" | sed 's/:.*$//' | LC_ALL=C sort | tr '\n' ' ')
    [ "$top" = 'importers lockfileVersion packages settings snapshots ' ] || shape
    awk -v want="  chrome-devtools-mcp@$want:" '
        /^[A-Za-z]/ { section = $0; sub(/:.*/, "", section); next }
        section == "packages" && /^  [^ ]/ { seen++; line = $0 }
        END { exit !(seen == 1 && line == want) }
    ' "$lock" || shape
    awk -v want="  chrome-devtools-mcp@$want: {}" '
        /^[A-Za-z]/ { section = $0; sub(/:.*/, "", section); next }
        section == "snapshots" && /^  [^ ]/ { seen++; line = $0 }
        END { exit !(seen == 1 && line == want) }
    ' "$lock" || shape
    # A future upstream with dependencies must be a reviewed change here.
    [ "$(grep -c 'integrity: sha512-' "$lock")" -eq 1 ] || shape
}

check_inputs() {
    dir=$1
    # Pins first, so tampered bytes get the byte message, not a shape message.
    check_pins "$dir"
    selected=$(check_manifest "$dir")
    check_workspace "$dir"
    check_closure "$dir" "$selected"
    printf '%s\n' "$selected"
}

cmd_check() {
    dir=$1
    [ -d "$dir" ] || die "not a directory: $dir"
    present=$(listing "$dir")
    for name in $REVIEWED_SET; do
        printf '%s\n' "$present" | tr ' ' '\n' | grep -qxF "$name" || die "missing input: $name"
    done
    for name in $present; do
        case " $REVIEWED_SET " in
            *" $name "*) ;;
            *) die "unreviewed input: $name" ;;
        esac
    done
    check_inputs "$dir"
}

# --- install environment ---------------------------------------------------

reject_ambient() {
    offenders=$(awk 'BEGIN {
        for (name in ENVIRON) {
            lower = tolower(name)
            if (lower ~ /^(npm_config_|pnpm_config_|corepack_)/ || lower == "pnpm_home") print name
        }
    }')
    [ -z "$offenders" ] || die "ambient package-manager configuration is unsupported: $(printf '%s' "$offenders" | tr '\n' ' ' | sed 's/ $//')"
    for name in NODE_OPTIONS NODE_PATH BUN_OPTIONS; do
        eval "value=\${$name:-}"
        [ -z "$value" ] || die "manager runtime overrides are unsupported: $name"
    done
    [ "${NODE_TLS_REJECT_UNAUTHORIZED:-}" != 0 ] || die 'TLS verification cannot be disabled'
}

# Private HOME/XDG/TMPDIR so no per-user state is read or written, and a PATH
# in which v0.1.3's `#!/usr/bin/env node` pnpm resolves /usr/bin/node.
private_state() {
    work=$(mktemp -d)
    work=$(cd "$work" && pwd -P)
    HOME=$work/home
    XDG_CONFIG_HOME=$work/home/.config
    XDG_CACHE_HOME=$work/home/.cache
    XDG_DATA_HOME=$work/home/.local/share
    XDG_STATE_HOME=$work/home/.local/state
    TMPDIR=$work/tmp
    mkdir -p "$HOME" "$TMPDIR"
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
    export HOME XDG_CONFIG_HOME XDG_CACHE_HOME XDG_DATA_HOME XDG_STATE_HOME TMPDIR PATH
}

# Every manager call below must see all five settings explicitly.
export_policy() {
    PNPM_CONFIG_MINIMUM_RELEASE_AGE=1440
    PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT=true
    PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME=false
    PNPM_CONFIG_TRUST_POLICY=no-downgrade
    PNPM_CONFIG_IGNORE_SCRIPTS=true
    export PNPM_CONFIG_MINIMUM_RELEASE_AGE PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT \
        PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME PNPM_CONFIG_TRUST_POLICY \
        PNPM_CONFIG_IGNORE_SCRIPTS
}

assert_manager() {
    want=$(jq -r '.packageManager | strings' "$1/package.json" | sed 's/^pnpm@//')
    got=$(timeout --kill-after=5 20 "$MANAGER" --version --pm-on-fail=error \
        --config.update-notifier=false 2>/dev/null) || got=unavailable
    [ "$got" = "$want" ] || die "pnpm $want is required (packageManager); parent provides $got"
}

# --- install ---------------------------------------------------------------

cmd_install() {
    [ "$(id -u)" = 0 ] || die 'install must run as root'
    # Before any Node or pnpm process exists.
    reject_ambient
    private_state
    export_policy
    success=false
    trap cleanup 0
    trap 'exit 130' INT
    trap 'exit 143' TERM
    trap 'exit 129' HUP
    selected=$(cmd_check "$PREFIX")
    assert_manager "$PREFIX"
    ( cd "$PREFIX" && "$MANAGER" install --config.update-notifier=false \
        --frozen-lockfile --prod --no-optional --ignore-scripts --ignore-pnpmfile \
        --pm-on-fail=error --registry="$REGISTRY" --package-import-method=copy \
        --store-dir "$work/store" )
    # COPY keeps context modes (e.g. 0664 under umask 002); normalize instead.
    chown -R -h 0:0 "$PREFIX"
    chmod -R u=rwX,go=rX "$PREFIX"
    cmd_verify
    success=true
    echo "chrome-devtools-mcp@$selected: installed and verified"
}

cleanup() {
    [ "${success:-false}" = true ] || rm -rf "$PREFIX/node_modules"
    rm -rf "$work"
}

# --- installed tree invariant ----------------------------------------------

root_only() { # PATH DESCRIPTION: no group/other write, no set-id, no sticky bit
    out=$(find "$1" -maxdepth 0 \( ! -user 0 -o -perm /7022 \) -print)
    [ -z "$out" ] || die "ancestor not root-owned and write-protected: $1"
}

cmd_verify() {
    p=$PREFIX
    selected=$(jq -er '.dependencies["chrome-devtools-mcp"] | strings' "$p/package.json")
    printf '%s' "$selected" | grep -Eqx "$SEMVER" || die 'selection must be one canonical exact version'

    # I1 the prefix is not reached through a symlink
    [ "$(cd "$p" && pwd -P)" = "$p" ] || die 'prefix must not be reached through a symlink'
    # I2 ancestors and the wrapper's ancestors are root-owned real directories
    for dir in / /opt /opt/agent-vm /usr /usr/local /usr/local/bin; do
        [ ! -L "$dir" ] && [ -d "$dir" ] || die "ancestor not root-owned and write-protected: $dir"
        root_only "$dir"
    done
    # I3 only regular files, directories and symlinks, and no newline in a name
    out=$(find "$p" ! -type f ! -type d ! -type l -print)
    [ -z "$out" ] || die "unsupported entry in prefix: $out"
    out=$(find "$p" -name "$(printf '*\n*')")
    [ -z "$out" ] || die "unsupported entry in prefix: $out"
    # I4 physical entries: 0:0, no group/other write, no set-id, world-readable
    out=$(find "$p" \( -type f -o -type d \) \
        \( ! -user 0 -o ! -group 0 -o -perm /7022 -o ! -perm -0444 \) -print)
    [ -z "$out" ] || die "prefix entry not root-owned read-only: $out"
    out=$(find "$p" -type d ! -perm -0111 -print)
    [ -z "$out" ] || die "prefix entry not root-owned read-only: $out"
    # I5 symlink owners (mode bits on a link are not used for access)
    out=$(find "$p" -type l ! -user 0 -print)
    [ -z "$out" ] || die "prefix link not root-owned: $out"
    # I6 every link resolves inside the physical prefix onto a file or directory
    find "$p" -type l -print >"$p/.links.tmp"
    while IFS= read -r link; do
        target=$(readlink -f -- "$link") || die "prefix link escapes or dangles: $link"
        case "$target" in
            "$p"/*) ;;
            *) die "prefix link escapes or dangles: $link" ;;
        esac
        [ -f "$target" ] || [ -d "$target" ] || die "prefix link escapes or dangles: $link"
    done <"$p/.links.tmp"
    rm -f "$p/.links.tmp"
    # I7 exact link set for the single-package closure
    links=$(find "$p" -type l -printf '%P -> %l\n')
    want="node_modules/chrome-devtools-mcp -> .pnpm/chrome-devtools-mcp@$selected/node_modules/chrome-devtools-mcp"
    [ "$links" = "$want" ] || die 'unexpected prefix links'
    # I8 exact layout
    [ "$(listing "$p")" = 'node_modules package.json pnpm-inputs.sha256 pnpm-lock.yaml pnpm-workspace.yaml ' ] ||
        die "unexpected prefix layout: $p"
    [ "$(listing "$p/node_modules")" = '.bin .modules.yaml .package-map.json .pnpm .pnpm-workspace-state-v1.json chrome-devtools-mcp ' ] ||
        die "unexpected prefix layout: $p/node_modules"
    [ "$(listing "$p/node_modules/.pnpm")" = "chrome-devtools-mcp@$selected lock.yaml " ] ||
        die "unexpected prefix layout: $p/node_modules/.pnpm"
    [ "$(listing "$p/node_modules/.bin")" = 'chrome-devtools chrome-devtools-mcp ' ] ||
        die "unexpected prefix layout: $p/node_modules/.bin"
    # I9 inputs still equal their pins; pnpm used the committed lock
    check_inputs "$p" >/dev/null
    cmp -s "$p/pnpm-lock.yaml" "$p/node_modules/.pnpm/lock.yaml" || die 'installed lock differs from the reviewed lock'
    # I10 the installed package is the selection
    [ "$(jq -er '.name' "$p/node_modules/chrome-devtools-mcp/package.json")" = chrome-devtools-mcp ] ||
        die "installed package is not chrome-devtools-mcp@$selected"
    [ "$(jq -er '.version' "$p/node_modules/chrome-devtools-mcp/package.json")" = "$selected" ] ||
        die "installed package is not chrome-devtools-mcp@$selected"
    # I11 pnpm's own record of the effective policy (not a constant: the
    # values differ when the exports are weakened or absent)
    jq -e '
        .settings
        | .minimumReleaseAge == 1440
        and .minimumReleaseAgeStrict == true
        and .minimumReleaseAgeIgnoreMissingTime == false
        and .trustPolicy == "no-downgrade"
        and .allowBuilds == {}
        and .nodeLinker == "isolated"
        and .production == true
        and .optional == false
    ' "$p/node_modules/.pnpm-workspace-state-v1.json" >/dev/null ||
        die 'pnpm did not record the reviewed policy'
    jq -e '.pendingBuilds == [] and .allowBuilds == {}' "$p/node_modules/.modules.yaml" >/dev/null ||
        die 'pnpm did not record the reviewed policy'
    # I12 the wrapper is root-owned, exactly 0755, not a link
    wrapper=/usr/local/bin/agent-vm-chrome-mcp
    [ ! -L "$wrapper" ] || die 'wrapper must be root-owned 0755'
    [ "$(stat -c '%u:%g:%a' "$wrapper")" = 0:0:755 ] || die 'wrapper must be root-owned 0755'
    # I13 the installed bin runs as chrome, offline, without chrome's HOME
    home=$(mktemp -d)
    chown 9999:9999 "$home"
    got=$(sudo -n -u chrome -- env HOME="$home" XDG_CACHE_HOME="$home/.cache" \
        "$p/node_modules/.bin/chrome-devtools-mcp" --version) ||
        { rm -rf "$home"; die 'installed chrome-devtools-mcp does not run as chrome'; }
    rm -rf "$home"
    [ "$got" = "$selected" ] || die 'installed chrome-devtools-mcp does not run as chrome'
    echo "chrome-devtools-mcp@$selected: installed tree verified"
}

# --- maintenance -----------------------------------------------------------

cmd_resolve() {
    dir=$1
    reject_ambient
    private_state
    export_policy
    success=false
    trap cleanup 0
    trap 'exit 130' INT
    trap 'exit 143' TERM
    trap 'exit 129' HUP
    [ "$(listing "$dir")" = 'package.json pnpm-workspace.yaml ' ] ||
        die 'resolve requires exactly package.json and pnpm-workspace.yaml'
    selected=$(check_manifest "$dir")
    check_workspace "$dir"
    assert_manager "$dir"
    ( cd "$dir" && "$MANAGER" install --lockfile-only --no-frozen-lockfile \
        --config.update-notifier=false --ignore-scripts --ignore-pnpmfile \
        --pm-on-fail=error --registry="$REGISTRY" --store-dir "$work/store" )
    [ ! -e "$dir/node_modules" ] || die 'resolve must not create node_modules'
    check_closure "$dir" "$selected"
    echo "chrome-devtools-mcp@$selected: lock resolved"
}

case ${1:-} in
    check) [ "$#" -eq 2 ] || usage; cmd_check "$2" ;;
    install) [ "$#" -eq 1 ] || usage; cmd_install ;;
    verify) [ "$#" -eq 1 ] || usage; cmd_verify ;;
    resolve) [ "$#" -eq 2 ] || usage; cmd_resolve "$2" ;;
    *) usage ;;
esac
