#!/bin/sh
set -eu
umask 022

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/mqtt-raspberry-controller-uninstaller-test.XXXXXX")
trap 'rm -rf "$TMP"' 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

populate_stage() {
    stage=$1
    mkdir -p \
        "$stage/usr/local/bin" \
        "$stage/usr/lib/sysusers.d" \
        "$stage/etc/systemd/system" \
        "$stage/etc/udev/rules.d" \
        "$stage/etc/mqtt-raspberry-controller" \
        "$stage/var/lib/mqtt-raspberry-controller"
    printf 'binary\n' >"$stage/usr/local/bin/mqtt-raspberry-controller"
    printf 'sysusers\n' >"$stage/usr/lib/sysusers.d/mqtt-raspberry-controller.conf"
    printf 'service\n' >"$stage/etc/systemd/system/mqtt-raspberry-controller.service"
    printf 'udev\n' >"$stage/etc/udev/rules.d/60-mqtt-raspberry-controller.rules"
    printf 'config\n' >"$stage/etc/mqtt-raspberry-controller/config.yaml"
    printf 'password\n' >"$stage/etc/mqtt-raspberry-controller/mqtt-password"
    printf 'state\n' >"$stage/var/lib/mqtt-raspberry-controller/discovery-state.json"
}

stage=$TMP/remove-stage
populate_stage "$stage"
DESTDIR=$stage "$ROOT/scripts/uninstall.sh" --remove >/dev/null
test ! -e "$stage/usr/local/bin/mqtt-raspberry-controller"
test ! -e "$stage/usr/lib/sysusers.d/mqtt-raspberry-controller.conf"
test ! -e "$stage/etc/systemd/system/mqtt-raspberry-controller.service"
test ! -e "$stage/etc/udev/rules.d/60-mqtt-raspberry-controller.rules"
grep -Fqx config "$stage/etc/mqtt-raspberry-controller/config.yaml"
grep -Fqx password "$stage/etc/mqtt-raspberry-controller/mqtt-password"
grep -Fqx state "$stage/var/lib/mqtt-raspberry-controller/discovery-state.json"

stage=$TMP/clear-stage
populate_stage "$stage"
DESTDIR=$stage "$ROOT/scripts/uninstall.sh" --clear >/dev/null
test ! -e "$stage/usr/local/bin/mqtt-raspberry-controller"
test ! -e "$stage/usr/lib/sysusers.d/mqtt-raspberry-controller.conf"
test ! -e "$stage/etc/systemd/system/mqtt-raspberry-controller.service"
test ! -e "$stage/etc/udev/rules.d/60-mqtt-raspberry-controller.rules"
test ! -e "$stage/etc/mqtt-raspberry-controller"
test ! -e "$stage/var/lib/mqtt-raspberry-controller"

DESTDIR=$TMP/default-stage "$ROOT/scripts/uninstall.sh" --help >/dev/null
if DESTDIR=$TMP/bad-stage "$ROOT/scripts/uninstall.sh" --unknown >/dev/null 2>&1; then
    printf 'uninstaller accepted an unknown option\n' >&2
    exit 1
fi
if DESTDIR=$TMP/bad-stage "$ROOT/scripts/uninstall.sh" --remove --clear >/dev/null 2>&1; then
    printf 'uninstaller accepted multiple modes\n' >&2
    exit 1
fi

# Exercise the privileged file-removal, service-state, rollback, and clear paths
# inside DESTDIR with instrumented system tools.
priv_bin=$TMP/privileged-bin
priv_state=$TMP/privileged-state
mkdir -p "$priv_bin" "$priv_state"
cat >"$priv_bin/systemctl" <<'EOF'
#!/bin/sh
command=$1
shift
case "$command" in
    is-enabled) test -e "$SYSTEMCTL_STATE/enabled" ;;
    is-active) test -e "$SYSTEMCTL_STATE/active" ;;
    stop)
        test ! -e "$SYSTEMCTL_STATE/fail-stop" || exit 1
        rm -f "$SYSTEMCTL_STATE/active"
        ;;
    disable)
        test ! -e "$SYSTEMCTL_STATE/fail-disable" || exit 1
        rm -f "$SYSTEMCTL_STATE/enabled"
        ;;
    daemon-reload)
        if [ -e "$SYSTEMCTL_STATE/fail-reload-once" ]; then
            rm -f "$SYSTEMCTL_STATE/fail-reload-once"
            exit 1
        fi
        ;;
    enable) touch "$SYSTEMCTL_STATE/enabled" ;;
    start) touch "$SYSTEMCTL_STATE/active" ;;
    reset-failed) : ;;
    *) printf 'unexpected systemctl command: %s\n' "$command" >&2; exit 1 ;;
esac
EOF
cat >"$priv_bin/udevadm" <<'EOF'
#!/bin/sh
exit 0
EOF
cat >"$priv_bin/id" <<'EOF'
#!/bin/sh
if [ "${1:-}" = mqtt-raspberry-controller ]; then
    test -e "$IDENTITY_STATE/user"
else
    exec /usr/bin/id "$@"
