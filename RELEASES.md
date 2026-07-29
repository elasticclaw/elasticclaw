# ElasticClaw Release Policy

This document defines how ElasticClaw is released, versioned, and supported. It is designed to keep releases predictable for users who run ElasticClaw on their own servers.

## Versioning

ElasticClaw follows [Semantic Versioning](https://semver.org/) (SemVer):

- **MAJOR** (`X.0.0`): Incompatible changes that require user action when upgrading.
- **MINOR** (`0.X.0`): New features, improvements, and non-breaking additions.
- **PATCH** (`0.0.X`): Bug fixes, security fixes, and other backward-compatible corrections.

A version is written as `MAJOR.MINOR.PATCH`, for example `1.4.2`.

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

- **Patch releases** are made as needed for bug fixes and security fixes.
- **Minor releases** are made roughly every 2–4 weeks, or when a meaningful set of features is ready.
- **Major releases** are planned and announced in advance. They are used for breaking changes that cannot be avoided.

There is no guarantee of a fixed schedule. The release coordinator decides when the quality bar is met.

## Backward compatibility

Because users self-host ElasticClaw and manage their own upgrades, backward compatibility is important.

- **Patch releases** must be backward compatible with the same minor version.
- **Minor releases** should be backward compatible with the previous minor version. New features should be opt-in when possible.
- **Major releases** may contain breaking changes. Breaking changes are documented in release notes with migration guidance.

Stability boundaries include, but are not limited to:

- Public HTTP API endpoints and their request/response schemas.
- `hub.yaml` configuration options.
- Workspace and workflow YAML schemas.
- Command-line interface behavior and flags.
- Documented upgrade paths between releases.

If a change unintentionally breaks compatibility, it is treated as a bug and fixed in a patch release.

## Supported versions

Because ElasticClaw is self-hosted and moving quickly, we encourage users to run the latest release.

| Version | Support status |
|---------|----------------|
| Latest release | Fully supported: bug fixes, security fixes, and patches |
| Previous minor release | Security fixes and critical bug fixes for up to 6 months, or until the next major release, whichever is shorter |
| Older releases | No longer supported |

The release coordinator decides whether a fix is critical enough to backport to the previous minor release.

## Release process

1. **Prepare**: A maintainer proposes a release by reviewing merged changes and identifying the version bump.
2. **Branch or tag**: Patch releases are typically tagged from the release branch or main. Minor and major releases may use a release branch when multiple stabilization commits are needed.
3. **Verify**: Run the full test suite, including container and integration tests. Ensure builds and release artifacts are produced successfully.
4. **Release notes**: Write release notes summarizing changes, fixes, deprecations, and any breaking changes or migration steps.
5. **Tag and publish**: Create a signed Git tag, publish binaries and images, and update any release metadata.
6. **Announce**: Post the release in the project’s communication channels.

## Deprecation policy

- Deprecations are announced in release notes at least one minor version before the feature is removed or changed incompatibly.
- Deprecated features are removed only in a major release, unless the feature is experimental and explicitly marked as unstable.
- Migration guidance is provided when a feature is deprecated and when it is removed.

## Security releases

Security fixes are released as patch versions. See [SECURITY.md](SECURITY.md) for the vulnerability reporting process and coordinated disclosure policy.

## Changes to this policy

Changes to this release policy require approval from at least two maintainers, including the Project Lead, as described in [GOVERNANCE.md](GOVERNANCE.md).
