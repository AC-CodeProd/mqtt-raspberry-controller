#!/bin/sh
set -eu

version=${1:-}
fail() {
    printf 'version must be strict SemVer without a leading v: %s\n' "$version" >&2
    exit 2
}

printf '%s\n' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$' || fail

case "$version" in
    *-*)
        prerelease=${version#*-}
        prerelease=${prerelease%%+*}
        old_ifs=$IFS
        IFS=.
        set -- $prerelease
        IFS=$old_ifs
        for identifier do
            case "$identifier" in
                *[!0-9]*) ;;
                0) ;;
                0*) fail ;;
                *) ;;
            esac
        done
        ;;
esac
