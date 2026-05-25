# ElasticClaw E2E Suite

This package contains PR-gated end-to-end tests for real tracker delivery and
provider provisioning.

The default test is a contract check and does not contact external services:

```sh
go test ./test/e2e
```

External tests run only when `ELASTICCLAW_E2E=1` is set. The first enabled path
starts the current ElasticClaw binary, exposes it through the supplied public
URL, configures a workspace-scoped GitHub Issues tracker, creates a real issue
in the fixture repository, and asserts that exactly one noop-backed agent is
created from the webhook.

## Fixture Repo

The dedicated fixture repository is:

```text
elasticclaw/e2e-fixtures
```

The suite creates per-run issues, labels, webhooks, workspaces, and workflows
using a run id. Cleanup closes the issue, removes the webhook, and removes the
workspace even when the test fails.

## CI Environment

Required for external GitHub Issues E2E:

```text
ELASTICCLAW_E2E=1
ELASTICCLAW_E2E_BIN=/path/to/elasticclaw
ELASTICCLAW_E2E_PUBLIC_URL=https://example.ngrok.app
ELASTICCLAW_E2E_GITHUB_TOKEN=...
ELASTICCLAW_E2E_GITHUB_REPO=elasticclaw/e2e-fixtures
ELASTICCLAW_E2E_RUN_ID=pr-123-1-sha
```

The GitHub token needs access to the fixture repo with:

```text
Issues: read/write
Metadata: read
Webhooks: read/write
```

## Planned Matrix

The suite should grow in separate jobs so failures stay attributable:

```text
github-issues: webhook, polling recovery, duplicate prevention
linear: webhook, polling recovery, duplicate prevention
shortcut: webhook, polling recovery, duplicate prevention
exedev: provisioning reaches agent connected
daytona: provisioning reaches agent connected and repositories clone
replicated: provisioning reaches agent connected and repositories clone
```
