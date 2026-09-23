# Security operations

- Use TLS 1.2 or newer and dedicated least-privilege MQTT credentials. Remote plaintext requires an explicit unsafe-transport acknowledgement.
- Store passwords and private keys in restrictive regular files. The packaged service imports the MQTT password through `LoadCredential=`; never put secrets in YAML, broker URLs, command arguments, or broad environment files.
- Keep exact MQTT ACL topics. Do not replace them with `#` or `+` wildcards.
- Treat configured command actions as code. Use absolute audited executables, explicit arguments/environment, and preserve the systemd sandbox. Do not configure self-daemonizing programs.
- GPIO character-device permission is chip-wide. Validate offsets and wiring; udev cannot restrict individual lines.
- Review weekly Dependabot PRs. Release with the exact `.go-version`, and block on called vulnerabilities reported by `govulncheck`.
- Verify release SHA-256 checksums before installation and retain the CycloneDX SBOM with deployment records.

Report suspected vulnerabilities privately to the repository owner rather than publishing credentials, exploit details, configuration, logs, or broker addresses in a public issue. Rotate affected broker credentials and certificates, revoke unauthorized ACL access, preserve logs, and deploy a reviewed patch release.
