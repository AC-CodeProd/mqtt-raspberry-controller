#!/bin/sh
set -eu
umask 022

REPOSITORY=AC-CodeProd/mqtt-raspberry-controller
PROJECT=mqtt-raspberry-controller
REPOSITORY_URL=https://github.com/$REPOSITORY
RELEASE_BASE_URL=${MQTT_RASPBERRY_RELEASE_BASE_URL:-$REPOSITORY_URL/releases/download}
LATEST_URL=${MQTT_RASPBERRY_LATEST_URL:-$REPOSITORY_URL/releases/latest}
RAW_BASE_URL=https://raw.githubusercontent.com/$REPOSITORY
VERSION=${MQTT_RASPBERRY_VERSION:-}
REQUIRE_ATTESTATION=${MQTT_RASPBERRY_REQUIRE_ATTESTATION:-0}
CONFIGURE_MODE=${MQTT_RASPBERRY_CONFIGURE:-auto}
DESTDIR=${DESTDIR:-}
TMP_DIR=
ROLLBACK_ACTIVE=false
BACKUP_DIR=
CONFIG_CREATED=false
PASSWORD_CREATED=false
CONFIGURED=false
TTY_ECHO_DISABLED=false
TTY_STATE=

info() {
    printf '%s\n' "$*"
}

warn() {
    printf 'mqtt-raspberry-controller installer: warning: %s\n' "$*" >&2
}

fail() {
    printf 'mqtt-raspberry-controller installer: %s\n' "$*" >&2
    exit 1
}

usage() {
    cat <<'EOF'
Usage: install.sh [--configure | --no-configure]

  --configure     require the interactive MQTT setup wizard
  --no-configure  install without prompting for MQTT settings

Without either option, a fresh interactive installation offers the wizard and
non-interactive installations keep the example configuration unchanged.
EOF
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --configure) CONFIGURE_MODE=1 ;;
        --no-configure) CONFIGURE_MODE=0 ;;
        -h|--help) usage; exit 0 ;;
        *) fail "unknown option: $1" ;;
    esac
    shift
done

