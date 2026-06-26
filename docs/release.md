# Release Process

kruntimes uses SemVer tags with a leading `v`, for example `v0.1.0`.

The project is currently `v0.x experimental`. The CRDs are `v1alpha1`, so minor
releases may still include breaking API or behavior changes. Release notes must
make those changes explicit.

## Versioning

- Patch releases fix bugs, security issues, and documentation errors without
  intentional API or behavior changes.
- Minor releases add features, API fields, runtime behavior, or installation
  changes. During `v0.x`, they may include breaking changes when the release
  notes describe the impact and migration path.
- Major releases are reserved for stable API compatibility commitments.

Keep these versions aligned for a release:

- Git tag: `vX.Y.Z`
- `charts/kruntimes/Chart.yaml`: `version` and `appVersion`
- `charts/kruntimes-runtimes/Chart.yaml`: `version` and `appVersion`
- README examples that name a concrete released version

Do not reuse or move published release tags. If a release artifact is bad after
publication, cut a new patch release and mark the bad GitHub release as
superseded.

## Changelog

Every user-facing change should have a `CHANGELOG.md` entry under
`Unreleased`. Use these headings when they apply:

- `Added`
- `Changed`
- `Deprecated`
- `Removed`
- `Fixed`
- `Security`

Before tagging a release:

1. Move `Unreleased` entries into `## X.Y.Z - YYYY-MM-DD`.
2. Leave a fresh empty `## Unreleased` section at the top.
3. Call out breaking changes and required migrations before the regular lists.
4. Keep internal-only refactors out of the changelog unless they affect users,
   operators, runtime authors, or contributors.
5. Update `docs/compatibility.md` when the release changes Kubernetes, Helm,
   Go, Python, or published `krt` artifact support.
6. Update `docs/operations.md` when the release changes installation,
   upgrade, uninstall, troubleshooting, backup, or restore behavior.
7. Update `docs/custom-runtime.md` when the release changes the Runtime Server
   protocol, Runtime CRD template contract, or execution semantics.

## Release Notes

GitHub release notes should be written from the changelog and include:

- release type and support level,
- upgrade notes and breaking changes,
- image tags and verification instructions,
- CLI binary, checksum, and provenance verification instructions,
- known limitations for the release,
- links to the changelog and installation documentation.

Use this outline:

```markdown
## Summary

## Breaking Changes

## Upgrade Notes

## Images

## Verification

## Known Limitations

## Changelog
```

For `v0.x` releases, include an explicit sentence that `v1alpha1` APIs are
experimental and may change in later minor releases.

## Preflight Checks

Run these checks before creating the tag:

```bash
make test
make test-integration
make test-helm
make test-race
make govulncheck
```

Run `make e2e` before public release tags and any release that changes Runtime,
scheduler, controller, Helm, CRD, artifact, or Workflow behavior.

Confirm these generated files are clean after preflight:

```bash
git status --short
```

## Tagging

After checks pass:

1. Commit the changelog and version updates.
2. Create and push an annotated tag:

   ```bash
   git tag -a v0.1.0 -m "kruntimes v0.1.0"
   git push upstream v0.1.0
   ```

3. Confirm the `Release Images` workflow publishes signed images with SBOM and
   provenance attestations.
4. Confirm the `Release CLI` workflow uploads `krt` archives, checksums, and
   provenance attestations to the GitHub release.
5. Draft the GitHub release from the changelog and publish it only after the
   release artifacts are available.

## Artifact Verification

### Container Images

Published container images are expected under:

- `ghcr.io/<owner>/kruntimes-scheduler:<version>`
- `ghcr.io/<owner>/kruntimes-controller:<version>`
- `ghcr.io/<owner>/kruntimes-runtimed:<version>`
- `ghcr.io/<owner>/kruntimes-bash-runtime:<version>`
- `ghcr.io/<owner>/kruntimes-python-runtime:<version>`

Verify signatures with `cosign`:

```bash
cosign verify ghcr.io/<owner>/kruntimes-controller:0.1.0 \
  --certificate-identity-regexp 'https://github.com/.*/.github/workflows/release-images.yml@refs/tags/v0.1.0' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Also confirm the image digest shown in GitHub Packages matches the digest in
the `Release Images` workflow output.

### krt CLI

Published `krt` release assets are expected under the GitHub release:

- `krt_vX.Y.Z_linux_amd64.tar.gz`
- `krt_vX.Y.Z_linux_arm64.tar.gz`
- `krt_vX.Y.Z_darwin_amd64.tar.gz`
- `krt_vX.Y.Z_darwin_arm64.tar.gz`
- `krt_vX.Y.Z_windows_amd64.tar.gz`
- `krt_vX.Y.Z_checksums.txt`

Verify checksums after downloading the desired archive and checksum file:

```bash
sha256sum --check --ignore-missing krt_v0.1.0_checksums.txt
```

Verify GitHub artifact provenance:

```bash
gh attestation verify krt_v0.1.0_linux_amd64.tar.gz \
  --repo <owner>/kruntimes
```

The attestation subject digest must match the downloaded archive digest.

## Failed Releases

If tagging succeeds but artifact publication fails:

1. Fix the workflow or source issue in a normal PR.
2. Cut a new patch release after validation passes.
3. Do not retag the original version.
4. Add a release note explaining which version supersedes the failed release if
   any artifacts were visible to users.
