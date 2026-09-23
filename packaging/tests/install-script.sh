#!/bin/sh
set -eu
umask 022

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/mqtt-raspberry-controller-installer-test.XXXXXX")
trap 'rm -rf "$TMP"' 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) printf 'installer test unsupported on architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac

if command -v setsid >/dev/null 2>&1; then
    if setsid sh -c '(exec 3<>/dev/tty) 2>/dev/null'; then
        printf 'detached terminal probe incorrectly found a controlling TTY\n' >&2
        exit 1
    fi
fi

create_release() {
    version=$1
    unsafe=${2:-false}
    tag_dir=$TMP/releases/v$version
    base=mqtt-raspberry-controller_${version}_linux_${ARCH}
    package=$TMP/packages/$base
    rm -rf "$package"
    mkdir -p "$tag_dir" "$package/configs" "$package/packaging/sysusers.d" \
        "$package/packaging/systemd" "$package/packaging/udev"
    cat >"$package/mqtt-raspberry-controller" <<EOF
#!/bin/sh
[ "\${1:-}" = --version ] && { printf '%s\\n' '$version'; exit 0; }
[ "\${FAKE_CHECK_FAIL:-0}" = 1 ] && exit 1
for argument do
    if [ "\$argument" = --check ]; then
        if [ "\${REQUIRE_NON_ROOT_CHECK:-0}" = 1 ] && [ "\$(id -u)" -eq 0 ]; then
            exit 3
        fi
        exit 0
    fi
done
exit 2
$version
EOF
    chmod 0755 "$package/mqtt-raspberry-controller"
    cp "$ROOT/configs/config.example.yaml" "$package/configs/"
    cp "$ROOT/packaging/sysusers.d/mqtt-raspberry-controller.conf" "$package/packaging/sysusers.d/"
    cp "$ROOT/packaging/systemd/mqtt-raspberry-controller.service" "$package/packaging/systemd/"
    cp "$ROOT/packaging/udev/60-mqtt-raspberry-controller.rules" "$package/packaging/udev/"
    if [ "$unsafe" = true ]; then
        ln -s /etc/passwd "$package/unsafe-link"
    fi
    tar -C "$TMP/packages" -czf "$tag_dir/$base.tar.gz" "$base"
    (
        cd "$tag_dir"
        sha256sum "./$base.tar.gz" >SHA256SUMS
    )
}

create_release 1.2.3
stage=$TMP/stage
DESTDIR=$stage \
MQTT_RASPBERRY_VERSION=1.2.3 \
MQTT_RASPBERRY_TEST_ALLOW_FILE_URLS=1 \
MQTT_RASPBERRY_RELEASE_BASE_URL="file://$TMP/releases" \
    "$ROOT/scripts/install.sh" >/dev/null

test "$("$stage/usr/local/bin/mqtt-raspberry-controller" --version)" = 1.2.3
test "$(stat -c %a "$stage/usr/local/bin/mqtt-raspberry-controller")" = 755
test "$(stat -c %a "$stage/etc/mqtt-raspberry-controller/config.yaml")" = 640
test "$(stat -c %a "$stage/etc/mqtt-raspberry-controller/mqtt-password")" = 600
test -f "$stage/usr/lib/sysusers.d/mqtt-raspberry-controller.conf"
test -f "$stage/etc/systemd/system/mqtt-raspberry-controller.service"
test -f "$stage/etc/udev/rules.d/60-mqtt-raspberry-controller.rules"

printf 'preserve-existing-config\n' >"$stage/etc/mqtt-raspberry-controller/config.yaml"
printf 'preserve-existing-password\n' >"$stage/etc/mqtt-raspberry-controller/mqtt-password"
chmod 0600 "$stage/etc/mqtt-raspberry-controller/mqtt-password"
DESTDIR=$stage \
MQTT_RASPBERRY_VERSION=v1.2.3 \
MQTT_RASPBERRY_TEST_ALLOW_FILE_URLS=1 \
MQTT_RASPBERRY_RELEASE_BASE_URL="file://$TMP/releases" \
    "$ROOT/scripts/install.sh" >/dev/null
