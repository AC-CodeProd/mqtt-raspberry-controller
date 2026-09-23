#!/bin/sh
set -eu

VERSION=1.43.0
DESTINATION=${1:-"${TMPDIR:-/tmp}/syft-$VERSION"}
BASE_URL="https://github.com/anchore/syft/releases/download/v$VERSION"

case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) printf 'unsupported Syft host architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac

need() { command -v "$1" >/dev/null 2>&1 || { printf 'missing required command: %s\n' "$1" >&2; exit 1; }; }
need curl
need sha256sum
need tar

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
archive="syft_${VERSION}_linux_${arch}.tar.gz"
curl --fail --location --proto '=https' --tlsv1.2 --output "$tmp/$archive" "$BASE_URL/$archive"
curl --fail --location --proto '=https' --tlsv1.2 --output "$tmp/checksums.txt" "$BASE_URL/syft_${VERSION}_checksums.txt"
(
    cd "$tmp"
    grep "  $archive\$" checksums.txt >selected-checksum.txt
    test -s selected-checksum.txt
    sha256sum --check selected-checksum.txt
)
mkdir -p "$DESTINATION"
tar -xzf "$tmp/$archive" -C "$DESTINATION" syft
"$DESTINATION/syft" version | grep -Eq "^Version:[[:space:]]+$VERSION$"
printf '%s\n' "$DESTINATION/syft"
