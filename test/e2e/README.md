# ElasticClaw E2E Suite

This package contains PR-gated end-to-end tests for real tracker delivery and
provider provisioning.

The first E2E path runs against real services:

```text
Depot CI -> ngrok -> ElasticClaw Server -> GitHub Issues -> Daytona -> OpenClaw -> Fireworks Kimi
```

It creates a workspace and workflow with the ElasticClaw CLI, configures a
workspace GitHub App with the CLI, configures the GitHub Issues tracker through
the server API, creates a real GitHub issue that asks for a dad joke, labels it,
then waits for one Daytona-backed agent to connect and reply.

## Fixture Repo

The dedicated fixture repository is:

```text
elasticclaw/e2e-fixtures
```

The suite creates per-run issues, labels, webhooks, workspaces, workflows, and
agents using a run id. Cleanup closes the issue, removes the webhook, removes
the workspace, and kills the agent.

## Local Run

Run:

```sh
make e2e
```

`make e2e` builds `bin/elasticclaw`, stops any already-running ngrok agent,
creates a temporary reserved ngrok domain named with the current git SHA and
timestamp, starts a tunnel for `ELASTICCLAW_E2E_HUB_ADDR` or `127.0.0.1:8080`
using a temporary ngrok config, and runs the same real Daytona + GitHub Issues
test that Depot CI runs. The make target refuses the shared
`elasticclaw.ngrok.app` domain, then kills the ngrok agent it started, deletes
the temporary reserved domain, and removes the temporary ngrok config when the
test exits. It assumes the required secrets below are already exported in your
shell.

## Depot CI Environment

Required secrets:

```text
ELASTICCLAW_E2E_GITHUB_TOKEN
ELASTICCLAW_E2E_GITHUB_APP_ID
ELASTICCLAW_E2E_GITHUB_APP_PRIVATE_KEY
DAYTONA_API_KEY
FIREWORKS_API_KEY
NGROK_AUTHTOKEN
NGROK_API_KEY
```

Optional vars:

```text
ELASTICCLAW_E2E_GITHUB_REPO=elasticclaw/e2e-fixtures
ELASTICCLAW_E2E_GITHUB_APP_URL=https://github.com/settings/apps/...
ELASTICCLAW_E2E_GITHUB_APP_INSTALLATION=elasticclaw
```

The GitHub token needs access to the fixture repo with:

```text
Issues: read/write
Metadata: read
Webhooks: read/write
```

The GitHub App must be installed on the fixture repo and have repository
contents permissions sufficient for ElasticClaw to mint a checkout token.

## Planned Matrix

The suite should grow in separate jobs so failures stay attributable:

```text
github-issues: webhook, polling recovery, duplicate prevention
linear: webhook, polling recovery, duplicate prevention
shortcut: webhook, polling recovery, duplicate prevention
exedev: provisioning reaches agent connected
daytona: provisioning reaches agent connected and repositories clone
replicated: provisioning reaches agent connected and repositories clone
models: Fireworks Kimi, plus additional production models
```
