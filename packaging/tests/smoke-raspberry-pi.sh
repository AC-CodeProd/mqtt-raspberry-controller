#!/bin/bash
set -Eeuo pipefail

SERVICE=mqtt-raspberry-controller
UNIT=/etc/systemd/system/mqtt-raspberry-controller.service
CONFIG=/etc/mqtt-raspberry-controller/config.yaml
CHIP_DEVICE=/dev/gpiochip0
MQTT_OPERATION_TIMEOUT=10
MQTT_RESTORE_TIMEOUT=30
COMMAND_KILL_GRACE=2
SYSTEMCTL_OPERATION_TIMEOUT=15

fail() {
    printf 'smoke test failed: %s\n' "$*" >&2
    exit 1
}

need() {
    command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

require_env() {
    local name=$1
    [[ -n ${!name:-} ]] || fail "set $name"
}

contains_line_break() {
    [[ $1 == *$'\n'* || $1 == *$'\r'* ]]
}

systemctl_bounded() {
    timeout --foreground --kill-after="$COMMAND_KILL_GRACE" \
        "$SYSTEMCTL_OPERATION_TIMEOUT" systemctl "$@"
}

wait_for_service() {
    local attempt
    for attempt in {1..50}; do
        systemctl_bounded is-active --quiet "$SERVICE" && return 0
        sleep 0.1
    done
    return 1
}

wait_for_service_ready() {
    local invocation_id attempt
    wait_for_service || return 1
    invocation_id=$(systemctl_bounded show --property=InvocationID --value "$SERVICE") || return 1
    [[ $invocation_id =~ ^[[:xdigit:]]{32}$ ]] || return 1
    for attempt in {1..50}; do
        if timeout --foreground --kill-after="$COMMAND_KILL_GRACE" 2 \
            journalctl -u "$SERVICE" "_SYSTEMD_INVOCATION_ID=$invocation_id" \
            --no-pager -o cat --grep='mqtt-raspberry-controller .* started' -n 1 \
            >/dev/null 2>&1; then
            return 0
        fi
        sleep 0.1
    done
    return 1
}

mqtt_pub() {
    timeout --foreground --kill-after="$COMMAND_KILL_GRACE" \
        "$MQTT_OPERATION_TIMEOUT" \
        env XDG_CONFIG_HOME="$mqtt_config_dir" \
        mosquitto_pub "${mqtt_args[@]}" "$@"
}

mqtt_sub() {
    local operation_timeout=$1
    shift
    timeout --foreground --kill-after="$COMMAND_KILL_GRACE" \
        "$operation_timeout" \
        env XDG_CONFIG_HOME="$mqtt_config_dir" \
        mosquitto_sub "${mqtt_args[@]}" "$@"
}

start_message_capture() {
    local operation_timeout=$1 topic=$2 destination=$3
    timeout --foreground --kill-after="$COMMAND_KILL_GRACE" \
        "$operation_timeout" \
        env XDG_CONFIG_HOME="$mqtt_config_dir" \
        mosquitto_sub "${mqtt_args[@]}" -q 1 -R -C 1 -t "$topic" \
        >"$destination" &
    subscriber_pid=$!
}

stop_subscriber() {
    if [[ -n $subscriber_pid ]]; then
        kill "$subscriber_pid" 2>/dev/null || true
        wait "$subscriber_pid" 2>/dev/null || true
        subscriber_pid=
    fi
}

restore_gpio_state() {
    local acknowledgement

    if ! systemctl_bounded is-active --quiet "$SERVICE"; then
        systemctl_bounded start "$SERVICE" || return 1
    fi
    wait_for_service_ready || return 1

    state_capture=$(mktemp)
    start_message_capture "$MQTT_RESTORE_TIMEOUT" "$state_topic" "$state_capture"
    sleep 0.2
    if ! mqtt_pub -q 1 -t "$set_topic" -m "$restore_state"; then
        stop_subscriber
        return 1
    fi
    if ! wait "$subscriber_pid"; then
        subscriber_pid=
        return 1
    fi
    subscriber_pid=
    acknowledgement=$(<"$state_capture")
    rm -f "$state_capture"
    state_capture=
    [[ $acknowledgement == "$restore_state" ]]
}

was_active=false
restore_state=
state_capture=
status_capture=
subscriber_pid=
mqtt_config_dir=
service_state_captured=false

cleanup() {
    local rc=$? restoration_unconfirmed=false
    trap - EXIT HUP INT TERM
    set +e

    stop_subscriber
    if [[ -n $restore_state ]]; then
        if ! restore_gpio_state; then
            printf 'smoke test cleanup failed: GPIO state restoration was not acknowledged\n' >&2
            rc=1
            restoration_unconfirmed=true
        fi
    fi

    stop_subscriber
    [[ -z $state_capture ]] || rm -f "$state_capture"
    [[ -z $status_capture ]] || rm -f "$status_capture"
    [[ -z $mqtt_config_dir ]] || rm -rf "$mqtt_config_dir"

    if $service_state_captured; then
        if $was_active; then
            systemctl_bounded start "$SERVICE" || rc=1
        elif ! $restoration_unconfirmed; then
            systemctl_bounded stop "$SERVICE" || rc=1
        else
            printf 'smoke test cleanup warning: service was left running so a pending GPIO restoration can still process\n' >&2
        fi
    fi
    exit "$rc"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

[[ ${EUID} -eq 0 ]] || fail 'run as root'
[[ ${MQTT_RASPBERRY_SMOKE_CONFIRM:-} == 'YES' ]] || fail 'set MQTT_RASPBERRY_SMOKE_CONFIRM=YES to acknowledge a real service stop and GPIO toggle'
for name in MQTT_RASPBERRY_SMOKE_HOST MQTT_RASPBERRY_SMOKE_PORT MQTT_RASPBERRY_SMOKE_BASE_TOPIC \
    MQTT_RASPBERRY_SMOKE_SWITCH_ID MQTT_RASPBERRY_SMOKE_OUTPUT_NAME MQTT_RASPBERRY_SMOKE_GPIO_LINE; do
    require_env "$name"
done
[[ ${MQTT_RASPBERRY_SMOKE_GPIO_CHIP:-} == "$CHIP_DEVICE" ]] || \
    fail "set MQTT_RASPBERRY_SMOKE_GPIO_CHIP=$CHIP_DEVICE (the packaged policy permits only this chip)"
[[ ${MQTT_RASPBERRY_SMOKE_PORT} =~ ^[0-9]+$ ]] || fail 'MQTT_RASPBERRY_SMOKE_PORT must be numeric'
(( MQTT_RASPBERRY_SMOKE_PORT >= 1 && MQTT_RASPBERRY_SMOKE_PORT <= 65535 )) || fail 'MQTT_RASPBERRY_SMOKE_PORT must be between 1 and 65535'
[[ ${MQTT_RASPBERRY_SMOKE_GPIO_LINE} =~ ^[0-9]+$ ]] || fail 'MQTT_RASPBERRY_SMOKE_GPIO_LINE must be numeric'
for value in "$MQTT_RASPBERRY_SMOKE_HOST" "$MQTT_RASPBERRY_SMOKE_BASE_TOPIC" \
    "$MQTT_RASPBERRY_SMOKE_SWITCH_ID" "$MQTT_RASPBERRY_SMOKE_OUTPUT_NAME" \
    "${MQTT_RASPBERRY_SMOKE_USERNAME:-}" "${MQTT_RASPBERRY_SMOKE_PASSWORD:-}" \
    "${MQTT_RASPBERRY_SMOKE_CAFILE:-}"; do
    contains_line_break "$value" && fail 'smoke-test values must not contain line breaks'
done
if [[ -n ${MQTT_RASPBERRY_SMOKE_USERNAME:-} && -z ${MQTT_RASPBERRY_SMOKE_PASSWORD+x} ]]; then
    fail 'set MQTT_RASPBERRY_SMOKE_PASSWORD when MQTT_RASPBERRY_SMOKE_USERNAME is set'
fi
if [[ -n ${MQTT_RASPBERRY_SMOKE_PASSWORD:-} && -z ${MQTT_RASPBERRY_SMOKE_USERNAME:-} ]]; then
    fail 'set MQTT_RASPBERRY_SMOKE_USERNAME when MQTT_RASPBERRY_SMOKE_PASSWORD is set'
fi

for command in systemctl systemd-analyze journalctl stat getent id awk grep install \
    mosquitto_pub mosquitto_sub timeout; do
    need "$command"
done

[[ -f "$UNIT" ]] || fail "$UNIT is not installed"
[[ -f "$CONFIG" ]] || fail "$CONFIG is not installed"
[[ -c "$CHIP_DEVICE" ]] || fail "$CHIP_DEVICE is not a character device"
getent passwd mqtt-raspberry-controller >/dev/null || fail 'mqtt-raspberry-controller user does not exist'
getent group mqtt-raspberry-controller >/dev/null || fail 'mqtt-raspberry-controller group does not exist'
getent group gpio >/dev/null || fail 'gpio group does not exist'

config_mode=$(stat -c '%a' "$CONFIG")
(( (8#$config_mode & ~8#640) == 0 )) || fail "$CONFIG mode $config_mode is broader than 0640"
[[ $(stat -c '%U:%G' "$CONFIG") == root:mqtt-raspberry-controller ]] || \
    fail "$CONFIG must be owned by root:mqtt-raspberry-controller"
[[ $(stat -c '%a' "$CHIP_DEVICE") == 660 ]] || fail "$CHIP_DEVICE must have mode 0660"
[[ $(stat -c '%U:%G' "$CHIP_DEVICE") == root:gpio ]] || fail "$CHIP_DEVICE must be owned by root:gpio"

systemd-analyze verify "$UNIT"
grep -Fqx 'User=mqtt-raspberry-controller' "$UNIT" || fail 'unit has the wrong User='
grep -Fqx 'Group=mqtt-raspberry-controller' "$UNIT" || fail 'unit has the wrong Group='
grep -Fqx 'SupplementaryGroups=gpio' "$UNIT" || fail 'unit lacks SupplementaryGroups=gpio'
grep -Fqx 'DevicePolicy=closed' "$UNIT" || fail 'unit lacks DevicePolicy=closed'
grep -Fqx 'DeviceAllow=/dev/gpiochip0 rw' "$UNIT" || fail 'unit lacks the exact GPIO device allowance'

mqtt_config_dir=$(mktemp -d)
chmod 0700 "$mqtt_config_dir"
for client in mosquitto_pub mosquitto_sub; do
    if [[ -n ${MQTT_RASPBERRY_SMOKE_USERNAME:-} ]]; then
        printf '%s\n%s\n' \
            "-u $MQTT_RASPBERRY_SMOKE_USERNAME" \
            "-P $MQTT_RASPBERRY_SMOKE_PASSWORD" >"$mqtt_config_dir/$client"
    else
        : >"$mqtt_config_dir/$client"
    fi
    chmod 0600 "$mqtt_config_dir/$client"
done
unset MQTT_RASPBERRY_SMOKE_PASSWORD

mqtt_args=(-h "$MQTT_RASPBERRY_SMOKE_HOST" -p "$MQTT_RASPBERRY_SMOKE_PORT")
if [[ -n ${MQTT_RASPBERRY_SMOKE_CAFILE:-} ]]; then
    [[ -r $MQTT_RASPBERRY_SMOKE_CAFILE ]] || fail 'MQTT_RASPBERRY_SMOKE_CAFILE is not readable'
    mqtt_args+=(--cafile "$MQTT_RASPBERRY_SMOKE_CAFILE")
fi

status_topic=${MQTT_RASPBERRY_SMOKE_BASE_TOPIC}/status
set_topic=${MQTT_RASPBERRY_SMOKE_BASE_TOPIC}/${MQTT_RASPBERRY_SMOKE_SWITCH_ID}/set
state_topic=${MQTT_RASPBERRY_SMOKE_BASE_TOPIC}/${MQTT_RASPBERRY_SMOKE_SWITCH_ID}/state
if systemctl_bounded is-active --quiet "$SERVICE"; then
    was_active=true
fi
service_state_captured=true

systemctl_bounded start "$SERVICE"
wait_for_service || fail 'service did not become active'

main_pid=$(systemctl_bounded show --property=MainPID --value "$SERVICE")
[[ $main_pid =~ ^[1-9][0-9]*$ ]] || fail 'service has no MainPID'
expected_uid=$(id -u mqtt-raspberry-controller)
actual_uid=$(awk '/^Uid:/ { print $2 }' "/proc/$main_pid/status")
[[ $actual_uid == "$expected_uid" ]] || fail "service UID is $actual_uid, expected $expected_uid"
invocation_id=$(systemctl_bounded show --property=InvocationID --value "$SERVICE")
[[ $invocation_id =~ ^[[:xdigit:]]{32}$ ]] || fail 'service has no current InvocationID'
output_log="GPIO output \"$MQTT_RASPBERRY_SMOKE_OUTPUT_NAME\" opened on gpiochip0:$MQTT_RASPBERRY_SMOKE_GPIO_LINE "
entity_log="switch \"$MQTT_RASPBERRY_SMOKE_SWITCH_ID\" state uses GPIO output \"$MQTT_RASPBERRY_SMOKE_OUTPUT_NAME\""
mapping_confirmed=false
for _ in {1..50}; do
    current_journal=$(journalctl -u "$SERVICE" "_SYSTEMD_INVOCATION_ID=$invocation_id" --no-pager -o cat)
    if grep -Fq "$output_log" <<<"$current_journal" && grep -Fq "$entity_log" <<<"$current_journal"; then
        mapping_confirmed=true
        break
    fi
    sleep 0.1
done
$mapping_confirmed || fail 'current service invocation does not confirm the switch, output name, and GPIO line mapping'

online=$(mqtt_sub "$MQTT_OPERATION_TIMEOUT" -q 1 -C 1 -t "$status_topic")
[[ $online == online ]] || fail "expected online status, received '$online'"
initial=$(mqtt_sub "$MQTT_OPERATION_TIMEOUT" -q 1 -C 1 -t "$state_topic")
[[ $initial == ON || $initial == OFF ]] || fail "expected ON/OFF state, received '$initial'"
restore_state=$initial
if [[ $initial == ON ]]; then target=OFF; else target=ON; fi

state_capture=$(mktemp)
start_message_capture "$MQTT_OPERATION_TIMEOUT" "$state_topic" "$state_capture"
sleep 0.2
mqtt_pub -q 1 -t "$set_topic" -m "$target" || fail 'timed out or failed publishing the GPIO toggle'
wait "$subscriber_pid" || fail 'timed out waiting for toggled GPIO state'
subscriber_pid=
[[ $(<"$state_capture") == "$target" ]] || fail "GPIO state did not change to $target"
rm -f "$state_capture"
state_capture=

restore_gpio_state || fail "GPIO state did not restore to $initial before restart"

status_capture=$(mktemp)
start_message_capture 20 "$status_topic" "$status_capture"
sleep 0.2
systemctl_bounded stop "$SERVICE"
wait "$subscriber_pid" || fail 'timed out waiting for offline status'
subscriber_pid=
[[ $(<"$status_capture") == offline ]] || fail 'service did not publish offline on stop'
rm -f "$status_capture"
status_capture=

systemctl_bounded start "$SERVICE"
wait_for_service || fail 'service did not become active after restart'
restarted=$(mqtt_sub "$MQTT_OPERATION_TIMEOUT" -q 1 -C 1 -t "$status_topic")
[[ $restarted == online ]] || fail 'service did not publish online after restart'
restore_gpio_state || fail "GPIO state did not restore to $initial after restart"
restore_state=

rm -rf "$mqtt_config_dir"
mqtt_config_dir=
if ! $was_active; then
    systemctl_bounded stop "$SERVICE"
fi
service_state_captured=false
trap - EXIT HUP INT TERM
printf 'smoke test passed: unit, ownership/modes, UID %s, current-invocation mapping, MQTT online/state/offline, gpiochip0 line %s toggle\n' \
    "$actual_uid" "$MQTT_RASPBERRY_SMOKE_GPIO_LINE"
