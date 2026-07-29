# ElasticClaw Governance

This document describes how the ElasticClaw project is governed. It is intentionally lightweight and will be updated as the project grows.

## Overview

ElasticClaw is an open-source platform for self-hosted workflow automation around coding agents. It is licensed under the Apache License 2.0.

ElasticClaw was started by Replicated, which currently provides the majority of development resources. As the contributor base and funding model evolve, this governance document will be updated to reflect a broader, more independent project structure.

## Roles

### Project Lead

Marc Campbell (`marc@replicated.com`) is the Project Lead. The Project Lead:

- Has final decision authority on technical and governance disputes that cannot be resolved by maintainers.
- Appoints and removes maintainers.
- Represents the project publicly when needed.
- Ensures the project remains focused on the needs of its users and contributors.

### Maintainers

Maintainers are trusted contributors with merge rights and decision-making authority over the project. They are responsible for:

- Reviewing and merging pull requests.
- Triaging issues and prioritizing work.
- Defining and upholding project standards for code, tests, and documentation.
- Enforcing the project’s code of conduct.
- Guiding the project’s technical direction.

Current maintainers are listed in [MAINTAINERS.md](MAINTAINERS.md).

### Contributors

Anyone who contributes to the project—whether through code, documentation, tests, issue triage, or discussion—is a contributor. Contributions are reviewed by maintainers before being merged.

## Decision-making

The project uses lazy consensus for most decisions and explicit approval for significant changes.

### Routine changes

Routine changes include bug fixes, minor features, documentation updates, and test improvements. A pull request may be merged when:

- At least one maintainer has approved it, and
- No maintainer has objected within a reasonable review period (typically 72 hours, excluding weekends and holidays).

Maintainers should wait for feedback from relevant reviewers before merging. If unsure, ask for another review rather than merge quickly.

### Significant changes

Significant changes require approval from at least two maintainers. These include:

- Changes to the project’s architecture or public APIs.
- New dependencies or major dependency upgrades.
- Breaking changes or deprecations.
- Changes to security, governance, or the code of conduct.
- Changes to the [release or support policy](RELEASES.md).

### Disputes

If maintainers disagree and cannot reach consensus, the Project Lead makes the final decision. If the Project Lead has a conflict of interest on the decision, the remaining maintainers will vote and the majority prevails.

## Architecture Decision Records (ADRs)

Substantial or breaking changes should be documented with a lightweight Architecture Decision Record (ADR). An ADR is a short markdown file that explains the problem, the chosen approach, and the trade-offs.

### When to write an ADR

Write an ADR for changes that are:

- Architectural or design-level decisions with broad impact.
- Public API or configuration format changes.
- Breaking changes or deprecations.
- New dependencies with security, licensing, or operational implications.
- Changes to security model, authentication, or authorization.
- Significant UX or workflow changes.

Smaller changes can include the same context in the PR description instead of a formal ADR.

### ADR process

1. Open a PR with a proposed ADR in `docs/adr/` using the filename `NNNN-short-title.md`. Use the next available number.
2. Discuss the ADR in the PR. Update the document based on feedback.
3. Merge once it is approved as a significant change (at least two maintainer approvals). ADRs are merged with status `proposed` or `accepted`.
4. If a decision is later reversed or superseded, update the ADR status to `deprecated` or `superseded` and add a note explaining the change.

### Suggested ADR template

```markdown
# Title

- Date: YYYY-MM-DD
- Status: proposed | accepted | deprecated | superseded
- Deciders: list of maintainers who reviewed

## Context and problem statement

What is the decision we need to make?

## Decision

What we decided to do.

## Consequences

What becomes easier or harder because of this decision.

## Alternatives considered

What else we considered and why we did not choose it.
```

## Conflict of interest

Maintainers and contributors must declare conflicts of interest when participating in decisions where they, their employer, or a related party may benefit financially or commercially. They must abstain from voting on those decisions.

Replicated currently provides the majority of development resources for ElasticClaw. Replicated employees who participate as maintainers act in the interest of the project and its users, not solely in Replicated’s commercial interest.

## Sponsorship and neutrality

At this stage, ElasticClaw is a Replicated open-source project. Replicated provides the majority of development resources. As the project matures, governance will be updated to reflect a broader contributor base and more independent oversight.

The project remains open to contributions from any individual or organization, regardless of commercial relationship to Replicated or ElasticClaw.

## Code of conduct

We are committed to making ElasticClaw a welcoming project for everyone. Contributors are expected to:

- Be respectful and constructive in all interactions.
- Welcome newcomers and help them learn the project.
- Disagree respectfully, focusing on technical merits.
- Avoid harassment, discrimination, and personal attacks.

Maintainers have the authority to moderate discussions, close issues, and remove contributors who violate these standards.

If you experience or witness behavior that violates these standards, contact the Project Lead directly at `marc@replicated.com`. Reports will be handled confidentially.

## Security

Security issues are handled separately from regular issue tracking. See [SECURITY.md](SECURITY.md) for how to report vulnerabilities and the project’s disclosure policy.

## Releases

The release policy, versioning, and support schedule are documented in [RELEASES.md](RELEASES.md).

## Changes to governance

Changes to this document require approval from at least two maintainers, including the Project Lead.

## Acknowledgments

This governance model is inspired by lightweight open-source governance practices used by many small-to-medium open-source projects. It will be revisited as the project grows.