grep -Fqx 'preserve-existing-config' "$stage/etc/mqtt-raspberry-controller/config.yaml"
grep -Fqx 'preserve-existing-password' "$stage/etc/mqtt-raspberry-controller/mqtt-password"

wizard_driver=$TMP/run-install-wizard.py
cat >"$wizard_driver" <<'PY'
import errno
import os
import pty
import sys

script, stage, release_base, version, should_fail, expect_instance = sys.argv[1:]
env = os.environ.copy()
env.update({
    "DESTDIR": stage,
    "MQTT_RASPBERRY_VERSION": version,
    "MQTT_RASPBERRY_TEST_ALLOW_FILE_URLS": "1",
    "MQTT_RASPBERRY_RELEASE_BASE_URL": release_base,
    "FAKE_CHECK_FAIL": should_fail,
    "REQUIRE_NON_ROOT_CHECK": "1",
})
steps = [
    (b"MQTT broker URL", b"mqtt://192.168.1.163:1883\n"),
    (b"Allow insecure MQTT transport?", b"n\n"),
    (b"MQTT broker URL", b"mqtt://192.168.1.163:1883\n"),
    (b"Allow insecure MQTT transport?", b"y\n"),
    (b"MQTT username", b"mqtt'user\n"),
]
if expect_instance == "1":
    steps.append((b"Instance identifier", b"HyperHDR\n"))
steps.extend([
    (b"MQTT password:", b"s3cr'et\n"),
    (b"Confirm MQTT password:", b"s3cr'et\n"),
])
pid, fd = pty.fork()
if pid == 0:
    os.execve(script, [script, "--configure"], env)

buffer = b""
try:
    for marker, response in steps:
        while marker not in buffer:
            chunk = os.read(fd, 4096)
            if not chunk:
                raise RuntimeError(f"installer exited before prompt {marker!r}")
            buffer += chunk
        buffer = buffer.split(marker, 1)[1]
        os.write(fd, response)
    while True:
        try:
            chunk = os.read(fd, 4096)
            if not chunk:
                break
        except OSError as exc:
            if exc.errno == errno.EIO:
                break
            raise
finally:
    os.close(fd)

_, status = os.waitpid(pid, 0)
code = os.waitstatus_to_exitcode(status)
if should_fail == "1":
    sys.exit(0 if code != 0 else 1)
sys.exit(code)
PY

wizard_stage=$TMP/wizard-stage
python3 "$wizard_driver" "$ROOT/scripts/install.sh" "$wizard_stage" \
    "file://$TMP/releases" 1.2.3 0 1
grep -Fqx "  broker: 'mqtt://192.168.1.163:1883'" "$wizard_stage/etc/mqtt-raspberry-controller/config.yaml"
grep -Fqx "  username: 'mqtt''user'" "$wizard_stage/etc/mqtt-raspberry-controller/config.yaml"
grep -Fqx '  allow_insecure_transport: true' "$wizard_stage/etc/mqtt-raspberry-controller/config.yaml"
grep -Fqx '  tls: {}' "$wizard_stage/etc/mqtt-raspberry-controller/config.yaml"
if grep -Fq '    min_version:' "$wizard_stage/etc/mqtt-raspberry-controller/config.yaml"; then
    printf 'plaintext wizard preserved an incompatible MQTT TLS option\n' >&2
    exit 1
fi
grep -Fqx "  client_id: 'mqtt-raspberry-controller-hyperhdr'" "$wizard_stage/etc/mqtt-raspberry-controller/config.yaml"
grep -Fqx "  base_topic: 'mqtt-raspberry-controller/hyperhdr'" "$wizard_stage/etc/mqtt-raspberry-controller/config.yaml"
grep -Fqx "  id: 'mqtt-raspberry-controller-hyperhdr'" "$wizard_stage/etc/mqtt-raspberry-controller/config.yaml"
grep -Fqx "  name: 'MQTT Raspberry Controller hyperhdr'" "$wizard_stage/etc/mqtt-raspberry-controller/config.yaml"
grep -Fqx "s3cr'et" "$wizard_stage/etc/mqtt-raspberry-controller/mqtt-password"
test "$(stat -c %a "$wizard_stage/etc/mqtt-raspberry-controller/config.yaml")" = 640
test "$(stat -c %a "$wizard_stage/etc/mqtt-raspberry-controller/mqtt-password")" = 600
real_validator=$TMP/mqtt-raspberry-controller-validator
(
    cd "$ROOT"
    go build -trimpath -o "$real_validator" ./cmd/mqtt-raspberry-controller
)
MQTT_PASSWORD_FILE=$wizard_stage/etc/mqtt-raspberry-controller/mqtt-password \
    "$real_validator" --config "$wizard_stage/etc/mqtt-raspberry-controller/config.yaml" --check

