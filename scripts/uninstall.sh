#!/bin/sh
set -eu
umask 022

REPOSITORY=AC-CodeProd/mqtt-raspberry-controller
PROJECT=mqtt-raspberry-controller
REPOSITORY_URL=https://github.com/$REPOSITORY
LATEST_URL=$REPOSITORY_URL/releases/latest
RAW_BASE_URL=https://raw.githubusercontent.com/$REPOSITORY
VERSION=${MQTT_RASPBERRY_VERSION:-}
DESTDIR=${DESTDIR:-}
TEST_PRIVILEGED=${MQTT_RASPBERRY_TEST_PRIVILEGED:-0}
MODE=remove
TMP_DIR=
ROLLBACK_ACTIVE=false
BACKUP_DIR=
WAS_ENABLED=false
WAS_ACTIVE=false

info() {
    printf '%s\n' "$*"
}

warn() {
    printf 'mqtt-raspberry-controller uninstaller: warning: %s\n' "$*" >&2
}

fail() {
    printf 'mqtt-raspberry-controller uninstaller: %s\n' "$*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
usage: uninstall.sh [--remove|--clear]

  --remove  Remove the program and service files, preserving configuration,
            credentials, state, and the service account (default).
  --clear   Remove everything handled by --remove, then permanently delete the
            default configuration, credentials, state, service user and group.
EOF
}

case "${1:-}" in
    ''|--remove) MODE=remove ;;
    --clear) MODE=clear ;;
    -h|--help) usage; exit 0 ;;
    *) usage >&2; fail "unknown option: $1" ;;
esac
[ "$#" -le 1 ] || { usage >&2; fail 'only one uninstall mode may be selected'; }

case "$DESTDIR" in
    ''|/*) ;;
    *) fail 'DESTDIR must be an absolute path' ;;
esac
case "$TEST_PRIVILEGED" in
    0) ;;
    1) [ -n "$DESTDIR" ] || fail 'MQTT_RASPBERRY_TEST_PRIVILEGED requires DESTDIR' ;;
    *) fail 'MQTT_RASPBERRY_TEST_PRIVILEGED must be 0 or 1' ;;
esac

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

curl_secure() {
    curl --proto '=https' --proto-redir '=https' "$@"
}

restore_target() {
    target=$1
    name=$2
    if [ -e "$BACKUP_DIR/$name" ] || [ -L "$BACKUP_DIR/$name" ]; then
        rm -rf "$target"
        cp -a "$BACKUP_DIR/$name" "$target"
    fi
}

rollback() {
    warn 'uninstall failed; restoring removed program and service files'
    restore_target "$BIN_TARGET" binary
    restore_target "$SYSUSERS_TARGET" sysusers
    restore_target "$SERVICE_TARGET" service
    restore_target "$UDEV_TARGET" udev
    systemctl daemon-reload >/dev/null 2>&1 || true
    if [ "$WAS_ENABLED" = true ]; then
        systemctl enable "$PROJECT.service" >/dev/null 2>&1 || true
    fi
    if [ "$WAS_ACTIVE" = true ]; then
        systemctl start "$PROJECT.service" >/dev/null 2>&1 || true
    fi
    if command -v udevadm >/dev/null 2>&1; then
        udevadm control --reload-rules >/dev/null 2>&1 || true
    fi
}

finish() {
    status=$?
    trap - 0 HUP INT TERM
    if [ "$status" -ne 0 ] && [ "$ROLLBACK_ACTIVE" = true ]; then
        rollback || true
    fi
    if [ -n "$TMP_DIR" ]; then
        rm -rf "$TMP_DIR"
    fi
    exit "$status"
}
trap finish 0
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

for command in id rm uname chmod; do
    require_command "$command"
done

case "$(uname -s)" in
    Linux) ;;
    *) fail 'only Linux is supported' ;;
esac

# The public one-liner starts unprivileged. Resolve the latest published tag (or
# use MQTT_RASPBERRY_VERSION), fetch the uninstaller from that tag, and pass only a
# hash-verified root-owned copy across the sudo boundary.
if [ -z "$DESTDIR" ] && [ "$(id -u)" -ne 0 ]; then
    for command in curl sudo sha256sum awk grep mktemp install; do
        require_command "$command"
    done
    if [ -n "$VERSION" ]; then
        VERSION=${VERSION#v}
        TAG=v$VERSION
    else
        info 'Resolving the latest published release...'
        EFFECTIVE_URL=$(curl_secure -fsSL -o /dev/null -w '%{url_effective}' "$LATEST_URL") || \
            fail 'could not resolve the latest published release'
        TAG=${EFFECTIVE_URL##*/}
        case "$TAG" in
            v*) VERSION=${TAG#v} ;;
            *) fail "unexpected latest release URL: $EFFECTIVE_URL" ;;
        esac
    fi
    printf '%s\n' "$VERSION" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$' || \
        fail "invalid release version: $VERSION"
    case "$VERSION" in
        *-*)
            PRERELEASE=${VERSION#*-}
            PRERELEASE=${PRERELEASE%%+*}
            OLD_IFS=$IFS
            IFS=.
            set -- $PRERELEASE
            IFS=$OLD_IFS
            for IDENTIFIER do
                case "$IDENTIFIER" in
                    *[!0-9]*|0) ;;
                    0*) fail "invalid release version: $VERSION" ;;
                esac
            done
            ;;
    esac

    TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/${PROJECT}-uninstall-bootstrap.XXXXXX")
    BOOTSTRAP=$TMP_DIR/uninstall.sh
    TAGGED_UNINSTALLER_URL=$RAW_BASE_URL/refs/tags/$TAG/scripts/uninstall.sh
    info "Loading the versioned uninstaller from $TAG..."
    curl_secure -fsSL --retry 3 --retry-delay 1 -o "$BOOTSTRAP" "$TAGGED_UNINSTALLER_URL" || \
        fail "the $TAG release does not contain scripts/uninstall.sh"
    BOOTSTRAP_HASH=$(sha256sum "$BOOTSTRAP" | awk '{print $1}')
    sudo sh -c '
        set -eu
        PATH=/usr/sbin:/usr/bin:/sbin:/bin
        export PATH
        source_script=$1
        expected_hash=$2
        mode=$3
        root_dir=$(mktemp -d /var/tmp/mqtt-raspberry-controller-uninstall.XXXXXX)
        chmod 0700 "$root_dir"
        cleanup_root() { rm -rf "$root_dir"; }
        trap cleanup_root 0
        trap "exit 129" HUP
        trap "exit 130" INT
        trap "exit 143" TERM
        install -m 0700 "$source_script" "$root_dir/uninstall.sh"
        actual_hash=$(sha256sum "$root_dir/uninstall.sh" | awk "{print \$1}")
        [ "$actual_hash" = "$expected_hash" ] || {
            printf "%s\n" "versioned uninstaller changed before privilege escalation" >&2
            exit 1
        }
        sh "$root_dir/uninstall.sh" "$mode"
    ' sh "$BOOTSTRAP" "$BOOTSTRAP_HASH" "--$MODE"
    exit 0