fi
EOF
cat >"$priv_bin/getent" <<'EOF'
#!/bin/sh
[ "${1:-}" = group ] && [ "${2:-}" = mqtt-raspberry-controller ] && test -e "$IDENTITY_STATE/group"
EOF
cat >"$priv_bin/userdel" <<'EOF'
#!/bin/sh
test "${1:-}" = mqtt-raspberry-controller
[ "${FAIL_USERDEL:-0}" = 0 ] || exit 1
rm -f "$IDENTITY_STATE/user"
EOF
cat >"$priv_bin/groupdel" <<'EOF'
#!/bin/sh
test "${1:-}" = mqtt-raspberry-controller
rm -f "$IDENTITY_STATE/group"
EOF
chmod 0755 "$priv_bin"/*

stage=$TMP/privileged-remove
populate_stage "$stage"
touch "$priv_state/active" "$priv_state/enabled"
PATH="$priv_bin:$PATH" SYSTEMCTL_STATE=$priv_state IDENTITY_STATE=$priv_state \
TMPDIR=$TMP MQTT_RASPBERRY_TEST_PRIVILEGED=1 DESTDIR=$stage \
    "$ROOT/scripts/uninstall.sh" --remove >/dev/null
test ! -e "$stage/usr/local/bin/mqtt-raspberry-controller"
test -e "$stage/etc/mqtt-raspberry-controller/config.yaml"
test ! -e "$priv_state/active"
test ! -e "$priv_state/enabled"

rm -rf "$priv_state"
mkdir -p "$priv_state"
stage=$TMP/privileged-rollback
populate_stage "$stage"
touch "$priv_state/active" "$priv_state/enabled" "$priv_state/fail-reload-once"
if PATH="$priv_bin:$PATH" SYSTEMCTL_STATE=$priv_state IDENTITY_STATE=$priv_state \
    TMPDIR=$TMP MQTT_RASPBERRY_TEST_PRIVILEGED=1 DESTDIR=$stage \
    "$ROOT/scripts/uninstall.sh" --remove >/dev/null 2>&1
then
    printf 'privileged uninstall did not fail at the injected reload error\n' >&2
    exit 1
fi
grep -Fqx binary "$stage/usr/local/bin/mqtt-raspberry-controller"
grep -Fqx service "$stage/etc/systemd/system/mqtt-raspberry-controller.service"
test -e "$priv_state/active"
test -e "$priv_state/enabled"

rm -rf "$priv_state"
mkdir -p "$priv_state"
stage=$TMP/privileged-clear
populate_stage "$stage"
touch "$priv_state/user" "$priv_state/group"
PATH="$priv_bin:$PATH" SYSTEMCTL_STATE=$priv_state IDENTITY_STATE=$priv_state \
TMPDIR=$TMP MQTT_RASPBERRY_TEST_PRIVILEGED=1 DESTDIR=$stage \
    "$ROOT/scripts/uninstall.sh" --clear >/dev/null
test ! -e "$stage/etc/mqtt-raspberry-controller"
test ! -e "$stage/var/lib/mqtt-raspberry-controller"
test ! -e "$priv_state/user"
test ! -e "$priv_state/group"

rm -rf "$priv_state"
mkdir -p "$priv_state"
stage=$TMP/privileged-clear-failure
populate_stage "$stage"
touch "$priv_state/user" "$priv_state/group"
if PATH="$priv_bin:$PATH" SYSTEMCTL_STATE=$priv_state IDENTITY_STATE=$priv_state FAIL_USERDEL=1 \
    TMPDIR=$TMP MQTT_RASPBERRY_TEST_PRIVILEGED=1 DESTDIR=$stage \
    "$ROOT/scripts/uninstall.sh" --clear >/dev/null 2>&1
then
    printf 'clear uninstall ignored a service-user removal failure\n' >&2
    exit 1
fi
test -e "$stage/etc/mqtt-raspberry-controller/config.yaml"
test -e "$stage/var/lib/mqtt-raspberry-controller/discovery-state.json"
test -e "$priv_state/user"
test -e "$priv_state/group"

# Verify that the non-root entrypoint fetches the script from the selected tag,
# passes its exact hash to sudo, and preserves the requested mode.
if [ "$(id -u)" -ne 0 ]; then
    mock_bin=$TMP/mock-bin
    mock_log=$TMP/bootstrap.log
    probe=$TMP/tagged-uninstall-probe.sh
    mkdir -p "$mock_bin" "$TMP/bootstrap-tmp"
    cat >"$probe" <<'EOF'
#!/bin/sh
mode=$1
dir=$(dirname "$0")
test "$(stat -c %a "$dir")" = 700
printf 'probe-mode=%s\n' "$mode" >>"$MOCK_LOG"
printf 'probe-dir-mode=700\n' >>"$MOCK_LOG"
EOF
    chmod 0755 "$probe"
    cat >"$mock_bin/curl" <<'EOF'
#!/bin/sh
out=
url=
while [ "$#" -gt 0 ]; do
    case "$1" in
        -o) out=$2; shift 2 ;;
        http://*|https://*) url=$1; shift ;;
        *) shift ;;
    esac
done
printf 'curl=%s\n' "$url" >>"$MOCK_LOG"
case "$url" in
    */refs/tags/v1.2.3/scripts/uninstall.sh)
        cp "$TAGGED_PROBE" "$out"
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
"$@"
EOF
    chmod 0755 "$mock_bin/curl" "$mock_bin/sudo"
    PATH="$mock_bin:$PATH" \
    TMPDIR=$TMP/bootstrap-tmp \
    MOCK_LOG=$mock_log \
    TAGGED_PROBE=$probe \
    MQTT_RASPBERRY_VERSION=1.2.3 \
        "$ROOT/scripts/uninstall.sh" --clear >/dev/null
    grep -Fqx 'curl=https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/tags/v1.2.3/scripts/uninstall.sh' "$mock_log"
    grep -Fqx 'probe-mode=--clear' "$mock_log"
    grep -Fqx 'probe-dir-mode=700' "$mock_log"
    test "$(grep -c '^curl=' "$mock_log")" = 1
fi

printf 'uninstaller tests passed\n'
