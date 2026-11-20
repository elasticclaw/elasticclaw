# ElasticClaw Release Policy

This document defines how ElasticClaw is released, versioned, and supported. It is designed to keep releases predictable for users who run ElasticClaw on their own servers.

## Versioning

ElasticClaw uses [Calendar Versioning](https://calver.org/) (CalVer) for release tags:

- Tags are written as `YYYY.M.D`, where:
  - `YYYY` is the four-digit year,
  - `M` is the month without leading zeros,
  - `D` is the day without leading zeros.
- Same-day hotfixes use an additional `.MICRO` component: `YYYY.M.D.MICRO`.
- Prerelease versions append a hyphen and label: `YYYY.M.D-<label>`.

Examples: `2026.7.7`, `2026.7.7.1`, `2026.7.7-rc1`.

The version is embedded in release binaries and used by the install script and update checks.

## Release coordinator

Each release is shepherded by a **release coordinator**, usually a maintainer. The coordinator is responsible for:

- Deciding when a release is ready based on the contents on the main branch.
- Ensuring the full test suite and release checks pass.
- Writing or reviewing release notes.
- Creating the release tag and publishing artifacts.
- Announcing the release.

Until the maintainer team grows, the Project Lead may act as the default release coordinator. The coordinator can delegate tasks to other maintainers and contributors.

## Release cadence

Releases are driven by content, not by a strict calendar:

- **Regular releases** are tagged with the current date (`YYYY.M.D`) when a meaningful set of changes is ready.
- **Same-day hotfixes** add a `.MICRO` component (`YYYY.M.D.MICRO`) for urgent fixes to the same day's release.
- **Prerelease versions** (`YYYY.M.D-<label>`) are used for testing or early access.

There is no guarantee of a fixed schedule. The release coordinator decides when the quality bar is met.

## Backward compatibility

Because users self-host ElasticClaw and manage their own upgrades, backward compatibility is important.

- **Hotfix releases** (`YYYY.M.D.MICRO`) on the same day are backward compatible with the base release (`YYYY.M.D`).
- **Regular releases** should be backward compatible with the previous regular release. New features should be opt-in when possible.
- **Breaking changes** are documented in release notes with migration guidance.

Stability boundaries include, but are not limited to:

- Public HTTP API endpoints and their request/response schemas.
- `hub.yaml` configuration options.
- Workspace and workflow YAML schemas.
- Command-line interface behavior and flags.
- Documented upgrade paths between releases.

If a change unintentionally breaks compatibility, it is treated as a bug and fixed in a hotfix release.

## Supported versions

Because ElasticClaw is self-hosted and moving quickly, we encourage users to run the latest release.

| Version | Support status |
|---------|----------------|
| Latest release | Fully supported: bug fixes, security fixes, and hotfixes |
| Previous regular release | Security fixes and critical bug fixes for up to 6 months, or until the next breaking release, whichever is shorter |
| Older releases | No longer supported |

The release coordinator decides whether a fix is critical enough to backport to the previous regular release.

## Release process

1. **Prepare**: A maintainer proposes a release by reviewing merged changes and identifying the version tag.
2. **Branch or tag**: Hotfixes are typically tagged from the release branch or main. Regular releases may use a release branch when multiple stabilization commits are needed.
3. **Verify**: Run the full test suite, including container and integration tests. Ensure builds and release artifacts are produced successfully.
4. **Release notes**: Write release notes summarizing changes, fixes, deprecations, and any breaking changes or migration steps.
5. **Tag and publish**: Create a signed Git tag using the CalVer format, publish binaries and images, and update any release metadata.
6. **Announce**: Post the release in the project’s communication channels.

## Deprecation policy

- Deprecations are announced in release notes at least one regular release before the feature is removed or changed incompatibly.
- Deprecated features are removed only in a breaking release, unless the feature is experimental and explicitly marked as unstable.
- Migration guidance is provided when a feature is deprecated and when it is removed.

## Security releases

Security fixes are released as hotfix versions on the current CalVer date. See [SECURITY.md](SECURITY.md) for the vulnerability reporting process and coordinated disclosure policy.

## Changes to this policy

Changes to this release policy require approval from at least two maintainers, including the Project Lead, as described in [GOVERNANCE.md](GOVERNANCE.md).
