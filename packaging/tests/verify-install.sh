#!/bin/sh
set -eu

REPO_ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
UNIT=mqtt-raspberry-controller.service
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT HUP INT TERM

need() {
    command -v "$1" >/dev/null 2>&1 || {
        printf 'missing required command: %s\n' "$1" >&2
        exit 1
    }
}

need systemd-analyze
need systemd-sysusers
need stat
need id
need awk

install -d "$STAGE/etc/systemd/system" \
    "$STAGE/etc/mqtt-raspberry-controller" \
    "$STAGE/etc/sysusers.d" \
    "$STAGE/etc/udev/rules.d" \
    "$STAGE/usr/local/bin"
install -m 0644 "$REPO_ROOT/packaging/sysusers.d/mqtt-raspberry-controller.conf" "$STAGE/etc/sysusers.d/mqtt-raspberry-controller.conf"
printf 'g gpio -\n' >"$STAGE/etc/sysusers.d/gpio.conf"
for target in sysinit.target basic.target shutdown.target network-online.target multi-user.target; do
    printf '[Unit]\nDescription=staged %s\n' "$target" >"$STAGE/etc/systemd/system/$target"
done
systemd-sysusers --root="$STAGE"

install -m 0755 /bin/true "$STAGE/usr/local/bin/mqtt-raspberry-controller"
if [ "$(id -u)" -eq 0 ]; then
    mqtt_gid=$(awk -F: '$1 == "mqtt-raspberry-controller" { print $3 }' "$STAGE/etc/group")
    [ -n "$mqtt_gid" ]
    install -o 0 -g "$mqtt_gid" -m 0640 "$REPO_ROOT/configs/config.example.yaml" "$STAGE/etc/mqtt-raspberry-controller/config.yaml"
    install -o 0 -g 0 -m 0600 /dev/null "$STAGE/etc/mqtt-raspberry-controller/mqtt-password"
else
    install -m 0640 "$REPO_ROOT/configs/config.example.yaml" "$STAGE/etc/mqtt-raspberry-controller/config.yaml"
    install -m 0600 /dev/null "$STAGE/etc/mqtt-raspberry-controller/mqtt-password"
fi
install -m 0644 "$REPO_ROOT/packaging/systemd/$UNIT" "$STAGE/etc/systemd/system/$UNIT"
install -m 0644 "$REPO_ROOT/packaging/udev/60-mqtt-raspberry-controller.rules" "$STAGE/etc/udev/rules.d/60-mqtt-raspberry-controller.rules"

systemd-analyze verify --root="$STAGE" "$UNIT"

config_mode=$(stat -c '%a' "$STAGE/etc/mqtt-raspberry-controller/config.yaml")
[ $((0$config_mode & ~0640)) -eq 0 ]
[ "$(stat -c '%a' "$STAGE/etc/mqtt-raspberry-controller/mqtt-password")" = 600 ]
if [ "$(id -u)" -eq 0 ]; then
    [ "$(stat -c '%u:%g' "$STAGE/etc/mqtt-raspberry-controller/config.yaml")" = "0:$mqtt_gid" ]
    [ "$(stat -c '%u:%g' "$STAGE/etc/mqtt-raspberry-controller/mqtt-password")" = 0:0 ]
    ownership_result='staged ownership verified as root'
else
    grep -Fq 'install -o root -g mqtt-raspberry-controller -m 0640 configs/config.example.yaml' "$REPO_ROOT/README.md"
    grep -Fq 'install -o root -g root -m 0600 /dev/null /etc/mqtt-raspberry-controller/mqtt-password' "$REPO_ROOT/README.md"
    ownership_result='ownership commands verified textually (staged chown requires root)'
fi

grep -Fqx 'u mqtt-raspberry-controller - "MQTT Raspberry Controller" /nonexistent /usr/sbin/nologin' \
    "$STAGE/etc/sysusers.d/mqtt-raspberry-controller.conf"
grep -Fqx 'SUBSYSTEM=="gpio", KERNEL=="gpiochip0", OWNER="root", GROUP="gpio", MODE="0660"' \
    "$STAGE/etc/udev/rules.d/60-mqtt-raspberry-controller.rules"
grep -Fqx 'User=mqtt-raspberry-controller' "$STAGE/etc/systemd/system/$UNIT"
grep -Fqx 'Group=mqtt-raspberry-controller' "$STAGE/etc/systemd/system/$UNIT"
grep -Fqx 'SupplementaryGroups=gpio' "$STAGE/etc/systemd/system/$UNIT"
grep -Fqx 'LoadCredential=mqtt-password:/etc/mqtt-raspberry-controller/mqtt-password' "$STAGE/etc/systemd/system/$UNIT"
grep -Fqx 'Environment=MQTT_PASSWORD_FILE=%d/mqtt-password' "$STAGE/etc/systemd/system/$UNIT"
grep -Fqx 'user mqtt-raspberry-controller' "$REPO_ROOT/packaging/mosquitto/mqtt-raspberry-controller.acl"
if grep -Eq '^topic .*([+#])' "$REPO_ROOT/packaging/mosquitto/mqtt-raspberry-controller.acl"; then
    printf 'Mosquitto ACL must not contain wildcard topics\n' >&2
    exit 1
fi
for topic in \
    'topic read mqtt-raspberry-controller/example/relay/set' \
    'topic read mqtt-raspberry-controller/example/announce/press' \
    'topic write mqtt-raspberry-controller/example/status' \
    'topic write mqtt-raspberry-controller/example/relay/state' \
    'topic write mqtt-raspberry-controller/example/contact/state' \
    'topic write mqtt-raspberry-controller/example/events' \
    'topic write homeassistant/switch/mqtt-raspberry-controller-example/relay/config' \
    'topic write homeassistant/button/mqtt-raspberry-controller-example/announce/config' \
    'topic write homeassistant/binary_sensor/mqtt-raspberry-controller-example/contact/config'; do
    grep -Fqx "$topic" "$REPO_ROOT/packaging/mosquitto/mqtt-raspberry-controller.acl"
done
grep -Fqx 'DevicePolicy=closed' "$STAGE/etc/systemd/system/$UNIT"
grep -Fqx 'DeviceAllow=/dev/gpiochip0 rw' "$STAGE/etc/systemd/system/$UNIT"
for directive in \
    'NoNewPrivileges=yes' \
    'CapabilityBoundingSet=' \
    'AmbientCapabilities=' \
    'ProtectSystem=strict' \
    'ProtectHome=yes' \
    'PrivateTmp=yes' \
    'PrivateMounts=yes' \
	'StateDirectory=mqtt-raspberry-controller' \
	'StateDirectoryMode=0700' \
    'ProtectControlGroups=yes' \
    'ProtectKernelModules=yes' \
    'ProtectKernelTunables=yes' \
    'ProtectKernelLogs=yes' \
    'ProtectClock=yes' \
    'RestrictNamespaces=yes' \
    'RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6' \
    'UMask=0077' \
    'MemoryMax=128M' \
    'TasksMax=64' \
    'KillMode=control-group' \
    'TimeoutStopSec=15s'; do
    grep -Fqx "$directive" "$STAGE/etc/systemd/system/$UNIT"
done
if grep -Eq '^[[:space:]]*PrivateDevices=(yes|true|1)[[:space:]]*$' "$STAGE/etc/systemd/system/$UNIT"; then
    printf 'PrivateDevices=yes would hide /dev/gpiochip0\n' >&2
    exit 1
fi

printf 'packaging verification passed in staged root: %s (%s)\n' "$STAGE" "$ownership_result"
