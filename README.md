# mqtt-raspberry-controller

**English** | [Français](README_FR.md)

Generic MQTT-controlled GPIO and command daemon written in Go.

Licensed under the [MIT License](LICENSE). Operational documentation: [configuration](docs/CONFIGURATION.md), [installation and upgrade](docs/INSTALL.md), [security](docs/SECURITY.md), [troubleshooting](docs/TROUBLESHOOTING.md), and [release process](docs/RELEASE.md).

## Supported entities

- `switch`
  - state from a GPIO output or in-memory state
  - configurable ON/OFF action sequences
- `button`
  - configurable PRESS action sequence
- `binary_sensor`
  - GPIO input with edge detection

## Supported actions

- `gpio`: set a named GPIO output
- `command`: execute an absolute executable path directly without a shell or `PATH` lookup
- `delay`: wait for a duration
- `mqtt`: publish another MQTT message

Actions are executed sequentially. Set `ignore_error: true` on an action when a failure should not abort the remaining sequence.

### External command isolation

A `command` action is validated before startup. `command` must be an absolute path to an existing regular file with at least one executable mode bit; it is never resolved through `PATH`. An optional `working_dir` must be an existing absolute directory. Environment names must match `[A-Za-z_][A-Za-z0-9_]*`, and command fields, arguments, working directories, and environment values may not contain NUL bytes. Arguments are passed directly to the executable; shell syntax, expansion, pipelines, and redirection are not interpreted.

Commands receive a deterministic base environment containing only `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`, `LANG=C`, and `LC_ALL=C`, merged with that action's explicit `env` map. The daemon never copies its own environment into a command, so MQTT and systemd credential variables are absent unless the action explicitly configures them. An action may explicitly replace any of the three base values.

Standard output and standard error share one concurrency-safe 64 KiB capture. Additional output is discarded while saturating byte totals and truncation status continue to be tracked. Output content, arguments, and environment values are never returned in errors or written to logs; only byte-count/truncation metadata is reported.

On Linux each command starts in a new process group. On timeout or shutdown cancellation, the supervisor sends `SIGTERM` to that group while the leader PID is still owned, allows a bounded grace period, then sends `SIGKILL`; a bounded pipe wait also prevents descendants that retain stdout or stderr from hanging execution. Normal completion does not signal the process group after reaping the leader, avoiding any risk of signaling an unrelated reused PID. Commands must therefore not launch background services: a surviving child is unsupported and is cleaned up when systemd stops the unit through `KillMode=control-group`. A program that deliberately self-daemonizes with `setsid(2)` escapes the per-action process group but remains subject to the service cgroup at unit shutdown; such programs are unsupported because their individual action timeout cannot be guaranteed.

## MQTT command safety and shutdown

- Control messages on `set` and `press` topics must not be retained. Retained controls are rejected so reconnecting cannot replay an action.
- Within one MQTT connection, QoS 1/2 retransmissions already accepted by this process are deduplicated by topic and packet ID using a bounded 256-entry cache. The cache is reset on reconnect because packet IDs are session-scoped; a DUP packet not previously observed by this process is accepted to avoid losing a command. State and Discovery publications remain retained.
- Command payloads are limited to 4096 bytes. Each entity has a 32-command FIFO; a command arriving while that queue is full is rejected and logged with rate limiting. Separate entities have independent workers. FIFO starts from the order in which Paho delivers callbacks; MQTT ordering across mixed QoS levels still depends on broker/in-flight settings.
- On SIGINT/SIGTERM the daemon stops accepting commands and uses one 10-second shutdown budget for controller workers and MQTT callbacks. It cancels active delays/external commands, discards queued commands, republishes the final observable state of a cancelled switch when possible, publishes `offline`, disconnects MQTT, and finally closes GPIO. If the deadline expires, `run` returns without concurrently closing resources and systemd terminates the failed process.

## Home Assistant Discovery lifecycle

Discovery topics are retained and reconciled on every MQTT connection. The daemon stores the exact topics it may have published in `mqtt.discovery.state_file` (default `/var/lib/mqtt-raspberry-controller/discovery-state.json`) and binds that manifest to the stable `mqtt.client_id`. Before publishing the current configuration it sends a retained empty payload at acknowledged QoS (at least QoS 1) to tracked topics that became stale. This covers entity deletion, entity ID rename, component type change, Discovery-prefix change, device-ID change, and `discovery.enabled: false`. The state update is written atomically before broker changes as an over-approximation, so an interrupted or partially failed reconciliation is retained for the next explicit call, restart, or MQTT reconnect rather than forgotten.

