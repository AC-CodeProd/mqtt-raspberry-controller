# Troubleshooting

Start with configuration validation and current-invocation logs:

```bash
sudo systemctl status mqtt-raspberry-controller
sudo journalctl -u mqtt-raspberry-controller -b --no-pager
sudo systemctl show mqtt-raspberry-controller -p InvocationID -p MainPID
```

## Configuration fails

Run the README's `systemd-run` validation command so `LoadCredential=` matches the installed service. Unknown YAML keys are rejected. Confirm duration syntax, absolute command paths, restrictive credential modes, unique entity/GPIO IDs, and required MQTT authentication.

## MQTT connection fails

Confirm the broker URL has an explicit port, its certificate name matches, the CA is readable, system time is correct, and the exact ACL topics cover subscriptions/publications. Do not disable certificate verification. Use broker logs without placing passwords on command lines.

## GPIO open fails

Confirm `/dev/gpiochip0` exists, the udev rule has produced `root:gpio` mode `0660`, and the service account is in the `gpio` supplementary group. GPIO `line` values are Linux offsets, not header pin numbers. Add an exact unit/udev allowance for another chip; never broaden to a wildcard.

## Service is restart-looping

Inspect only the current boot/invocation, validate the configuration, and check command paths against the sandbox. `ProtectHome=yes` blocks executables under home directories; install audited helpers under `/usr/local/libexec`. Add only narrow `ReadWritePaths=` or address-family overrides.

## Discovery entries are stale

Preserve `mqtt.client_id` and the discovery state file. Follow the README's upgrade/manual-cleanup sequence and never wildcard-delete broker topics.

## Release verification fails

Discard the download if `sha256sum --check` fails. Confirm the archive name/architecture and that `--version` equals the GitHub tag without its leading `v`. Report a missing/invalid SBOM or mismatched asset on the release; do not install it.
