#!/bin/sh
set -eu

VERSION=${1:-}
DIST=${2:-dist}
case "$VERSION" in '') printf 'usage: %s VERSION [DIST]\n' "$0" >&2; exit 2 ;; esac
command -v file >/dev/null 2>&1 || { printf 'missing required command: file\n' >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { printf 'missing required command: python3\n' >&2; exit 1; }

(
    cd "$DIST"
    sha256sum --check SHA256SUMS
)
for arch in amd64 arm64; do
    base="mqtt-raspberry-controller_${VERSION}_linux_${arch}"
    archive="$DIST/$base.tar.gz"
    sbom="$DIST/$base.sbom.cdx.json"
    version_record="$DIST/$base.version.txt"
    [ -s "$archive" ] && [ -s "$sbom" ] && [ -s "$version_record" ]
    for required in LICENSE README.md configs/config.example.yaml docs/INSTALL.md packaging/systemd/mqtt-raspberry-controller.service; do
        tar -tzf "$archive" | grep -Fqx "$base/$required"
    done
    grep -Fqx "$VERSION" "$version_record"
    actual=$(tar -xOf "$archive" "$base/mqtt-raspberry-controller" | file -b -)
    case "$arch:$actual" in
        amd64:*x86-64*) : ;;
        arm64:*ARM\ aarch64*) : ;;
        *) printf 'unexpected %s binary architecture: %s\n' "$arch" "$actual" >&2; exit 1 ;;
    esac
    tar -xOf "$archive" "$base/mqtt-raspberry-controller" | strings | grep -Fqx "$VERSION"
    python3 - "$sbom" "$archive" "$base" "$VERSION" <<'PY'
import hashlib
import json
import sys
import tarfile

sbom_path, archive_path, base, version = sys.argv[1:]
with open(sbom_path, encoding="utf-8") as stream:
    document = json.load(stream)
assert document.get("bomFormat") == "CycloneDX"
assert document.get("specVersion")
metadata = document.get("metadata", {}).get("component", {})
assert metadata.get("name") == "mqtt-raspberry-controller"
assert metadata.get("version") == version
components = {component.get("name"): component for component in document.get("components", [])}
required = {
    "github.com/AC-CodeProd/mqtt-raspberry-controller",
    "github.com/eclipse/paho.mqtt.golang",
    "github.com/warthog618/go-gpiocdev",
    "go.yaml.in/yaml/v3",
    "stdlib",
    "mqtt-raspberry-controller",
}
assert required <= components.keys()
for name in required - {"mqtt-raspberry-controller"}:
    assert components[name].get("version") not in (None, "", "UNKNOWN")
assert components["github.com/AC-CodeProd/mqtt-raspberry-controller"]["version"] == version
with tarfile.open(archive_path, "r:gz") as archive:
    binary = archive.extractfile(f"{base}/mqtt-raspberry-controller")
    assert binary is not None
    digest = hashlib.sha256(binary.read()).hexdigest()
hashes = {entry["alg"]: entry["content"].lower() for entry in components["mqtt-raspberry-controller"].get("hashes", [])}
assert hashes.get("SHA-256") == digest
PY
done
[ -s "$DIST/RELEASE-VERIFICATION.txt" ]
grep -Fqx 'Physical GPIO / Raspberry Pi smoke test: NOT RUN' "$DIST/RELEASE-VERIFICATION.txt"
printf 'release verification passed for %s\n' "$VERSION"