The state file is treated as an ownership record, not as an arbitrary cleanup list. It must be a regular `0600` file owned by the service user, inside a service-owned directory that is not group/world-writable and contains no symlink components. A non-blocking lock prevents two local processes from reconciling the same manifest concurrently. Each entry must have the supported Discovery shape, the recorded device ID, and the same `mqtt.client_id`; a malformed, foreign, or shared manifest stops reconciliation without publishing or deleting anything. The packaged systemd unit creates the private writable state directory with mode `0700`. Keep one state file and one unique stable MQTT client ID per controller. When changing `device.id`, preserve `mqtt.client_id` so the previous ownership can be authenticated. A custom path outside `/var/lib/mqtt-raspberry-controller` also requires a matching `StateDirectory=` or narrowly scoped `ReadWritePaths=` systemd override.

### Manual cleanup limits

The controller can only clean up Discovery topics recorded in its current ownership state file. If that file is lost, or if a topic was created outside this controller, remove each known stale topic explicitly with the same broker credentials and an empty retained publication:

```bash
install -d -m 0700 "$HOME/.config"
install -m 0600 /dev/null "$HOME/.config/mosquitto_pub"
# Edit that private Mosquitto client config with one option per line, including:
# -h mqtt.example.net
# -p 8883
# --cafile /path/to/ca.pem
# -u mqtt-raspberry-controller
# -P THE_PASSWORD
mosquitto_pub \
  --topic homeassistant/switch/OLD_DEVICE_ID/OLD_ENTITY_ID/config \
  --null-message --retain --qos 1
```

Never use a wildcard cleanup: MQTT `PUBLISH` topics cannot contain wildcards, and broad deletion risks other devices. When an ACL lists exact topics, retain write access to old Discovery topics through the first successful restart after a rename/removal; remove those old ACL lines only after cleanup. Changing `device.id` similarly requires temporary write access to both the old and new exact topics. If the state file is lost after stale topics were created, the daemon deliberately cannot rediscover ownership from the broker; use explicit manual cleanup.

No Home Assistant birth-topic subscription is needed. Home Assistant receives these retained Discovery configurations when it subscribes after startup, while the controller already republishes on its own MQTT reconnect. A birth-triggered republish would duplicate broker traffic without improving recovery for retained configurations.

## Build

Source compatibility starts at Go 1.24 because `github.com/eclipse/paho.mqtt.golang v1.5.1` requires it. CI and releases use the exact supported security toolchain in `.go-version` (currently Go 1.26.8).

```bash
go mod download
make build VERSION=1.0.0
./bin/mqtt-raspberry-controller --version
```

Use `make check` to run the build, formatting check, tests, vet, module-tidiness check, and packaging checks. `make ci` also runs the race suite. `make coverage` writes `bin/coverage.out`, and `make clean` removes generated build/release artifacts. See [docs/RELEASE.md](docs/RELEASE.md) for reproducible archives, checksums, SBOMs, and release gates.

## Tests

The default suite is hardware-independent and needs no broker:

```bash
go test ./...
go test -race ./...
make check
```

GPIO behavior is exercised through the manager's small line-request seam, including output and input opening, logical state, edge handling, rollback, errors, and close behavior. This portable fake-line coverage runs on Linux CI and developer machines without granting access to GPIO hardware. `gpio-sim`/configfs and physical Raspberry Pi testing remain optional because kernel modules, configfs permissions, and line availability vary by runner; use the packaged Raspberry Pi smoke test below when validating a board and kernel together.

A real MQTT integration test is opt-in. Point `MQTT_TEST_BROKER` at an ephemeral anonymous test broker; the test creates cryptographically unique client IDs and topics, verifies QoS 1 command delivery, and checks retained `online`/`offline` availability:

```bash
docker run --detach --rm --name mqtt-raspberry-controller-test \
  --publish 127.0.0.1:1883:1883 \
  --volume "$PWD/packaging/mosquitto/test.conf:/mosquitto/config/mosquitto.conf:ro" \
  eclipse-mosquitto:2.0.20
trap 'docker stop mqtt-raspberry-controller-test >/dev/null' EXIT
MQTT_TEST_BROKER=tcp://127.0.0.1:1883 go test -race ./mqtt
```

