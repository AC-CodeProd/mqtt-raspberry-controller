# Install and upgrade

## Quick installation

Install the latest supported Linux release with:

```bash
curl -fsSL https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/heads/main/scripts/install.sh | sh
```

The installer supports Linux `amd64` and `arm64`. It downloads the matching archive and `SHA256SUMS` from the same published release, verifies the checksum, rejects unsafe archive paths and links, and checks the embedded version without executing the downloaded binary as root. Before using `sudo`, it switches from the mutable `main` bootstrap to the installer stored in the selected release tag. Privileged downloads, verification, extraction, and installation then occur in a root-owned `0700` temporary directory. Existing `/etc/mqtt-raspberry-controller/config.yaml` and `/etc/mqtt-raspberry-controller/mqtt-password` files are preserved, replaced files are restored after an installation failure, and the service is not started automatically because the example broker, GPIO lines, and credentials must be reviewed first.

A fresh installation with a controlling terminal offers an interactive wizard. It reads directly from `/dev/tty`, so it also works with the documented `curl | sh` command. The wizard asks for the broker URL, username, instance identifier, and password with terminal echo disabled. It writes the password only to a temporary `0600` file, validates the generated configuration in a sandboxed transient unit running as `mqtt-raspberry-controller`, and atomically replaces the configuration and credential files only after validation succeeds. It never places the password in command-line arguments, environment variables, YAML, or logs.

Pass `--no-configure` to suppress the automatic offer. Pass `--configure` to require a terminal and run the wizard explicitly, including on an existing installation:

```bash
curl -fsSL https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/heads/main/scripts/install.sh | sh -s -- --configure
```

On a fresh configuration, the instance identifier is normalized to lowercase, must start with a letter or digit, and produces valid unique `client_id`, `base_topic`, `device.id`, and `device.name` values. On an existing configuration, the wizard changes only the canonical two-space-indented `mqtt.broker`, `mqtt.username`, and password fields. It requires exactly one matching broker and username field and refuses other YAML layouts before replacing the credential, preventing a partial update. GPIO assignments remain untouched and the service remains stopped until they have been reviewed.

A broker using `mqtt://`, `tcp://`, or `ws://` sends credentials and messages without transport encryption. The wizard warns before accepting one of these schemes and requires an explicit confirmation; acceptance activates the canonical `mqtt.allow_insecure_transport: true` setting, replaces the canonical `mqtt.tls` block with an empty mapping, and then validates the complete result. Declining returns to the broker prompt. Prefer `tls://`, `mqtts://`, `tcps://`, or `wss://` when the broker supports TLS.

A checksum proves that the archive matches `SHA256SUMS`; because both are release assets, it does not by itself authenticate the publisher. Tagged public releases also receive GitHub build-provenance attestations. If GitHub CLI is installed, the installer verifies the archive against the repository and `.github/workflows/release.yml`. Set `MQTT_RASPBERRY_REQUIRE_ATTESTATION=1` to make this verification mandatory.

The one-line `main` bootstrap is convenient but necessarily trusts the current default branch. For a specific reproducible release, replace `X.Y.Z` and use the tagged installer:

```bash
version=X.Y.Z
curl -fsSL "https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/tags/v${version}/scripts/install.sh" | MQTT_RASPBERRY_VERSION="$version" MQTT_RASPBERRY_REQUIRE_ATTESTATION=1 sh
```

If a privileged installation fails after creating previously absent system identities or directories, replaced files and newly created configuration/credential files are rolled back, but the idempotent service user, groups, and empty directories may remain. A later installer run safely reuses them.

## Uninstall

Safe removal preserves configuration, credentials, Discovery state, and the service account so the installation can be restored later without losing ownership or state:

```bash
curl -fsSL https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/heads/main/scripts/uninstall.sh | sh -s -- --remove
```

Full cleanup is intentionally explicit and irreversible for the default data paths:

```bash
curl -fsSL https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/heads/main/scripts/uninstall.sh | sh -s -- --clear
```

Both modes stop and disable the service, remove the binary, systemd unit, udev rule, and sysusers declaration, then reload systemd and udev. `--clear` additionally deletes `/etc/mqtt-raspberry-controller` and `/var/lib/mqtt-raspberry-controller`, then removes the `mqtt-raspberry-controller` user and group when the platform tools permit it. The shared `gpio` group is never removed. Custom paths configured outside the default directories must be reviewed and removed manually.

Like the installer, the normal non-root uninstaller resolves the selected release and switches to `scripts/uninstall.sh` from that tag before invoking `sudo`. Set `MQTT_RASPBERRY_VERSION=X.Y.Z` before `sh` to select a specific published tag.

## Verify a downloaded release

Download the archive, its SBOM, and `SHA256SUMS` from the same GitHub Release. Keep only the relevant checksum lines, or download all assets, then run:

```bash
sha256sum --check SHA256SUMS
```

Do not install when a checksum fails. Inspect the CycloneDX JSON SBOM with tooling that supports CycloneDX 1.x. Extract the archive and verify the embedded version:

```bash
tar -xzf mqtt-raspberry-controller_VERSION_linux_arm64.tar.gz
./mqtt-raspberry-controller_VERSION_linux_arm64/mqtt-raspberry-controller --version
```

Choose `linux_arm64` for 64-bit Raspberry Pi OS and `linux_amd64` for x86-64 Linux. No other operating system or architecture is released.

## Service installation

Follow the commands in the root README to create the service account, install the binary/configuration/udev rule/systemd unit, and create the root-owned MQTT password credential. Run configuration validation before enabling the service. The binary is static (`CGO_ENABLED=0`), but GPIO access still requires Linux, a compatible kernel GPIO character device, the packaged udev policy, and correct board wiring.

## Upgrade

1. Back up `/etc/mqtt-raspberry-controller/config.yaml`, the credential files, and `/var/lib/mqtt-raspberry-controller/discovery-state.json`.
2. Verify the new release checksums and version.
3. Read generated release notes for configuration changes.
4. Stop the unit, atomically replace `/usr/local/bin/mqtt-raspberry-controller`, and run `--check` through the service credential context.
5. Start the unit and inspect its current invocation logs and MQTT availability/state.

Use the rollback procedure in [RELEASE.md](RELEASE.md) if validation or startup fails.