wizard_failed_stage=$TMP/wizard-failed-stage
python3 "$wizard_driver" "$ROOT/scripts/install.sh" "$wizard_failed_stage" \
    "file://$TMP/releases" 1.2.3 1 1
grep -Fqx '  broker: tls://mqtt.example.net:8883' "$wizard_failed_stage/etc/mqtt-raspberry-controller/config.yaml"
test ! -s "$wizard_failed_stage/etc/mqtt-raspberry-controller/mqtt-password"

noncanonical_stage=$TMP/noncanonical-stage
DESTDIR=$noncanonical_stage \
MQTT_RASPBERRY_VERSION=1.2.3 \
MQTT_RASPBERRY_TEST_ALLOW_FILE_URLS=1 \
MQTT_RASPBERRY_RELEASE_BASE_URL="file://$TMP/releases" \
    "$ROOT/scripts/install.sh" --no-configure >/dev/null
python3 - "$noncanonical_stage/etc/mqtt-raspberry-controller/config.yaml" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
text = path.read_text()
text = text.replace("  broker: tls://mqtt.example.net:8883", '  "broker": tls://old.example.net:8883')
text = text.replace("  username: mqtt-raspberry-controller", '  "username": old-user')
path.write_text(text)
PY
printf 'old-password\n' >"$noncanonical_stage/etc/mqtt-raspberry-controller/mqtt-password"
python3 "$wizard_driver" "$ROOT/scripts/install.sh" "$noncanonical_stage" \
    "file://$TMP/releases" 1.2.3 1 0
grep -Fqx '  "broker": tls://old.example.net:8883' "$noncanonical_stage/etc/mqtt-raspberry-controller/config.yaml"
grep -Fqx '  "username": old-user' "$noncanonical_stage/etc/mqtt-raspberry-controller/config.yaml"
grep -Fqx 'old-password' "$noncanonical_stage/etc/mqtt-raspberry-controller/mqtt-password"

python3 - "$ROOT/scripts/install.sh" "$TMP/signal-stage" "file://$TMP/releases" <<'PY'
import errno
import os
import pty
import signal
import sys
import termios
import time

script, stage, release_base = sys.argv[1:]
env = os.environ.copy()
env.update({
    "DESTDIR": stage,
    "MQTT_RASPBERRY_VERSION": "1.2.3",
    "MQTT_RASPBERRY_TEST_ALLOW_FILE_URLS": "1",
    "MQTT_RASPBERRY_RELEASE_BASE_URL": release_base,
})
pid, fd = pty.fork()
if pid == 0:
    os.execve(script, [script, "--configure"], env)

buffer = b""
for marker, response in [
    (b"MQTT broker URL", b"tls://broker.example.net:8883\n"),
    (b"MQTT username", b"user\n"),
    (b"Instance identifier", b"signal-test\n"),
]:
    while marker not in buffer:
        buffer += os.read(fd, 4096)
    buffer = buffer.split(marker, 1)[1]
    os.write(fd, response)
while b"MQTT password:" not in buffer:
    buffer += os.read(fd, 4096)
for _ in range(100):
    if not (termios.tcgetattr(fd)[3] & termios.ECHO):
        break
    time.sleep(0.01)
else:
    raise SystemExit("password input did not disable terminal echo")
os.killpg(pid, signal.SIGQUIT)
while True:
    try:
        if not os.read(fd, 4096):
            break
    except OSError as exc:
        if exc.errno == errno.EIO:
            break
        raise
_, status = os.waitpid(pid, 0)
if os.waitstatus_to_exitcode(status) == 0:
    raise SystemExit("SIGQUIT did not terminate the installer")
if not (termios.tcgetattr(fd)[3] & termios.ECHO):
    raise SystemExit("terminal echo remained disabled after SIGQUIT")