GitHub Actions uses the same digest-pinned Mosquitto version and configuration, waits for broker readiness, then runs formatting, module-tidiness, vet, ordinary tests, broker-backed race tests, staticcheck, govulncheck, a static versioned build assertion, and packaging checks. Physical GPIO remains **NOT RUN** in hosted CI.

## Quick install

Install the latest published Linux release with:

```bash
curl -fsSL https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/heads/main/scripts/install.sh | sh
```

The installer detects `amd64` or `arm64`, downloads the matching published archive, verifies it against the release's `SHA256SUMS`, checks the embedded version without executing the downloaded binary as root, and installs the binary, systemd unit, udev rule, sysusers declaration, example configuration, and restricted password file. It uses `sudo` only after switching to the installer stored in the selected release tag, performs privileged work in a root-owned temporary directory, preserves an existing configuration and password file, rolls replaced files back after an installation failure, and deliberately does not start the service before you configure it.

On a fresh installation with a controlling terminal, the installer offers an interactive MQTT setup. It asks for the broker URL, username, an instance identifier, and a hidden password entered twice. The generated configuration is validated as the unprivileged service account before it replaces the example files. Use `--no-configure` to skip the prompt, or `--configure` to require the wizard. On an existing installation, the forced wizard changes only the canonical broker, username, and password fields and refuses an unsupported YAML layout instead of applying a partial update:

```bash
curl -fsSL https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/heads/main/scripts/install.sh | sh -s -- --configure
```

The wizard configures MQTT and a unique client/device identity. Instance identifiers are normalized to lowercase and must start with a letter or digit so the generated `device.id` is valid. The wizard does not start the service or decide which GPIO lines are safe for the connected hardware. For `mqtt://`, `tcp://`, or `ws://` brokers, it displays a plaintext-transport warning and requires explicit confirmation before setting `allow_insecure_transport: true` and clearing incompatible TLS options; prefer a TLS broker whenever possible.

`SHA256SUMS` detects corruption but does not authenticate the publisher. For tagged public releases, the workflow also creates a GitHub build-provenance attestation. When GitHub CLI is installed, the installer verifies that attestation automatically; set `MQTT_RASPBERRY_REQUIRE_ATTESTATION=1` before `sh` to refuse installation when provenance verification is unavailable. The convenient `main` command inherently trusts the current default branch. For a reproducible installation, replace `X.Y.Z` and fetch the installer from that immutable release tag:

```bash
version=X.Y.Z
curl -fsSL "https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/tags/v${version}/scripts/install.sh" | MQTT_RASPBERRY_VERSION="$version" MQTT_RASPBERRY_REQUIRE_ATTESTATION=1 sh
```

### Uninstall

Remove the program and service integration while preserving configuration, credentials, Discovery state, and the service account:

```bash
curl -fsSL https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/heads/main/scripts/uninstall.sh | sh -s -- --remove
```

To permanently remove the default configuration, password file, Discovery state, service user, and service group as well, use the explicit destructive mode:

```bash
curl -fsSL https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/heads/main/scripts/uninstall.sh | sh -s -- --clear
```

`--clear` preserves the shared `gpio` group and cannot discover custom configuration, credential, or state paths outside the default directories.

## Install

The packaged service uses a static, unprivileged `mqtt-raspberry-controller` account. Create the GPIO group if the distribution does not provide it, then install the sysusers declaration (or use the shown `useradd` fallback):