case "$DESTDIR" in
    ''|/*) ;;
    *) fail 'DESTDIR must be an absolute path' ;;
esac
case "$REQUIRE_ATTESTATION" in
    0|1) ;;
    *) fail 'MQTT_RASPBERRY_REQUIRE_ATTESTATION must be 0 or 1' ;;
esac
case "$CONFIGURE_MODE" in
    auto|0|1) ;;
    *) fail 'MQTT_RASPBERRY_CONFIGURE must be auto, 0 or 1' ;;
esac

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

curl_secure() {
    if [ -n "$DESTDIR" ] && [ "${MQTT_RASPBERRY_TEST_ALLOW_FILE_URLS:-0}" = 1 ]; then
        curl "$@"
    else
        curl --proto '=https' --proto-redir '=https' "$@"
    fi
}

tty_available() {
    (exec 3<>/dev/tty) 2>/dev/null
}

tty_prompt() {
    printf '%b' "$*" >/dev/tty
}

yaml_quote() {
    sed "s/'/''/g"
}

restore_tty() {
    if [ "$TTY_ECHO_DISABLED" = true ] && [ -n "$TTY_STATE" ] && tty_available; then
        stty "$TTY_STATE" </dev/tty >/dev/null 2>&1 || true
    fi
}

suspend_for_tty() {
    restore_tty
    trap - TSTP
    kill -TSTP "$$"
    trap suspend_for_tty TSTP
    if [ "$TTY_ECHO_DISABLED" = true ] && tty_available; then
        stty -echo </dev/tty
    fi
}

configure_mqtt() {
    require_command sed
    require_command stty
    tty_available || fail 'interactive MQTT configuration requires a controlling terminal'

    if [ "$CONFIGURE_MODE" = auto ]; then
        while :; do
            tty_prompt 'Configure MQTT now? [Y/n] '
            IFS= read -r WIZARD_ANSWER </dev/tty || fail 'could not read from the terminal'
            case "$WIZARD_ANSWER" in
                ''|y|Y|yes|YES|Yes) break ;;
                n|N|no|NO|No)
                    info 'Skipped interactive MQTT configuration.'
                    return 0
                    ;;
                *) tty_prompt 'Please answer yes or no.\n' ;;
            esac
        done
    fi

    WIZARD_ALLOW_INSECURE=false
    while :; do
        tty_prompt 'MQTT broker URL (for example tls://broker.example.net:8883): '
        IFS= read -r WIZARD_BROKER </dev/tty || fail 'could not read the MQTT broker URL'
        if [ -z "$WIZARD_BROKER" ]; then
            tty_prompt 'The broker URL cannot be empty.\n'
            continue
        fi
        WIZARD_BROKER_SCHEME=$(printf '%s' "${WIZARD_BROKER%%:*}" | tr '[:upper:]' '[:lower:]')
        case "$WIZARD_BROKER_SCHEME" in
            mqtt|tcp|ws)
                tty_prompt 'Warning: this broker uses plaintext transport; credentials and messages can be intercepted.\n'
                while :; do
                    tty_prompt 'Allow insecure MQTT transport? [y/N] '
                    IFS= read -r WIZARD_INSECURE_ANSWER </dev/tty || \
                        fail 'could not read the insecure transport confirmation'
                    case "$WIZARD_INSECURE_ANSWER" in
                        y|Y|yes|YES|Yes)
                            WIZARD_ALLOW_INSECURE=true
                            break 2
                            ;;
                        ''|n|N|no|NO|No)
                            tty_prompt 'Choose a TLS broker URL or explicitly accept plaintext transport.\n'
                            break
                            ;;
                        *) tty_prompt 'Please answer yes or no.\n' ;;
                    esac
                done
                ;;
            *) break ;;
        esac
    done
    while :; do
        tty_prompt 'MQTT username: '
        IFS= read -r WIZARD_USERNAME </dev/tty || fail 'could not read the MQTT username'
        [ -n "$WIZARD_USERNAME" ] && break
        tty_prompt 'The MQTT username cannot be empty.\n'
    done

    WIZARD_INSTANCE=
    if [ "$CONFIG_CREATED" = true ]; then
        WIZARD_DEFAULT_INSTANCE=$(printf '%s' "$(uname -n)" | \
            tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9_-' '-')
        [ -n "$WIZARD_DEFAULT_INSTANCE" ] || WIZARD_DEFAULT_INSTANCE=device
        case "$WIZARD_DEFAULT_INSTANCE" in
            [a-z0-9]*) ;;
            *) WIZARD_DEFAULT_INSTANCE=device-$WIZARD_DEFAULT_INSTANCE ;;
        esac
        while :; do
            tty_prompt "Instance identifier [$WIZARD_DEFAULT_INSTANCE]: "
            IFS= read -r WIZARD_INSTANCE </dev/tty || fail 'could not read the instance identifier'
            [ -n "$WIZARD_INSTANCE" ] || WIZARD_INSTANCE=$WIZARD_DEFAULT_INSTANCE
            WIZARD_NORMALIZED_INSTANCE=$(printf '%s' "$WIZARD_INSTANCE" | tr '[:upper:]' '[:lower:]')
            if [ "$WIZARD_NORMALIZED_INSTANCE" != "$WIZARD_INSTANCE" ]; then
                tty_prompt "Normalized instance identifier to lowercase: $WIZARD_NORMALIZED_INSTANCE\n"
            fi
            WIZARD_INSTANCE=$WIZARD_NORMALIZED_INSTANCE
            case "$WIZARD_INSTANCE" in
                [a-z0-9]*)
                    case "$WIZARD_INSTANCE" in
                        *[!a-z0-9_-]*) tty_prompt 'Use only lowercase letters, digits, underscores and hyphens.\n' ;;
                        *) break ;;
                    esac
                    ;;
                *) tty_prompt 'The instance identifier must start with a lowercase letter or digit.\n' ;;
            esac
        done
    fi

    while :; do
        tty_prompt 'MQTT password: '
        TTY_STATE=$(stty -g </dev/tty) || fail 'could not read terminal settings'
        TTY_ECHO_DISABLED=true
        stty -echo </dev/tty
        if ! IFS= read -r WIZARD_PASSWORD </dev/tty; then
            restore_tty
            TTY_ECHO_DISABLED=false
            fail 'could not read the MQTT password'
        fi
        restore_tty
        TTY_ECHO_DISABLED=false
        tty_prompt '\nConfirm MQTT password: '
        TTY_STATE=$(stty -g </dev/tty) || fail 'could not read terminal settings'
        TTY_ECHO_DISABLED=true
        stty -echo </dev/tty
        if ! IFS= read -r WIZARD_PASSWORD_CONFIRM </dev/tty; then
            restore_tty
            TTY_ECHO_DISABLED=false
            fail 'could not confirm the MQTT password'
        fi
        restore_tty
        TTY_ECHO_DISABLED=false
        tty_prompt '\n'
        if [ -z "$WIZARD_PASSWORD" ]; then
            tty_prompt 'The MQTT password cannot be empty.\n'
        elif [ "$WIZARD_PASSWORD" != "$WIZARD_PASSWORD_CONFIRM" ]; then
            tty_prompt 'The passwords do not match; try again.\n'
        else
            break
        fi
    done

    WIZARD_BROKER_YAML=$(printf '%s' "$WIZARD_BROKER" | yaml_quote)
    WIZARD_USERNAME_YAML=$(printf '%s' "$WIZARD_USERNAME" | yaml_quote)
    WIZARD_CONFIG=$TMP_DIR/config.configured.yaml
    WIZARD_PASSWORD_FILE=$TMP_DIR/mqtt-password.configured
    WIZARD_SECTION=
    WIZARD_BROKER_REPLACED=0
    WIZARD_USERNAME_REPLACED=0
    WIZARD_INSECURE_REPLACED=0
    WIZARD_TLS_REPLACED=0
    WIZARD_SKIP_TLS=false
    WIZARD_CLIENT_ID_REPLACED=0
    WIZARD_BASE_TOPIC_REPLACED=0
    WIZARD_DEVICE_ID_REPLACED=0
    WIZARD_DEVICE_NAME_REPLACED=0
    while IFS= read -r WIZARD_LINE || [ -n "$WIZARD_LINE" ]; do
        if [ "$WIZARD_SKIP_TLS" = true ]; then
            case "$WIZARD_LINE" in
                ''|'    '*) continue ;;
                *) WIZARD_SKIP_TLS=false ;;
            esac
        fi
        case "$WIZARD_LINE" in
            mqtt:) WIZARD_SECTION=mqtt ;;
            device:) WIZARD_SECTION=device ;;
            [![:space:]]*:) WIZARD_SECTION=other ;;
        esac
        case "$WIZARD_SECTION:$WIZARD_LINE" in
            mqtt:'  broker:'*)
                WIZARD_BROKER_REPLACED=$((WIZARD_BROKER_REPLACED + 1))
                printf "  broker: '%s'\n" "$WIZARD_BROKER_YAML"
                ;;
            mqtt:'  username:'*)
                WIZARD_USERNAME_REPLACED=$((WIZARD_USERNAME_REPLACED + 1))
                printf "  username: '%s'\n" "$WIZARD_USERNAME_YAML"
                ;;
            mqtt:'  allow_insecure_transport:'*|mqtt:'  # allow_insecure_transport:'*)
                if [ "$WIZARD_ALLOW_INSECURE" = true ]; then
                    WIZARD_INSECURE_REPLACED=$((WIZARD_INSECURE_REPLACED + 1))
                    printf '  allow_insecure_transport: true\n'
                else
                    printf '%s\n' "$WIZARD_LINE"
                fi
                ;;
            mqtt:'  tls:'*)
                if [ "$WIZARD_ALLOW_INSECURE" = true ]; then
                    WIZARD_TLS_REPLACED=$((WIZARD_TLS_REPLACED + 1))
                    WIZARD_SKIP_TLS=true
                    printf '  tls: {}\n'
                else
                    printf '%s\n' "$WIZARD_LINE"
                fi
                ;;
            mqtt:'  client_id:'*)
                if [ -n "$WIZARD_INSTANCE" ]; then
                    WIZARD_CLIENT_ID_REPLACED=$((WIZARD_CLIENT_ID_REPLACED + 1))
                    printf "  client_id: 'mqtt-raspberry-controller-%s'\n" "$WIZARD_INSTANCE"
                else
                    printf '%s\n' "$WIZARD_LINE"
                fi
                ;;
            mqtt:'  base_topic:'*)
                if [ -n "$WIZARD_INSTANCE" ]; then
                    WIZARD_BASE_TOPIC_REPLACED=$((WIZARD_BASE_TOPIC_REPLACED + 1))
                    printf "  base_topic: 'mqtt-raspberry-controller/%s'\n" "$WIZARD_INSTANCE"
                else
                    printf '%s\n' "$WIZARD_LINE"
                fi
                ;;
            device:'  id:'*)
                if [ -n "$WIZARD_INSTANCE" ]; then
                    WIZARD_DEVICE_ID_REPLACED=$((WIZARD_DEVICE_ID_REPLACED + 1))
                    printf "  id: 'mqtt-raspberry-controller-%s'\n" "$WIZARD_INSTANCE"
                else
                    printf '%s\n' "$WIZARD_LINE"
                fi
                ;;
            device:'  name:'*)
                if [ -n "$WIZARD_INSTANCE" ]; then
                    WIZARD_DEVICE_NAME_REPLACED=$((WIZARD_DEVICE_NAME_REPLACED + 1))
                    printf "  name: 'MQTT Raspberry Controller %s'\n" "$WIZARD_INSTANCE"
                else
                    printf '%s\n' "$WIZARD_LINE"
                fi
                ;;
            *) printf '%s\n' "$WIZARD_LINE" ;;
        esac
    done <"$CONFIG_DIR/config.yaml" >"$WIZARD_CONFIG"

    [ "$WIZARD_BROKER_REPLACED" -eq 1 ] || \
        fail 'MQTT wizard requires exactly one canonical mqtt.broker field'
    [ "$WIZARD_USERNAME_REPLACED" -eq 1 ] || \
        fail 'MQTT wizard requires exactly one canonical mqtt.username field'
    if [ "$WIZARD_ALLOW_INSECURE" = true ]; then
        [ "$WIZARD_INSECURE_REPLACED" -eq 1 ] || \
            fail 'MQTT wizard requires one canonical mqtt.allow_insecure_transport field for plaintext brokers'
        [ "$WIZARD_TLS_REPLACED" -le 1 ] || \
            fail 'MQTT wizard found multiple canonical mqtt.tls fields'
        if [ "$CONFIG_CREATED" = true ]; then
            [ "$WIZARD_TLS_REPLACED" -eq 1 ] || \
                fail 'MQTT wizard requires the canonical mqtt.tls field in the fresh configuration'
        fi
    fi
    if [ -n "$WIZARD_INSTANCE" ]; then
        [ "$WIZARD_CLIENT_ID_REPLACED" -eq 1 ] && \
        [ "$WIZARD_BASE_TOPIC_REPLACED" -eq 1 ] && \
        [ "$WIZARD_DEVICE_ID_REPLACED" -eq 1 ] && \
        [ "$WIZARD_DEVICE_NAME_REPLACED" -eq 1 ] || \
            fail 'MQTT wizard requires the canonical fresh-install identity fields'
    fi

    (umask 077; printf '%s\n' "$WIZARD_PASSWORD" >"$WIZARD_PASSWORD_FILE")
    WIZARD_PASSWORD=
    WIZARD_PASSWORD_CONFIRM=

    if [ -n "$DESTDIR" ]; then
        MQTT_PASSWORD_FILE=$WIZARD_PASSWORD_FILE \
            "$BIN_DIR/$PROJECT" --config "$WIZARD_CONFIG" --check >/dev/null || \
            fail 'the generated MQTT configuration did not pass validation'
        install -m 0640 "$WIZARD_CONFIG" "$CONFIG_DIR/.config.yaml.install.$$"
        install -m 0600 "$WIZARD_PASSWORD_FILE" "$CONFIG_DIR/.mqtt-password.install.$$"
    else
        require_command systemd-run
        install -o root -g mqtt-raspberry-controller -m 0640 \
            "$WIZARD_CONFIG" "$CONFIG_DIR/.config.yaml.install.$$"
        systemd-run --quiet --wait --pipe --collect \
            --uid=mqtt-raspberry-controller \
            --property=NoNewPrivileges=yes \
            --property=PrivateTmp=yes \
            --property=ProtectSystem=strict \
            --property=ProtectHome=yes \
            --property="LoadCredential=mqtt-password:$WIZARD_PASSWORD_FILE" \
            /bin/sh -c '
                MQTT_PASSWORD_FILE=$CREDENTIALS_DIRECTORY/mqtt-password
                export MQTT_PASSWORD_FILE
                exec "$1" --config "$2" --check
            ' sh "$BIN_DIR/$PROJECT" "$CONFIG_DIR/.config.yaml.install.$$" >/dev/null || \
                fail 'the generated MQTT configuration did not pass validation'
        install -o root -g root -m 0600 \
            "$WIZARD_PASSWORD_FILE" "$CONFIG_DIR/.mqtt-password.install.$$"
    fi
    mv -f "$CONFIG_DIR/.config.yaml.install.$$" "$CONFIG_DIR/config.yaml"
    mv -f "$CONFIG_DIR/.mqtt-password.install.$$" "$CONFIG_DIR/mqtt-password"
    CONFIGURED=true
    info 'MQTT configuration validated and saved.'
}

maybe_configure_mqtt() {
    case "$CONFIGURE_MODE" in
        0) return 0 ;;
        1) configure_mqtt ;;
        auto)
            if [ -z "$DESTDIR" ] && [ "$CONFIG_CREATED" = true ] && \
                [ "$PASSWORD_CREATED" = true ] && tty_available
            then
                configure_mqtt
            fi
            ;;
    esac
}

restore_target() {
    target=$1
    name=$2
    if [ -e "$BACKUP_DIR/$name" ] || [ -L "$BACKUP_DIR/$name" ]; then
        rm -rf "$target"
        cp -a "$BACKUP_DIR/$name" "$target"
    else
        rm -f "$target"
    fi
}

rollback() {
    warn 'installation failed; restoring previously installed files'
    restore_target "$BIN_DIR/$PROJECT" binary
    restore_target "$SYSUSERS_DIR/mqtt-raspberry-controller.conf" sysusers
    restore_target "$SYSTEMD_DIR/mqtt-raspberry-controller.service" service
    restore_target "$UDEV_DIR/60-mqtt-raspberry-controller.rules" udev
    restore_target "$CONFIG_DIR/config.yaml" config
    restore_target "$CONFIG_DIR/mqtt-password" password
    rm -f \
        "$BIN_DIR/.$PROJECT.install.$$" \
        "$CONFIG_DIR/.config.yaml.install.$$" \
        "$CONFIG_DIR/.mqtt-password.install.$$" \
        "$SYSUSERS_DIR/.mqtt-raspberry-controller.conf.install.$$" \
        "$SYSTEMD_DIR/.mqtt-raspberry-controller.service.install.$$" \
        "$UDEV_DIR/.60-mqtt-raspberry-controller.rules.install.$$"
    systemctl daemon-reload >/dev/null 2>&1 || true
    if command -v udevadm >/dev/null 2>&1; then
        udevadm control --reload-rules >/dev/null 2>&1 || true
    fi
}

finish() {
    status=$?
    trap - 0 HUP INT QUIT TERM TSTP
    if [ "$TTY_ECHO_DISABLED" = true ]; then
        if tty_available; then
            restore_tty
            tty_prompt '\n'
        fi
        TTY_ECHO_DISABLED=false
    fi
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
trap 'exit 131' QUIT
trap 'exit 143' TERM
trap suspend_for_tty TSTP

for command in curl tar sha256sum install mktemp uname awk grep id tr wc strings; do
    require_command "$command"
done

case "$(uname -s)" in
    Linux) ;;
    *) fail 'only Linux is supported' ;;
esac

case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
esac

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

# The public one-liner starts unprivileged. Before requesting sudo, fetch the
# installer from the selected immutable release tag. The short root bootstrap
# copies it into a root-owned 0700 directory and verifies the exact hash passed
# in argv before executing it. All artifact verification and extraction then
# happen in that root-owned directory, closing the user-writable-file TOCTOU.
if [ -z "$DESTDIR" ] && [ "$(id -u)" -ne 0 ]; then
    require_command sudo
    TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/${PROJECT}-bootstrap.XXXXXX")
    BOOTSTRAP=$TMP_DIR/install.sh
    TAGGED_INSTALLER_URL=$RAW_BASE_URL/refs/tags/$TAG/scripts/install.sh
    info "Loading the versioned installer from $TAG..."
    curl_secure -fsSL --retry 3 --retry-delay 1 -o "$BOOTSTRAP" "$TAGGED_INSTALLER_URL" || \
        fail "the $TAG release does not contain scripts/install.sh"
    BOOTSTRAP_HASH=$(sha256sum "$BOOTSTRAP" | awk '{print $1}')
    sudo sh -c '
        set -eu
        PATH=/usr/sbin:/usr/bin:/sbin:/bin
        export PATH
        source_script=$1
        expected_hash=$2
        version=$3
        require_attestation=$4
        configure_mode=$5
        root_dir=$(mktemp -d /var/tmp/mqtt-raspberry-controller-bootstrap.XXXXXX)
        chmod 0700 "$root_dir"
        cleanup_root() { rm -rf "$root_dir"; }
        trap cleanup_root 0
        trap "exit 129" HUP
        trap "exit 130" INT
        trap "exit 143" TERM
        install -m 0700 "$source_script" "$root_dir/install.sh"
        actual_hash=$(sha256sum "$root_dir/install.sh" | awk "{print \$1}")
        [ "$actual_hash" = "$expected_hash" ] || {
            printf "%s\n" "versioned installer changed before privilege escalation" >&2
            exit 1
        }
        MQTT_RASPBERRY_VERSION=$version \
        MQTT_RASPBERRY_REQUIRE_ATTESTATION=$require_attestation \
        MQTT_RASPBERRY_CONFIGURE=$configure_mode \
            sh "$root_dir/install.sh"
    ' sh "$BOOTSTRAP" "$BOOTSTRAP_HASH" "$VERSION" "$REQUIRE_ATTESTATION" "$CONFIGURE_MODE"
    exit 0
fi

ARCHIVE=${PROJECT}_${VERSION}_linux_${ARCH}.tar.gz
PACKAGE_DIR=${ARCHIVE%.tar.gz}
DOWNLOAD_URL=$RELEASE_BASE_URL/$TAG
if [ -n "$DESTDIR" ]; then
    TMP_DIR=$(mktemp -d "${TMPDIR:-/tmp}/${PROJECT}-install.XXXXXX")
else
    [ "$(id -u)" -eq 0 ] || fail 'privileged installation must run as root'
    TMP_DIR=$(mktemp -d "/var/tmp/${PROJECT}-install.XXXXXX")
    chmod 0700 "$TMP_DIR"
fi

info "Downloading $PROJECT $VERSION for linux/$ARCH..."
curl_secure -fsSL --retry 3 --retry-delay 1 -o "$TMP_DIR/$ARCHIVE" "$DOWNLOAD_URL/$ARCHIVE" || \
    fail "could not download $ARCHIVE"
curl_secure -fsSL --retry 3 --retry-delay 1 -o "$TMP_DIR/SHA256SUMS" "$DOWNLOAD_URL/SHA256SUMS" || \
    fail 'could not download SHA256SUMS'

awk -v archive="$ARCHIVE" '
    $2 == archive || $2 == "./" archive { print $1 "  " archive }
' "$TMP_DIR/SHA256SUMS" >"$TMP_DIR/archive.sha256"
[ "$(wc -l <"$TMP_DIR/archive.sha256" | tr -d '[:space:]')" = 1 ] || \
    fail "SHA256SUMS does not contain exactly one entry for $ARCHIVE"
(
    cd "$TMP_DIR"
    sha256sum --check archive.sha256
) || fail 'release checksum verification failed'

if [ -z "$DESTDIR" ]; then
    if command -v gh >/dev/null 2>&1; then
        info 'Verifying the GitHub build provenance attestation...'
        gh attestation verify "$TMP_DIR/$ARCHIVE" \
            --repo "$REPOSITORY" \
            --signer-workflow "$REPOSITORY/.github/workflows/release.yml" \
            --deny-self-hosted-runners >/dev/null || fail 'GitHub provenance attestation verification failed'
    elif [ "$REQUIRE_ATTESTATION" = 1 ]; then
        fail 'GitHub CLI is required because MQTT_RASPBERRY_REQUIRE_ATTESTATION=1'
    else
        warn 'GitHub CLI is unavailable; SHA-256 checks integrity but does not authenticate the publisher'
        warn 'install gh and set MQTT_RASPBERRY_REQUIRE_ATTESTATION=1 to require provenance verification'
    fi
fi

# Accept only the single expected top-level directory and reject path traversal,
# links, devices, sockets, and all other special archive entries.
tar -tzf "$TMP_DIR/$ARCHIVE" >"$TMP_DIR/archive.list" || fail 'release archive is invalid'
tar -tvzf "$TMP_DIR/$ARCHIVE" >"$TMP_DIR/archive.details" || fail 'release archive is invalid'
[ -s "$TMP_DIR/archive.list" ] || fail 'release archive is empty'
awk -v root="$PACKAGE_DIR/" '
    index($0, root) != 1 { exit 1 }
    $0 ~ /(^|\/)\.\.($|\/)/ { exit 1 }
' "$TMP_DIR/archive.list" || fail 'release archive contains an unsafe path'
awk 'substr($1, 1, 1) != "-" && substr($1, 1, 1) != "d" { exit 1 }' \
    "$TMP_DIR/archive.details" || fail 'release archive contains a link or special file'

tar -xzf "$TMP_DIR/$ARCHIVE" -C "$TMP_DIR"
PACKAGE=$TMP_DIR/$PACKAGE_DIR
for required in \
    "$PROJECT" \
    configs/config.example.yaml \
    packaging/sysusers.d/mqtt-raspberry-controller.conf \
    packaging/systemd/mqtt-raspberry-controller.service \
    packaging/udev/60-mqtt-raspberry-controller.rules
do
    [ -e "$PACKAGE/$required" ] || fail "release archive is missing $required"
done
[ -f "$PACKAGE/$PROJECT" ] && [ -x "$PACKAGE/$PROJECT" ] || fail 'release binary is not executable'
strings "$PACKAGE/$PROJECT" | grep -Fqx "$VERSION" || fail 'release binary does not contain the expected version'

PREFIX=$DESTDIR
BIN_DIR=$PREFIX/usr/local/bin
CONFIG_DIR=$PREFIX/etc/mqtt-raspberry-controller
SYSUSERS_DIR=$PREFIX/usr/lib/sysusers.d
SYSTEMD_DIR=$PREFIX/etc/systemd/system
UDEV_DIR=$PREFIX/etc/udev/rules.d

if [ -n "$DESTDIR" ]; then
    install -d -m 0755 "$BIN_DIR" "$SYSUSERS_DIR" "$SYSTEMD_DIR" "$UDEV_DIR"
    install -d -m 0750 "$CONFIG_DIR"
    install -m 0755 "$PACKAGE/$PROJECT" "$BIN_DIR/$PROJECT"
    install -m 0644 "$PACKAGE/packaging/sysusers.d/mqtt-raspberry-controller.conf" "$SYSUSERS_DIR/mqtt-raspberry-controller.conf"
    install -m 0644 "$PACKAGE/packaging/systemd/mqtt-raspberry-controller.service" "$SYSTEMD_DIR/mqtt-raspberry-controller.service"
    install -m 0644 "$PACKAGE/packaging/udev/60-mqtt-raspberry-controller.rules" "$UDEV_DIR/60-mqtt-raspberry-controller.rules"
    if [ ! -e "$CONFIG_DIR/config.yaml" ]; then
        install -m 0640 "$PACKAGE/configs/config.example.yaml" "$CONFIG_DIR/config.yaml"
        CONFIG_CREATED=true
    fi
    if [ ! -e "$CONFIG_DIR/mqtt-password" ]; then
        install -m 0600 /dev/null "$CONFIG_DIR/mqtt-password"
        PASSWORD_CREATED=true
    fi
    maybe_configure_mqtt
    info "Staged $PROJECT $VERSION under $DESTDIR"
    exit 0
fi

for command in getent systemctl cp mv rm; do
    require_command "$command"
done

BIN_TARGET=$BIN_DIR/$PROJECT
SYSUSERS_TARGET=$SYSUSERS_DIR/mqtt-raspberry-controller.conf
SERVICE_TARGET=$SYSTEMD_DIR/mqtt-raspberry-controller.service
UDEV_TARGET=$UDEV_DIR/60-mqtt-raspberry-controller.rules
BACKUP_DIR=$TMP_DIR/backup
install -d -m 0700 "$BACKUP_DIR"
for pair in \
    "$BIN_TARGET:binary" \
    "$SYSUSERS_TARGET:sysusers" \
    "$SERVICE_TARGET:service" \
    "$UDEV_TARGET:udev" \
    "$CONFIG_DIR/config.yaml:config" \
    "$CONFIG_DIR/mqtt-password:password"
do
    target=${pair%:*}
    name=${pair##*:}
    if [ -e "$target" ] || [ -L "$target" ]; then
        cp -a "$target" "$BACKUP_DIR/$name"
    fi
done
ROLLBACK_ACTIVE=true

if ! getent group gpio >/dev/null 2>&1; then
    require_command groupadd
    groupadd --system gpio
fi

install -d -m 0755 "$BIN_DIR" "$SYSUSERS_DIR" "$SYSTEMD_DIR" "$UDEV_DIR"
install -m 0644 "$PACKAGE/packaging/sysusers.d/mqtt-raspberry-controller.conf" \
    "$SYSUSERS_DIR/.mqtt-raspberry-controller.conf.install.$$"
mv -f "$SYSUSERS_DIR/.mqtt-raspberry-controller.conf.install.$$" "$SYSUSERS_TARGET"
if command -v systemd-sysusers >/dev/null 2>&1; then
    systemd-sysusers mqtt-raspberry-controller.conf
else
    require_command groupadd
    require_command useradd
    if ! getent group mqtt-raspberry-controller >/dev/null 2>&1; then
        groupadd --system mqtt-raspberry-controller
    fi
    if ! id mqtt-raspberry-controller >/dev/null 2>&1; then
        useradd --system --gid mqtt-raspberry-controller --home-dir /nonexistent \
            --shell /usr/sbin/nologin --comment 'MQTT Raspberry Controller' mqtt-raspberry-controller
    fi
fi

install -d -o root -g mqtt-raspberry-controller -m 0750 "$CONFIG_DIR"
install -o root -g root -m 0755 "$PACKAGE/$PROJECT" "$BIN_DIR/.$PROJECT.install.$$"
mv -f "$BIN_DIR/.$PROJECT.install.$$" "$BIN_TARGET"
install -m 0644 "$PACKAGE/packaging/systemd/mqtt-raspberry-controller.service" \
    "$SYSTEMD_DIR/.mqtt-raspberry-controller.service.install.$$"
mv -f "$SYSTEMD_DIR/.mqtt-raspberry-controller.service.install.$$" "$SERVICE_TARGET"
install -m 0644 "$PACKAGE/packaging/udev/60-mqtt-raspberry-controller.rules" \
    "$UDEV_DIR/.60-mqtt-raspberry-controller.rules.install.$$"
mv -f "$UDEV_DIR/.60-mqtt-raspberry-controller.rules.install.$$" "$UDEV_TARGET"

if [ ! -e "$CONFIG_DIR/config.yaml" ]; then
    install -o root -g mqtt-raspberry-controller -m 0640 "$PACKAGE/configs/config.example.yaml" "$CONFIG_DIR/config.yaml"
    CONFIG_CREATED=true
fi
if [ ! -e "$CONFIG_DIR/mqtt-password" ]; then
    install -o root -g root -m 0600 /dev/null "$CONFIG_DIR/mqtt-password"
    PASSWORD_CREATED=true
fi

maybe_configure_mqtt

systemctl daemon-reload
if command -v udevadm >/dev/null 2>&1; then
    udevadm control --reload-rules
    if [ -e /dev/gpiochip0 ]; then
        udevadm trigger --subsystem-match=gpio --sysname-match=gpiochip0
    fi
fi
ROLLBACK_ACTIVE=false

info "Installed $PROJECT $VERSION."
if [ "$CONFIG_CREATED" = true ]; then
    info "Created $CONFIG_DIR/config.yaml from the example configuration."
else
    info "Preserved the existing $CONFIG_DIR/config.yaml."
fi
if [ "$CONFIGURED" = true ]; then
    info 'The service was not started. The MQTT configuration passed validation.'
else
    info "The service was not started. Configure these restricted files first:"
    info "  $CONFIG_DIR/config.yaml"
    info "  $CONFIG_DIR/mqtt-password"
fi
info 'Then enable the service when the GPIO configuration is ready:'
info "  sudo systemctl enable --now $PROJECT"
info "  sudo systemctl status $PROJECT --no-pager"
