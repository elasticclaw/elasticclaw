# ElasticClaw E2E Suite

This package contains PR-gated end-to-end tests for real tracker delivery and
provider provisioning.

The E2E paths run against real services:

```text
Depot CI -> ngrok -> ElasticClaw Server -> GitHub Issues -> Daytona -> OpenClaw -> Fireworks Kimi
Depot CI -> ngrok -> ElasticClaw Server -> Linear -> Daytona -> OpenClaw -> Fireworks Kimi
```

Each test creates a workspace and workflow with the ElasticClaw CLI, configures
a workspace GitHub App with the CLI so repositories can clone in Daytona, then
configures the issue tracker through the server API. The GitHub Issues test
creates a real GitHub issue and labels it. The Linear test creates a real Linear
webhook, creates a real Linear issue in a non-trigger state, moves it into the
trigger state, then waits for one Daytona-backed agent to connect and reply.

## Fixture Repo

The dedicated fixture repository is:

```text
elasticclaw/e2e-fixtures
```

The suite creates per-run issues, labels, webhooks, workspaces, workflows, and
agents using a run id. Cleanup closes the issue, removes the webhook, removes
the workspace, and kills the agent.

## Local Run

Run all suites sequentially:

```sh
make e2e
```

Run one suite:

```sh
make e2e-github
make e2e-linear
make e2e-replicated-github
make e2e-replicated-linear
```

The make targets build `bin/elasticclaw` and `bin/claw-bridge-linux-amd64`,
create a temporary reserved ngrok domain named with the current git SHA and
timestamp, start a tunnel for
`ELASTICCLAW_E2E_HUB_ADDR` or `127.0.0.1:8080` using a temporary ngrok config,
and run the same real provider tests that Depot CI runs. The
test server serves the locally built connector through the ngrok tunnel so PR
runs do not depend on a pre-published release artifact. The E2E server protects
that connector route with a per-run token in the download URL. The make target
refuses the shared `elasticclaw.ngrok.app` domain, then kills the ngrok agent it
started, deletes the temporary reserved domain, and removes the temporary ngrok
config when the test exits. The test deletes any Daytona sandboxes whose names
start with `ec-e2e-` before and after the run. It also records any Daytona
sandbox IDs and Replicated CMX VM IDs created during the run, and the make
cleanup path runs explicit finalizers for those IDs even when the main test
fails. Use dedicated Daytona and Replicated API credentials for E2E.
It assumes the required secrets below are already exported in your shell.

## Depot CI Environment

Required secrets:

```text
ELASTICCLAW_E2E_GITHUB_TOKEN
ELASTICCLAW_E2E_GITHUB_APP_ID
ELASTICCLAW_E2E_GITHUB_APP_PRIVATE_KEY
ELASTICCLAW_E2E_LINEAR_API_KEY
ELASTICCLAW_E2E_LINEAR_TEAM_KEY
DAYTONA_API_KEY
REPLICATED_API_TOKEN
FIREWORKS_API_KEY
NGROK_AUTHTOKEN
NGROK_API_KEY
```

Optional secrets:

```text
ELASTICCLAW_E2E_GITHUB_REPO=elasticclaw/e2e-fixtures
ELASTICCLAW_E2E_GITHUB_APP_URL=https://github.com/settings/apps/...
ELASTICCLAW_E2E_GITHUB_APP_INSTALLATION=elasticclaw
ELASTICCLAW_E2E_DAYTONA_API_URL=https://app.daytona.io/api
ELASTICCLAW_E2E_DAYTONA_TARGET=org-allowed-target
ELASTICCLAW_E2E_DAYTONA_SNAPSHOT=daytona-medium
ELASTICCLAW_E2E_LINEAR_TRIGGER_STATE=Todo
ELASTICCLAW_E2E_LINEAR_INITIAL_STATE=Backlog
ELASTICCLAW_E2E_REPLICATED_API_URL=https://api.replicated.com/vendor/v3
ELASTICCLAW_E2E_REPLICATED_INSTANCE_TYPE=r1.small
ELASTICCLAW_E2E_REPLICATED_TTL=1h
```

The GitHub token needs access to the fixture repo with:

```text
Issues: read/write
Metadata: read
Webhooks: read/write
```

The GitHub App must be installed on the fixture repo and have repository
contents permissions sufficient for ElasticClaw to mint a checkout token.

Set `ELASTICCLAW_E2E_DAYTONA_TARGET` and, when needed,
`ELASTICCLAW_E2E_DAYTONA_SNAPSHOT` to a target/snapshot combination available
to the Daytona organization. If Daytona rejects the configured target/class
with a 403 "region is not available" response, the Daytona E2E marks the run as
skipped because the account cannot provision the required sandbox class.

The Linear API key must be able to create webhooks and issues for the team in
`ELASTICCLAW_E2E_LINEAR_TEAM_KEY`. The trigger state defaults to `Todo`; set
`ELASTICCLAW_E2E_LINEAR_TRIGGER_STATE` to the exact state name you want the
workflow to watch. Set `ELASTICCLAW_E2E_LINEAR_INITIAL_STATE` only if the test
cannot infer a non-trigger state for the initial issue creation.

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