fi

PREFIX=$DESTDIR
BIN_TARGET=$PREFIX/usr/local/bin/$PROJECT
CONFIG_DIR=$PREFIX/etc/mqtt-raspberry-controller
STATE_DIR=$PREFIX/var/lib/mqtt-raspberry-controller
SYSUSERS_TARGET=$PREFIX/usr/lib/sysusers.d/mqtt-raspberry-controller.conf
SERVICE_TARGET=$PREFIX/etc/systemd/system/mqtt-raspberry-controller.service
UDEV_TARGET=$PREFIX/etc/udev/rules.d/60-mqtt-raspberry-controller.rules

if [ -n "$DESTDIR" ] && [ "$TEST_PRIVILEGED" = 0 ]; then
    rm -f "$BIN_TARGET" "$SYSUSERS_TARGET" "$SERVICE_TARGET" "$UDEV_TARGET"
    if [ "$MODE" = clear ]; then
        rm -rf "$CONFIG_DIR" "$STATE_DIR"
    fi
    info "Staged $MODE uninstall under $DESTDIR"
    exit 0
fi

if [ "$TEST_PRIVILEGED" = 0 ]; then
    [ "$(id -u)" -eq 0 ] || fail 'privileged uninstall must run as root'
fi
for command in systemctl cp mktemp install; do
    require_command "$command"
done
if [ "$MODE" = clear ]; then
    for command in getent userdel groupdel; do
        require_command "$command"
    done
fi

if [ "$TEST_PRIVILEGED" = 1 ]; then
    TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/${PROJECT}-uninstall.XXXXXX")
else
    TMP_DIR=$(mktemp -d "/var/tmp/${PROJECT}-uninstall.XXXXXX")
fi
chmod 0700 "$TMP_DIR"
BACKUP_DIR=$TMP_DIR/backup
install -d -m 0700 "$BACKUP_DIR"
for pair in \
    "$BIN_TARGET:binary" \
    "$SYSUSERS_TARGET:sysusers" \
    "$SERVICE_TARGET:service" \
    "$UDEV_TARGET:udev"
do
    target=${pair%:*}
    name=${pair##*:}
    if [ -e "$target" ] || [ -L "$target" ]; then
        cp -a "$target" "$BACKUP_DIR/$name"
    fi
done

if systemctl is-enabled --quiet "$PROJECT.service" >/dev/null 2>&1; then
    WAS_ENABLED=true
fi
if systemctl is-active --quiet "$PROJECT.service" >/dev/null 2>&1; then
    WAS_ACTIVE=true
fi
ROLLBACK_ACTIVE=true
if [ "$WAS_ACTIVE" = true ]; then
    systemctl stop "$PROJECT.service"
fi
if [ "$WAS_ENABLED" = true ]; then
    systemctl disable "$PROJECT.service"
fi
rm -f "$BIN_TARGET" "$SYSUSERS_TARGET" "$SERVICE_TARGET" "$UDEV_TARGET"
systemctl daemon-reload
if command -v udevadm >/dev/null 2>&1; then
    udevadm control --reload-rules
fi
systemctl reset-failed "$PROJECT.service" >/dev/null 2>&1 || true
ROLLBACK_ACTIVE=false

if [ "$MODE" = clear ]; then
    info 'Clearing default configuration, credentials, state, service user and service group...'
    if id "$PROJECT" >/dev/null 2>&1; then
        userdel "$PROJECT"
    fi
    if getent group "$PROJECT" >/dev/null 2>&1; then
        groupdel "$PROJECT"
    fi
    rm -rf "$CONFIG_DIR" "$STATE_DIR"
    info 'The shared gpio group was preserved.'
    info 'Custom configuration, credential, or state paths outside the default directories were not removed.'
else
    info "Preserved $CONFIG_DIR, $STATE_DIR and the service account."
fi

info "Completed $MODE uninstall of $PROJECT."
