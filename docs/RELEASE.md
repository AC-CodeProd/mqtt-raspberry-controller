# Release process

## Policy

The supported release toolchain is the exact patch in `.go-version`; `go.mod` remains the source compatibility floor. Releases use SemVer tags (`vMAJOR.MINOR.PATCH`). A release is not evidence of physical GPIO compatibility unless the Raspberry Pi smoke test was separately run and its board/kernel/configuration evidence is attached.

Release Please derives versions from Conventional Commits:

- `fix:` produces a patch release;
- `feat:` produces a minor release;
- `type!:` or a `BREAKING CHANGE:` footer produces a major release;
- documentation, tests, CI and dependency-only commits are included in the changelog when relevant but do not independently trigger a release.

The initial bootstrap release is `1.0.0`. The new repository is initialized by placing the annotated `v1.0.0` tag on its sole root commit and pushing `main` and that tag together; the tag-triggered **Release** workflow then verifies and publishes the initial assets. `.release-please-manifest.json` records `1.0.0` as the latest released version so Release Please calculates only subsequent increments from it. After this bootstrap, do not edit the manifest outside a Release Please pull request.

## Automated maintainer procedure

1. Merge normal, CI-green pull requests into `main` using Conventional Commit titles.
2. The **Release Please** workflow opens or updates one **draft** release pull request containing the proposed version, `.release-please-manifest.json` and `CHANGELOG.md`.
3. The workflow dispatches the standard CI workflow with the exact release-PR head SHA, waits for that run to succeed, verifies that the PR head did not move, then marks the PR ready for review.
4. Review the proposed version and changelog. Before merging, ensure Dependabot changes are reviewed and called vulnerabilities are absent.
5. Optionally run the Raspberry Pi smoke test on a safe output. If omitted, retain `Physical GPIO / Raspberry Pi smoke test: NOT RUN` in the release evidence.
6. Merge the release pull request. Before creating anything, Release Please independently verifies that the exact merged PR head has a successful dispatched CI run; it then creates the exact `vMAJOR.MINOR.PATCH` tag and a temporary draft GitHub Release.
7. The reusable **Release** workflow checks that the tag targets the released commit, reruns all quality gates, builds reproducible Linux amd64/arm64 assets, verifies checksums, versions, architectures and CycloneDX SBOMs, then creates GitHub build-provenance attestations for tagged public releases and attaches the verified assets.
8. After every asset and provenance step succeeds, the workflow publishes the release automatically. If Release Please already created a temporary draft and a later build or verification step fails, that draft remains unpublished and is not visible through `/releases/latest`. For a manually pushed tag without an existing release, a failed build creates no release at all. Correct the cause and rerun the workflow without replacing or moving the tag.

Release Please uses the repository `GITHUB_TOKEN`; no personal access token is required. The repository setting **Allow GitHub Actions to create and approve pull requests** must remain enabled. Because events created by that token do not recursively start pull-request or tag workflows, the automation dispatches CI explicitly and calls the reusable Release workflow directly after creating a release. The manual tag trigger remains a recovery path and follows the same verify-assets-then-publish sequence.

The Release Please job has only the repository permissions needed to maintain release pull requests, tags and draft releases, plus `actions: write` to dispatch CI on its generated branch. The reusable release caller grants `id-token: write` and `attestations: write` so the called build can sign public tagged artifacts; the reusable build itself otherwise remains read-only, and only its asset-publication job receives `contents: write`. Actions use immutable commit SHAs; the broker image uses an immutable manifest digest. The release builder uses `CGO_ENABLED=0`, `-trimpath`, a fixed source timestamp, normalized tar metadata and gzip without timestamps.

## Dry run and manual recovery

Dispatch the **Release** workflow with a SemVer value and leave the tag input empty to build and inspect artifacts without creating or modifying a GitHub Release.

To rebuild assets or publish a release that remained in draft after correcting the workflow, dispatch **Release** from `main` with both the existing version and immutable tag (for example, version `1.0.0` and tag `v1.0.0`). The workflow checks out the tagged commit, reruns every release gate, replaces assets only while the release is still a draft, then publishes it automatically. If the release is already published, the workflow verifies the downloaded assets and `SHA256SUMS` against the new build without modifying the public release; any divergence fails and requires a new patch release.

If Release Please is unavailable, a maintainer may create and push an annotated `vMAJOR.MINOR.PATCH` tag pointing to a commit already reachable from `main`. The tag workflow applies the same validation and publishes only after all verified assets are attached. This is a recovery procedure, not the normal versioning path.

## Local release build

```bash
version=X.Y.Z
syft=$(./scripts/install-syft.sh "$PWD/.cache/syft-1.43.0")
GOTOOLCHAIN=go1.26.8 SYFT="$syft" make release VERSION="$version"
make release-verify VERSION="$version"
```

Artifacts include Linux amd64/arm64 archives, per-binary CycloneDX JSON SBOMs, version records, `SHA256SUMS`, and `RELEASE-VERIFICATION.txt`. `SHA256SUMS` covers every release asset except itself.

## Provenance

For tagged public releases, the workflow generates GitHub artifact attestations from `dist/SHA256SUMS` after every release gate succeeds. The reusable workflow and its Release Please caller both grant `id-token: write` and `attestations: write`; no long-lived signing key or personal access token is used. Verify an archive with GitHub CLI and the expected signer workflow:

```bash
gh attestation verify mqtt-raspberry-controller_VERSION_linux_arm64.tar.gz \
  --repo AC-CodeProd/mqtt-raspberry-controller \
  --signer-workflow AC-CodeProd/mqtt-raspberry-controller/.github/workflows/release.yml \
  --deny-self-hosted-runners
```

Private and dry-run builds do not create attestations. Checksums still detect corruption, but because an archive and `SHA256SUMS` come from the same release, checksum verification alone does not authenticate the publisher.

## Rollback

Do not delete or move a published tag. Mark the affected release as withdrawn, stop the service, install the previous archive after verifying its checksum, restore a compatible configuration backup, run `--check`, then restart. Publish a patch release rather than replacing assets. Discovery ownership state should normally be preserved; restore it only together with the matching `mqtt.client_id` and configuration.
