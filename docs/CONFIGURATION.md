# Configuration reference

The YAML decoder rejects unknown fields. Start from `configs/config.example.yaml` and run `mqtt-raspberry-controller --config FILE --check` before service startup.

## Root keys

| Key | Required | Meaning |
| --- | --- | --- |
| `mqtt` | yes | Broker, authentication, transport, topics and Discovery. |
| `device` | yes | Stable Home Assistant device identity. |
| `gpio` | yes | Named input/output lines. |
| `entities` | yes, non-empty | `switch`, `button`, or `binary_sensor` definitions. |

## `mqtt`

- `broker` (required): URL with an explicit port for network schemes. Supported: `mqtt`, `tcp`, `ws`, `ssl`, `tls`, `mqtts`, `mqtt+ssl`, `tcps`, `wss`; `unix` requires one clean absolute socket path.
- `username` with exactly one of `password` or `password_file`; inline `password` is compatibility-only. Set `allow_unauthenticated: true` only intentionally and without credentials.
- `allow_insecure_transport`: required for plaintext non-loopback network brokers.
- `client_id`: defaults to `device.id`; keep stable and unique. `base_topic`: defaults to `device.id`, with no MQTT wildcards.
- `qos`: `0`, `1`, or `2`, default `1`. `keep_alive`: positive Go duration, default `30s`. `connect_timeout`: positive Go duration, default `10s`.
- `tls.ca_file`, `cert_file`, `key_file`, `server_name`, `min_version`: only valid on secure schemes. Client cert/key are a pair; minimum is `1.2` or `1.3`.
- `discovery.enabled`: default true. `prefix`: default `homeassistant`, no wildcards. `state_file`: clean absolute path, default `/var/lib/mqtt-raspberry-controller/discovery-state.json`.

## `device`

`id` is required and matches `[a-z0-9][a-z0-9_-]*`; `name` is required. `manufacturer` and `model` default to `mqtt-raspberry-controller`.

## `gpio`

`outputs.NAME` and `inputs.NAME` use lowercase ID-shaped names. Each has `chip` (default `gpiochip0`), non-negative `line`, and `active_low`. Outputs add `initial`. Inputs add `bias` (`disabled`, `pull_up`, `pull_down`; default `disabled`) and optional positive `debounce` duration. A chip/line pair may appear only once.

## `entities`

Every entity needs unique `id`, `type`, and non-empty `name`; optional common keys are `icon` and `device_class`.

- `switch`: `state.type` is `memory` or `gpio`; GPIO state also needs `state.gpio`. `initial_state` applies to memory state. Non-empty `on` and `off` action lists are required.
- `button`: a non-empty `press` action list is required.
- `binary_sensor`: `source.type: gpio` and a named `source.gpio` input are required.

## Actions

- `gpio`: named output `gpio` plus boolean `value`.
- `command`: absolute regular executable `command`; optional string `args`, existing absolute `working_dir`, explicit `env` map, and positive `timeout` (default `10s`). No shell is used.
- `delay`: positive `duration`.
- `mqtt`: exact publication `topic`, `payload`, optional `qos` (`0`..`2`) and `retain`.
- All action types accept `ignore_error`; false stops the sequence on failure.

Environment substitution and its exact allowlist are documented in the root README. Map keys, structure, numbers and booleans are never expanded.