os.close(fd)
PY

create_release 1.2.4
printf '%064d  mqtt-raspberry-controller_1.2.4_linux_%s.tar.gz\n' 0 "$ARCH" \
    >"$TMP/releases/v1.2.4/SHA256SUMS"
if DESTDIR=$TMP/bad-checksum \
    MQTT_RASPBERRY_VERSION=1.2.4 \
    MQTT_RASPBERRY_TEST_ALLOW_FILE_URLS=1 \
    MQTT_RASPBERRY_RELEASE_BASE_URL="file://$TMP/releases" \
    "$ROOT/scripts/install.sh" >/dev/null 2>&1
then
    printf 'installer accepted a release with an invalid checksum\n' >&2
    exit 1
fi

create_release 1.2.5 true
if DESTDIR=$TMP/bad-link \
    MQTT_RASPBERRY_VERSION=1.2.5 \
    MQTT_RASPBERRY_TEST_ALLOW_FILE_URLS=1 \
    MQTT_RASPBERRY_RELEASE_BASE_URL="file://$TMP/releases" \
    "$ROOT/scripts/install.sh" >/dev/null 2>&1
then
    printf 'installer accepted an archive containing a symlink\n' >&2
    exit 1
fi

if DESTDIR=$TMP/bad-version \
    MQTT_RASPBERRY_VERSION=1.2.3-01 \
    MQTT_RASPBERRY_TEST_ALLOW_FILE_URLS=1 \
    MQTT_RASPBERRY_RELEASE_BASE_URL="file://$TMP/releases" \
    "$ROOT/scripts/install.sh" >/dev/null 2>&1
then
    printf 'installer accepted invalid SemVer\n' >&2
    exit 1
fi

# Exercise the non-root bootstrap without granting privileges. The mock sudo
# verifies that the exact tagged script and its hash are handed across the
# privilege boundary before any release artifact download can occur.
if [ "$(id -u)" -ne 0 ]; then
    mock_bin=$TMP/mock-bin
    mock_log=$TMP/bootstrap.log
    mkdir -p "$mock_bin" "$TMP/bootstrap-tmp"
    cat >"$mock_bin/curl" <<'EOF'
#!/bin/sh
out=
url=
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o) out=$2; shift 2 ;;
        http://*|https://*|file://*) url=$1; shift ;;
        *) shift ;;
    esac
done
printf 'curl=%s\n' "$url" >>"$MOCK_LOG"
case "$url" in
    */refs/tags/v1.2.3/scripts/install.sh)
        cp "$INSTALLER_SOURCE" "$out"
        ;;
    *)
        printf 'unexpected bootstrap download: %s\n' "$url" >&2
        exit 1
        ;;
esac
EOF
    cat >"$mock_bin/sudo" <<'EOF'
#!/bin/sh
[ "$1" = sh ] && [ "$2" = -c ] && [ "$4" = sh ]
source_script=$5
expected_hash=$6
version=$7
configure_mode=$9
actual_hash=$(sha256sum "$source_script" | awk '{print $1}')
[ "$actual_hash" = "$expected_hash" ]
cmp -s "$source_script" "$INSTALLER_SOURCE"
printf 'sudo-version=%s\n' "$version" >>"$MOCK_LOG"
printf 'sudo-configure=%s\n' "$configure_mode" >>"$MOCK_LOG"
EOF
    chmod 0755 "$mock_bin/curl" "$mock_bin/sudo"
    PATH="$mock_bin:$PATH" \
    TMPDIR=$TMP/bootstrap-tmp \
    MOCK_LOG=$mock_log \
    INSTALLER_SOURCE=$ROOT/scripts/install.sh \
    MQTT_RASPBERRY_VERSION=1.2.3 \
        "$ROOT/scripts/install.sh" --configure >/dev/null
    grep -Fqx 'curl=https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/tags/v1.2.3/scripts/install.sh' "$mock_log"
    grep -Fqx 'sudo-version=1.2.3' "$mock_log"
    grep -Fqx 'sudo-configure=1' "$mock_log"
    test "$(grep -c '^curl=' "$mock_log")" = 1
fi

printf 'installer tests passed\n'