```bash
make build VERSION=1.0.0
getent group gpio >/dev/null || sudo groupadd --system gpio
sudo install -m 0644 packaging/sysusers.d/mqtt-raspberry-controller.conf /usr/lib/sysusers.d/mqtt-raspberry-controller.conf
sudo systemd-sysusers mqtt-raspberry-controller.conf

# Fallback on systems without systemd-sysusers (do not run if the account exists):
# sudo groupadd --system mqtt-raspberry-controller
# sudo useradd --system --gid mqtt-raspberry-controller --home-dir /nonexistent \
#   --shell /usr/sbin/nologin --comment 'MQTT Raspberry Controller' mqtt-raspberry-controller

sudo install -m 0755 bin/mqtt-raspberry-controller /usr/local/bin/mqtt-raspberry-controller
sudo install -d -o root -g mqtt-raspberry-controller -m 0750 /etc/mqtt-raspberry-controller
sudo install -o root -g mqtt-raspberry-controller -m 0640 configs/config.example.yaml /etc/mqtt-raspberry-controller/config.yaml
sudo install -o root -g root -m 0600 /dev/null /etc/mqtt-raspberry-controller/mqtt-password
systemd-ask-password 'Password for the mqtt-raspberry-controller MQTT user' | sudo tee /etc/mqtt-raspberry-controller/mqtt-password >/dev/null
sudo install -m 0644 packaging/udev/60-mqtt-raspberry-controller.rules /etc/udev/rules.d/60-mqtt-raspberry-controller.rules
sudo install -m 0644 packaging/systemd/mqtt-raspberry-controller.service /etc/systemd/system/mqtt-raspberry-controller.service
sudo udevadm control --reload-rules
sudo udevadm trigger --subsystem-match=gpio --sysname-match=gpiochip0
```

The configuration must be owned by `root:mqtt-raspberry-controller` so the service account can open it; keep its mode at `0640` or stricter. The password source must be a regular file with exact mode `0400`, `0600` or `0640`; systemd's private credential copy is `0400`. The unit imports it with `LoadCredential=`, and the daemon reads only that private copy through the `MQTT_PASSWORD_FILE` path variable; do not put passwords in broker URIs, process environments, or world-readable YAML. Outside systemd, export `MQTT_PASSWORD_FILE` directly with the path of an equally restricted file. Inline `password` remains supported for compatibility but is discouraged. The packaged unit deliberately uses explicit `Environment=` assignments for credential paths and does not load a shell-style `EnvironmentFile=`.

### Configuration variable expansion

The daemon strictly decodes the YAML, including rejection of unknown fields, before expanding environment references in modeled string values. Use `${NAME}`, where names match `[A-Za-z_][A-Za-z0-9_]*`. Expansion is one pass: text supplied by a variable is inserted verbatim and is never expanded again. Use `$$` for a literal dollar, so `$${NAME}` produces the literal text `${NAME}`. A bare `$`, `$NAME`, `${}`, and malformed expressions such as `${BAD-NAME}` remain literal. A valid `${NAME}` whose variable is unset is an error that identifies the field path and variable name without printing the field or substituted value.

Expansion is allowed in these string values only:

- MQTT: `broker`, `username`, `password`, `password_file`, `client_id`, `base_topic`, `keep_alive`, `connect_timeout`; every string under `tls`; and `discovery.prefix` and `discovery.state_file`.
- Device: `id`, `name`, `manufacturer`, and `model`.
- GPIO: each output's `chip`, and each input's `chip`, `bias`, and `debounce`.
- Entities: `id`, `type`, `name`, `icon`, `device_class`; `state.type`, `state.gpio`, `source.type`, and `source.gpio`.
- Actions in `on`, `off`, and `press`: `type`, `gpio`, `command`, each `args` entry, `working_dir`, each `env` value, `timeout`, `duration`, `topic`, and `payload`.

Map keys (including GPIO names and action environment names), YAML structure, numbers, and booleans are never expanded. Because substitution happens after decoding, values containing YAML punctuation, quotes, `#`, or newlines remain one string and cannot create keys or list entries. Quote source scalars as YAML itself requires; expansion does not reinterpret the inserted value as YAML.

The udev rule deliberately matches only `gpiochip0` and enforces owner `root`, group `gpio`, and mode `0660`. Linux GPIO character-device permissions are **chip-level, not line-level**: udev cannot grant access to selected offsets within a chip. Configuration validation and wiring discipline must therefore limit which lines the daemon uses. If the board exposes the intended lines on another chip, add another exact `KERNEL=="gpiochipN"` rule and matching `DeviceAllow=` drop-in rather than using a `gpiochip*` wildcard.

Edit the configuration and broker hostname, then validate through the service's credential environment and start the service:

```bash
sudo systemd-run --wait --pipe --uid=mqtt-raspberry-controller \
  --property=LoadCredential=mqtt-password:/etc/mqtt-raspberry-controller/mqtt-password \
  /bin/sh -c 'export MQTT_PASSWORD_FILE="$CREDENTIALS_DIRECTORY/mqtt-password"; exec /usr/local/bin/mqtt-raspberry-controller --config /etc/mqtt-raspberry-controller/config.yaml --check'
sudo systemctl daemon-reload
sudo systemctl enable --now mqtt-raspberry-controller
sudo journalctl -u mqtt-raspberry-controller -f
```

### Service sandbox and compatibility exceptions

The unit removes all capabilities, sets `NoNewPrivileges=yes`, exposes only `/dev/gpiochip0`, makes the host filesystem read-only, hides home directories, isolates `/tmp` and mount changes, limits namespaces and address families, and places the whole process tree under memory/task/stop limits. `PrivateDevices=yes` is intentionally not used because it would hide the real GPIO device. Configured `command` actions inherit this same sandbox and always run as `mqtt-raspberry-controller`; the service will not silently elevate a configured command.

Keep exceptions narrow and local in `sudo systemctl edit mqtt-raspberry-controller`. For example, give one action a prepared writable directory without weakening the rest of the filesystem:

```ini
[Service]
ReadWritePaths=/var/lib/mqtt-raspberry-controller/action-output
```

Create that directory owned by `mqtt-raspberry-controller`, or install action executables outside `/home` (for example under `/usr/local/libexec`). A command that needs another socket family requires an explicit replacement such as `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`. Override `MemoryMax=` or `TasksMax=` only to a measured bound. For a genuinely privileged operation, do **not** disable `NoNewPrivileges` or add capabilities to this daemon; put the one allow-listed operation in a separately audited root helper/service and authorize only its narrow IPC activation. After any drop-in, run `systemd-analyze verify mqtt-raspberry-controller.service` and review `systemctl cat mqtt-raspberry-controller`.

On systemd 252, `systemd-analyze security --offline=yes packaging/systemd/mqtt-raspberry-controller.service` reports an exposure score of **3.2 (`OK`)**. Scores vary with systemd versions; GPIO and MQTT require the intentional device, supplementary-group, and network exceptions.

### Raspberry Pi root smoke test

`packaging/tests/smoke-raspberry-pi.sh` verifies the installed unit, required configuration/device owners and modes, actual service UID, current-systemd-invocation switch/output/line logs, MQTT `online`/state/`offline`, and a real GPIO OFF/ON toggle restored to its initial state. Installed credentials remain supplied by the unit's `LoadCredential=`, while smoke-client credentials come only from the `MQTT_RASPBERRY_SMOKE_*` environment into private temporary Mosquitto client configuration. The explicit output name must be the `gpio.outputs` name referenced by the tested switch's GPIO state. Every Mosquitto operation, including failure cleanup, has a timeout; restoration waits for a matching live state acknowledgement before the daemon may be stopped. If restoration cannot be confirmed, a daemon that the test started is deliberately left running so queued restoration work is not cut off, and the test fails with a manual-intervention warning. The script stops and restarts the canonical service, so run it only on a safe test output:

```bash
export MQTT_RASPBERRY_SMOKE_CONFIRM=YES
export MQTT_RASPBERRY_SMOKE_HOST=broker.example.net
export MQTT_RASPBERRY_SMOKE_PORT=1883
export MQTT_RASPBERRY_SMOKE_BASE_TOPIC=mqtt-raspberry-controller/test-pi
export MQTT_RASPBERRY_SMOKE_SWITCH_ID=relay
export MQTT_RASPBERRY_SMOKE_OUTPUT_NAME=relay
export MQTT_RASPBERRY_SMOKE_GPIO_CHIP=/dev/gpiochip0
export MQTT_RASPBERRY_SMOKE_GPIO_LINE=17
# Optional authentication/TLS:
# export MQTT_RASPBERRY_SMOKE_USERNAME=mqtt-smoke
# read -rsp 'MQTT password: ' MQTT_RASPBERRY_SMOKE_PASSWORD; export MQTT_RASPBERRY_SMOKE_PASSWORD
# export MQTT_RASPBERRY_SMOKE_CAFILE=/etc/ssl/certs/ca-certificates.crt
sudo --preserve-env=MQTT_RASPBERRY_SMOKE_CONFIRM,MQTT_RASPBERRY_SMOKE_HOST,MQTT_RASPBERRY_SMOKE_PORT,MQTT_RASPBERRY_SMOKE_BASE_TOPIC,MQTT_RASPBERRY_SMOKE_SWITCH_ID,MQTT_RASPBERRY_SMOKE_OUTPUT_NAME,MQTT_RASPBERRY_SMOKE_GPIO_CHIP,MQTT_RASPBERRY_SMOKE_GPIO_LINE,MQTT_RASPBERRY_SMOKE_USERNAME,MQTT_RASPBERRY_SMOKE_PASSWORD,MQTT_RASPBERRY_SMOKE_CAFILE \
  ./packaging/tests/smoke-raspberry-pi.sh
```

