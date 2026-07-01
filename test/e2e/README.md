# ElasticClaw E2E Suite

This package contains PR-gated end-to-end tests for real tracker delivery and
provider provisioning.

The E2E paths run against real services:

```text
Depot CI -> ngrok -> ElasticClaw Server -> GitHub Issues -> Daytona -> OpenClaw -> Fireworks Kimi
Depot CI -> ngrok -> ElasticClaw Server -> Linear -> Daytona -> OpenClaw -> Fireworks Kimi
Depot CI -> ngrok -> ElasticClaw Server -> Jira Cloud -> Daytona -> OpenClaw -> Fireworks Kimi
Local shell -> ngrok -> ElasticClaw Server -> manual workflow -> Docker -> OpenClaw -> Fireworks Kimi
```

Each test creates a workspace and workflow with the ElasticClaw CLI, configures
a workspace GitHub App with the CLI so repositories can clone in Daytona, then
configures the issue tracker through the server API. The GitHub Issues test
creates a real GitHub issue and labels it. The Linear test creates a real Linear
webhook, creates a real Linear issue in a non-trigger state, moves it into the
trigger state, then waits for one Daytona-backed agent to connect and reply. The
Jira test creates a real Jira Cloud issue, labels it, transitions it into the
trigger state, delivers a Jira-shaped webhook payload to the E2E hub, and lets
the poller run long enough to verify duplicate prevention.
The Docker workflow test creates only local resources, triggers a manual
workflow, and stops at the agent `connected` status so local runs can reproduce
Docker-to-hub tunnel failures without Depot.

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
make e2e-jira
make e2e-replicated-github
make e2e-replicated-linear
make e2e-replicated-jira
make e2e-docker-workflow
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

The Docker workflow target builds `elasticclaw/claw-agent:dev` locally and uses
the same ngrok-backed hub path as CI. It requires Docker, `FIREWORKS_API_KEY`,
`NGROK_AUTHTOKEN`, and `NGROK_API_KEY`, but does not require GitHub, Linear,
Jira, Daytona, Replicated, or Depot credentials. Override the local image with
`ELASTICCLAW_E2E_DOCKER_IMAGE`; set `ELASTICCLAW_E2E_DOCKER_NETWORK` only when
the agent should join a specific Docker network. When debugging a failed run,
use the container name or id in the hub log with `docker logs <container>` and
`docker inspect <container>` before rerunning cleanup.

## Depot CI Environment

Required secrets:

```text
ELASTICCLAW_E2E_GITHUB_TOKEN
ELASTICCLAW_E2E_GITHUB_APP_ID
ELASTICCLAW_E2E_GITHUB_APP_PRIVATE_KEY
ELASTICCLAW_E2E_LINEAR_API_KEY
ELASTICCLAW_E2E_LINEAR_TEAM_KEY
ELASTICCLAW_E2E_JIRA_BASE_URL
ELASTICCLAW_E2E_JIRA_USERNAME
ELASTICCLAW_E2E_JIRA_TOKEN
ELASTICCLAW_E2E_JIRA_PROJECT_KEY
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
ELASTICCLAW_E2E_LINEAR_TRIGGER_STATE=Todo
ELASTICCLAW_E2E_LINEAR_INITIAL_STATE=Backlog
ELASTICCLAW_E2E_JIRA_MANUAL_WEBHOOK=true
ELASTICCLAW_E2E_REPLICATED_API_URL=https://api.replicated.com/vendor/v3
ELASTICCLAW_E2E_REPLICATED_INSTANCE_TYPE=r1.small
ELASTICCLAW_E2E_REPLICATED_TTL=1h
ELASTICCLAW_E2E_DOCKER_IMAGE=elasticclaw/claw-agent:dev
ELASTICCLAW_E2E_DOCKER_NETWORK=
```

The GitHub token needs access to the fixture repo with:

```text
Issues: read/write
Metadata: read
Webhooks: read/write
```

The GitHub App must be installed on the fixture repo and have repository
contents permissions sufficient for ElasticClaw to mint a checkout token.

The Linear API key must be able to create webhooks and issues for the team in
`ELASTICCLAW_E2E_LINEAR_TEAM_KEY`. The trigger state defaults to `Todo`; set
`ELASTICCLAW_E2E_LINEAR_TRIGGER_STATE` to the exact state name you want the
workflow to watch. Set `ELASTICCLAW_E2E_LINEAR_INITIAL_STATE` only if the test
cannot infer a non-trigger state for the initial issue creation.

The Jira Cloud user is configured with `ELASTICCLAW_E2E_JIRA_USERNAME` and
`ELASTICCLAW_E2E_JIRA_TOKEN`. It must be able to browse the project, create
issues, edit issues, transition issues, add labels, and delete issues for
cleanup. `ELASTICCLAW_E2E_JIRA_PROJECT_KEY` must point at an existing project
whose workflow supports `Bug` issues moving from `To do` to `Ready for Agent`
and then to `Agent Working`.

Jira Cloud dynamic webhook registration is restricted to Connect and OAuth apps,
so the default E2E mode uses real Jira Cloud REST mutations and then posts a
Jira-shaped webhook payload to the ngrok-backed ElasticClaw webhook endpoint.
Leave `ELASTICCLAW_E2E_JIRA_MANUAL_WEBHOOK=true` for CI. Set it to `false` only
when a separate Jira app or automation rule is already delivering real Jira
webhooks to the current E2E ngrok URL.

## Planned Matrix

The suite should grow in separate jobs so failures stay attributable:

```text
github-issues: webhook, polling recovery, duplicate prevention
linear: webhook, polling recovery, duplicate prevention
shortcut: webhook, polling recovery, duplicate prevention
jira: Jira Cloud REST mutation, webhook processing, polling duplicate prevention
exedev: provisioning reaches agent connected
daytona: provisioning reaches agent connected and repositories clone
replicated: provisioning reaches agent connected and repositories clone
models: Fireworks Kimi, plus additional production models
```