`make packaging-check` always verifies staged modes and unit validity. When run as root it applies and asserts staged numeric ownership; without root it verifies the documented owner/group installation commands textually because it cannot chown files to `root` or the synthetic sysusers group. The Raspberry Pi smoke test is the authoritative installed-owner/device check.

## Validate the example configuration

The tracked example can be checked without opening GPIO devices or connecting to MQTT:

```bash
password_file=$(mktemp)
trap 'rm -f "$password_file"' EXIT
chmod 0600 "$password_file"
printf 'development-only-password\n' >"$password_file"
export MQTT_PASSWORD_FILE="$password_file"
./bin/mqtt-raspberry-controller --config ./configs/config.example.yaml --check
```

## MQTT transport, mTLS, and broker ACL

Secure broker schemes (`ssl`, `tls`, `mqtts`, `mqtt+ssl`, `tcps`, and `wss`) verify the broker certificate and require TLS 1.2 by default; set `mqtt.tls.min_version: "1.3"` to require TLS 1.3. An empty `ca_file` uses system roots. A custom CA file augments, rather than replaces, the system pool. `server_name` overrides certificate/DNS verification only when connecting by a different address; verification cannot be disabled. Configure both `cert_file` and restricted `key_file` to enable mTLS. Plain `tcp`, `mqtt`, or `ws` is accepted without an opt-in only for literal loopback addresses or `localhost`; remote plaintext needs `allow_insecure_transport: true`. Authentication is mandatory unless `allow_unauthenticated: true` is explicit.

For mTLS under systemd, install root-owned CA/client certificate material and the private key, copy `packaging/systemd/mqtt-raspberry-controller-mtls.conf` into `/etc/systemd/system/mqtt-raspberry-controller.service.d/mtls.conf`, and add these fields to `mqtt.tls`:

```yaml
ca_file: "${MQTT_CA_FILE}"
cert_file: "${MQTT_CLIENT_CERT_FILE}"
key_file: "${MQTT_CLIENT_KEY_FILE}"
```

The client key must have exact mode `0400`, `0600` or `0640`; systemd credentials use `0400`, while certificate and CA files may be public. Run `systemctl daemon-reload` and `mqtt-raspberry-controller --check` after changes. The check path parses the password, CA, certificate, and key without opening GPIO or connecting to the broker.

Use a dedicated Mosquitto account and install `packaging/mosquitto/mqtt-raspberry-controller.acl` as the ACL for the shipped example. It contains **no wildcards** and grants only the exact command subscriptions and exact state, status, action-publication, and Discovery publications used by that YAML. For example:

```conf
password_file /etc/mosquitto/passwd
acl_file /etc/mosquitto/mqtt-raspberry-controller.acl
allow_anonymous false
```

When adding or renaming an entity, add its exact command topic (`.../set` or `.../press`), state topic where applicable, and exact Discovery topic. Keep the old exact Discovery publication permission until the daemon has deleted that retained configuration. When adding an `mqtt` action, add that exact publication topic. Do not replace these entries with `#` or `+` wildcards.

## Topics

With `base_topic: mqtt-raspberry-controller/example`:

```text
mqtt-raspberry-controller/example/status
mqtt-raspberry-controller/example/relay/set
mqtt-raspberry-controller/example/relay/state
mqtt-raspberry-controller/example/announce/press
mqtt-raspberry-controller/example/contact/state
```

Home Assistant MQTT Discovery is published under the configured discovery prefix, usually `homeassistant`.

## GPIO numbering

GPIO lines are Linux GPIO offsets, not physical Raspberry Pi header pin numbers. Raspberry Pi physical pin 7 is BCM GPIO4, so use `line: 4` for a device connected to that GPIO.
