---
name: elasticclaw-workflow-v2
description: Use when the user is authoring, editing, or converting ElasticClaw workspace and workflow YAML files that use schema_version v2 (2). Helps write deterministic state-machine workflows, named workspace resources, and migrate from v1.
---

# ElasticClaw Workspace & Workflow v2

This skill documents the deterministic v2 schema introduced for ElasticClaw workspaces **and** workflows (schema_version: `2` or `v2`). Both workspace v2 (`elasticclaw-config.yaml`) and workflow v2 are fully supported and shipped; they are a strict, typed, state-machine-driven replacement for the transcript-driven v1 format.

Use this skill when the user asks about:
- Writing or editing `elasticclaw-config.yaml` or workflow `.yaml` files with `schema_version: 2`
- Converting v1 workspaces/workflows to v2
- Understanding why v2 validation fails
- Designing deterministic workflows, CI policies, review gates, or context bundles

## Core concepts

- **Workspace v2** declares authority: repositories, credentials, source-control connections, CI pipelines, issue trackers, review systems, and knowledge sources.
- **Workflow v2** declares a deterministic state machine: states, transitions, commands, events, CI/review policies, and delivery constraints. It does **not** embed repository lists or credentials.
- **Facts** are durable key/value observations produced by the hub. Workflows read protected namespaces (`ci.*`, `pull_request.*`, `review.*`, `effects.*`, `workflow.*`, `operator.*`) and may write only `work.*` or `custom.*`.
- **Effects** are durable actions the hub performs: `agent.task`, `ci.trigger`, `ci.retry`, `ci.cancel`, `issue.comment`.
- **Freeform chat/text markers** (`[DONE]`, `[READY_TO_COMMIT]`, `message_contains`) are never trusted control signals in v2.

## Schema version forms

Valid v2 identifiers (case-insensitive): `2`, `v2`.

```yaml
schema_version: 2
name: my-workspace
```

Integer `2` is the canonical authored form. The parser also accepts `v2`.

## Workspace v2 schema (`elasticclaw-config.yaml`)

Top-level keys are strict. Unknown fields are rejected.

```yaml
schema_version: 2
name: <required workspace name>

repositories:
  <name>:
    provider: github
    repository: owner/repo
    permissions: read | write         # default read; write is required for effects that push branches or open PRs
    source_control: <connection name>   # optional but required for PR/CI wiring
    checkout:
      ref: default | main | <sha>       # optional
      depth: full | <number>            # optional

execution:
  provider: <provider name>   # e.g. daytona, replicated, exedev, docker, lambda-microvms, noop
  nix: true | false
  docker: true | false
  tools:
    - git
    - gh
  capability_restrictions:
    execute_command: false      # narrows the provider; set false to forbid exec.run
    dependency_update: false    # narrows the provider; set false to forbid dependency.update

credentials:
  <name>:
    secret: <SECRET_NAME>     # secret *name* reference, never the value

source_control:
  connections:
    <name>:
      provider: github
      credentials: <credential name>
      capability_restrictions:
        <cap>: false          # only narrow capabilities; true is rejected if provider lacks it

ci:
  connections:
    <name>:
      provider: github_actions | depot | jenkins
      source_control: <source-control connection name>
      credentials: <credential name>
      base_url: <url>         # required for jenkins
      capability_restrictions:
        trigger_run: false
        cancel_run: false
  pipelines:
    <name>:
      connection: <ci connection name>
      repository: <repository name>
      workflow: <file>        # github_actions: e.g. ci.yml
      project: <project>      # depot
      pipeline: <pipeline>    # depot
      job: <job>              # jenkins

issue_trackers:
  connections:
    <name>:
      provider: linear
      credentials: <credential name>
      capability_restrictions:
        <cap>: false

review_systems:
  connections:
    <name>:
      provider: github | greptile
      source_control: <source-control connection name>
      credentials: <credential name>
      capability_restrictions:
        <cap>: false

knowledge:
  connections:
    <name>:
      provider: rag | <retrieval provider>
      base_url: <url>
      credentials: <credential name>
  sources:
    <name>:
      type: workspace_files | repository_files | retrieval
      scope: organization | repository
      required: true | false
      connection: <knowledge connection name>   # required for retrieval
      repositories: [<repo name>]              # only for repository_files
      paths: [RELATIVE_PATH, ...]                 # required for workspace_files / repository_files
      query: <string>                            # retrieval
      parameters: {}                               # retrieval
```

### Resource naming rules

Names used as map keys (`repositories`, `credentials`, `connections`, `pipelines`, `sources`, workflow `states`, `transitions`, `commands`, `policies`) must match:

```regex
^[A-Za-z0-9][A-Za-z0-9_.-]*$
```

Secret references must match:

```regex
^[A-Za-z_][A-Za-z0-9_]*$
```

### Supported providers and default capabilities

| Provider type | Default capabilities |
|---|---|
| `github_actions`, `depot`, `jenkins` | `observe_runs`, `observe_checks`, `trigger_run`, `retry_run`, `cancel_run`, `fetch_logs`, `reconcile` |
| `github` (source-control / review) | `observe_runs`, `observe_checks`, `fetch_logs`, `reconcile` |
| `linear`, `greptile` | `observe_runs`, `reconcile` |

`capability_restrictions` may only **narrow** (set `false`). Setting `true` for a capability the provider does not have is rejected. Unknown capability names are rejected.

### Repository permissions

Each repository in a workspace v2 can declare `permissions: read` (default) or `permissions: write`. The hub requests an installation token with the matching level when the workflow needs to interact with the repository:

- `read` is sufficient for observation, checkout, and reading files.
- `write` is required for effects that push branches, open/update pull requests, or create commits (e.g. `dependency.update`).
- Any other value is normalized to `read`.

Use `permissions: write` on any repository that a `dependency.update` or future write effect targets.

### Knowledge source rules

- `workspace_files`: `scope: organization`, relative `paths` inside the workspace files.
- `repository_files`: `scope: repository`, relative `paths`, optional `repositories` list (defaults to relevant workspace repos). Each repository name must exist in `repositories`.
- `retrieval`: `scope: organization | repository`, required `connection` pointing to a `knowledge.connections` entry, plus `query` / `parameters`.
- Paths must be non-empty, relative, and not contain `..`.

### Example workspace v2

```yaml
schema_version: 2
name: engineering

repositories:
  primary:
    provider: github
    repository: elasticclaw/elasticclaw
    permissions: write
    source_control: github-production

execution:
  provider: daytona
  nix: true
  docker: true
  tools:
    - git
    - gh

credentials:
  github_app:
    secret: GITHUB_APP_PRIVATE_KEY
  depot_token:
    secret: DEPOT_TOKEN
  linear_api_key:
    secret: LINEAR_API_KEY

source_control:
  connections:
    github-production:
      provider: github
      credentials: github_app

ci:
  connections:
    github-actions:
      provider: github_actions
      source_control: github-production
      credentials: github_app
      capability_restrictions:
        trigger_run: false
        cancel_run: false
    depot:
      provider: depot
      credentials: depot_token
  pipelines:
    github-pr:
      connection: github-actions
      repository: primary
      workflow: ci.yml
    depot-container:
      connection: depot
      repository: primary
      project: elasticclaw
      pipeline: container-build

issue_trackers:
  connections:
    product-linear:
      provider: linear
      credentials: linear_api_key

review_systems:
  connections:
    github-reviews:
      provider: github
      source_control: github-production

knowledge:
  sources:
    engineering-principles:
      type: workspace_files
      scope: organization
      required: true
      paths: [ENGINEERING.md, PRODUCT.md]
    repository-instructions:
      type: repository_files
      scope: repository
      required: true
      paths: [AGENTS.md]
```

## Workflow v2 schema

Top-level keys are strict. Unknown fields are rejected.

```yaml
schema_version: 2
name: <required>
enabled: true | false        # default false if omitted; required for automatic runs
manual_trigger: true | false  # default true; allows `elasticclaw workflow trigger` even when enabled: false
initial_state: <state name>    # required

states:
  <name>:
    description: <string>
    phase: setup | context | plan | build | test | pr | review | done
    terminal: true | false
    invariant: <predicate tree>
    on_enter:
      assert: <fact map>
      set: <fact map>
      effects:
        - <operation>: <config>

transitions:
  <name>:
    from: <state> | [<state>, ...]
    on: <event kind>            # optional; empty means state-entry evaluation
    when: <predicate tree>
    to: <state>
    assert: <fact map>
    set: <fact map>
    effects:
      - <operation>: <config>

commands:
  <name>:
    from: <state> | [<state>, ...]
    to: <state>
    require_reason: true | false

ci:
  policies:
    <name>:
      all | any:
        - pipeline: <pipeline name>
          checks: [check-name, ...]
      satisfied_for: current_pr_head
      # or not: <nested policy>

review:
  policies:
    <name>:
      all | any:
        - connection: <review connection name>
          approvals:
            minimum: <int>
      invalidate_on_new_head: true
      # or not: <nested policy>

delivery:
  pull_requests:
    required: true | false
    minimum: <int>
    ci_policy: <ci policy name>
    review_policy: <review policy name>
    completion: all_merged

events:
  <event kind>:
    clauses:
      - from: <state> | [<state>, ...]
        when: <predicate tree>
        assert: <fact map>
        set: <fact map>
        effects:
          - <operation>: <config>
        ignore: true | false
```

### State requirements

- `initial_state` must name a defined state.
- If `enabled: true`, every state must have a `phase`.
- Terminal states cannot have outgoing transitions or commands.
- `phase` must be one of: `setup`, `context`, `plan`, `build`, `test`, `pr`, `review`, `done`.

### Transitions and determinism

- For the same `from` state + `on` event, `when` clauses must be **provably disjoint**. The validator rejects overlapping clauses with a witness value.
- The `on` field names an event kind (e.g. `pull_request.verified_open`, `ci.policy.evaluated`).
- Transitions with no `on` are evaluated on state entry.
- `from` may be a single string or a list.

### Commands

Commands are operator-initiated transitions (e.g. cancel, retry). They are graph edges like transitions but require authenticated invocation.

```yaml
commands:
  cancel:
    from: [implementing, awaiting_ci, awaiting_review]
    to: cancelled
    require_reason: true
```

### Events

Custom event definitions let the workflow react to hub observations. Each event kind has ordered clauses. Clauses are matched by `from` state + `when`. The `from` field is required for deterministic event handling.

```yaml
events:
  ci.run.completed:
    clauses:
      - from: awaiting_ci
        when:
          conclusion:
            equals: failure
        assert:
          work.needs_fix: true
        effects:
          - agent.task:
              prompt: Investigate the CI failure.
```

### Allowed effect operations

Each effect is a single-key map. The operation determines the required config.

| Effect | Required config | Capability / dependency |
|---|---|---|
| `agent.task` | `prompt` or `instructions`; optional `include_facts: [fact keys, ...]` | None (hub-managed) |
| `exec.run` | `command: <shell command>`; optional `timeout: <duration>` | Execution provider must have `execute_command` |
| `dependency.update` | `ecosystems: [<name>, ...]`; optional `paths`, `exclude_paths`, `grouping`, `include_major`, `separate_major`, `separate_security`, `separate_runtime`, `allow`, `ignore`, `timeout` | Execution provider must have `dependency_update` |
| `ci.trigger` | `pipeline: <pipeline name>` | Connection must have `trigger_run` |
| `ci.retry` | `pipeline: <pipeline name>` | Connection must have `retry_run` |
| `ci.cancel` | `pipeline: <pipeline name>` | Connection must have `cancel_run` |
| `issue.comment` | `connection: <issue tracker connection name>` | Named connection must exist in workspace |

`agent.task` `include_facts` must be 1–20 unique non-empty fact keys. Conversation/transcript facts are forbidden.

### Fact namespaces

Workflows may write only `work.*` or `custom.*` in `assert`, `set`, and event clause `assert`/`set`.

Protected namespaces (read-only, written by hub adapters or effects):
- `ci.*`
- `pull_request.*`
- `review.*`
- `effects.*`
- `workflow.*`
- `operator.*`
- `exec.*` (written by `exec.run` and `dependency.update` effects)

Example:

```yaml
assert:
  work.needs_fix: true
set:
  custom.investigation_id: "abc-123"
```

### Effect receipts: `exec.run` and `dependency.update`

When an `exec.run` or `dependency.update` effect completes or fails, the bridge emits a typed receipt and the hub writes the result into the protected `exec.*` namespace. Workflow transitions can read these facts without trusting the claw as the source.

`exec.run` completion facts:

- `exec.last_run.succeeded` — boolean
- `exec.last_run.exit_code` — integer
- `exec.last_run.stdout` — string
- `exec.last_run.stderr` — string
- `exec.last_run.error` — string (human-readable error when `succeeded` is false)

`dependency.update` completion facts (also available under `exec.dependency_update.*`):

- `exec.dependency_update.succeeded` — boolean
- `exec.dependency_update.error` — string
- `exec.dependency_update.files_changed` — list of file paths
- `exec.dependency_update.ecosystems` — list of processed ecosystems
- `exec.dependency_update.manifests` — list of discovered manifests
- `exec.dependency_update.updates` — list of available updates
- `exec.dependency_update.commands` — list of commands the effect produced

A common pattern is a transition on `exec.run.failed` that moves to a `no_changes` or `failed` state when the script exits non-zero:

```yaml
transitions:
  no_changes_detected:
    from: detect_changes
    on: exec.run.failed
    when:
      exec.last_run.succeeded:
        equals: false
    to: no_changes
```

`dependency.update` emits `dependency.update.completed` / `dependency.update.failed` events, which are also producer-authorized by the engine rather than the claw.

### Predicate language (`when`, `invariant`)

Only these operators are allowed:

- `equals`
- `not_equals`
- `in` (list)
- `not_in` (list)
- `exists` (boolean)
- `all` (list of predicates, conjunction)
- `any` (list of predicates, disjunction)

Rejected operators include `regex`, `matches`, `contains`, `starts_with`, `gt`, `gte`, `lt`, `lte`, `script`, `javascript`, `shell`, `expression`.

Examples:

```yaml
when:
  pull_request:
    state: open

when:
  ci:
    policy: merge_ready
    status: satisfied

when:
  all:
    - pipeline:
        equals: depot-container
    - conclusion:
        equals: failure
```

### CI policy structure

```yaml
ci:
  policies:
    merge_ready:
      all:
        - pipeline: github-pr
          checks: [lint, unit-tests]
        - pipeline: depot-container
          checks: [container-build]
      satisfied_for: current_pr_head
```

Rules:
- Top-level CI policy must use `all`, `any`, or `not`.
- Leaf entries require `pipeline` and non-empty `checks`.
- `satisfied_for` must be `current_pr_head` if present.
- Pipeline names must exist in the paired workspace (`ci.pipelines`).

### Review policy structure

```yaml
review:
  policies:
    required_review:
      all:
        - connection: github-reviews
          approvals:
            minimum: 1
      invalidate_on_new_head: true
```

Rules:
- Top-level review policy must use `all`, `any`, or `not`.
- Leaf entries require `connection` and `approvals.minimum` (non-negative integer).
- `invalidate_on_new_head` must be `true` if present.
- Connection name must exist in the paired workspace (`review_systems.connections` or `source_control.connections`).

### Delivery constraints

```yaml
delivery:
  pull_requests:
    required: true
    minimum: 1
    ci_policy: merge_ready
    review_policy: required_review
    completion: all_merged
```

- `required: true` implies `minimum >= 1`.
- `completion` must be `all_merged` if present.
- `ci_policy` and `review_policy` must name existing policies.
- Workflows **cannot** declare a repository list under `delivery`. Repository authority comes from the workspace.

### Example workflow v2

```yaml
schema_version: 2
name: pull-request-delivery
enabled: true
initial_state: implementing

states:
  implementing:
    description: Work is in progress.
    phase: build
  awaiting_ci:
    description: A verified pull request exists and CI is unresolved.
    phase: pr
    invariant:
      pull_request:
        state: open
  fixing:
    description: Verified evidence indicates more work is required.
    phase: build
  awaiting_review:
    description: CI policy is satisfied.
    phase: review
  ready_to_merge:
    description: Ready.
    phase: review
  completed:
    phase: done
    terminal: true
  cancelled:
    phase: done
    terminal: true

transitions:
  pr_opened:
    from: implementing
    on: pull_request.verified_open
    when:
      pull_request:
        state: open
    to: awaiting_ci

  ci_satisfied:
    from: awaiting_ci
    on: ci.policy.evaluated
    when:
      ci:
        policy: merge_ready
        status: satisfied
    to: awaiting_review

  ci_failed:
    from: awaiting_ci
    on: ci.policy.evaluated
    when:
      ci:
        policy: merge_ready
        status: unsatisfied
    to: fixing

  fixes_pushed:
    from: fixing
    on: pull_request.head_changed
    to: awaiting_ci

  review_satisfied:
    from: awaiting_review
    on: review.policy.evaluated
    when:
      review:
        policy: required_review
        status: satisfied
    to: ready_to_merge

  review_unsatisfied:
    from: awaiting_review
    on: review.policy.evaluated
    when:
      review:
        policy: required_review
        status: unsatisfied
    to: fixing

  pull_request_merged:
    from: ready_to_merge
    on: pull_request.merged
    to: completed

commands:
  cancel:
    from: [implementing, awaiting_ci, fixing, awaiting_review, ready_to_merge]
    to: cancelled
    require_reason: true

ci:
  policies:
    merge_ready:
      all:
        - pipeline: github-pr
          checks: [lint, unit-tests]
        - pipeline: depot-container
          checks: [container-build]
      satisfied_for: current_pr_head

review:
  policies:
    required_review:
      all:
        - connection: github-reviews
          approvals:
            minimum: 1
      invalidate_on_new_head: true

delivery:
  pull_requests:
    required: true
    minimum: 1
    ci_policy: merge_ready
    review_policy: required_review
    completion: all_merged

events:
  ci.run.completed:
    clauses:
      - from: awaiting_ci
        when:
          all:
            - pipeline:
                equals: depot-container
            - conclusion:
                equals: failure
        assert:
          work.ci_failure_investigation_requested: true
        effects:
          - agent.task:
              prompt: Investigate the Depot CI failure.
```

## Converting from v1 to v2

Use the CLI conversion path:

```bash
# Workspace: path may be a directory (elasticclaw-config.yaml) or a YAML file
elasticclaw workspace convert .elasticclaw/workspaces/my-ws
elasticclaw workspace convert ./elasticclaw-config.yaml --to 2 -o v2.yaml
elasticclaw workspace convert ./ws --in-place

# Workflow: --workspace points to a workspace v2 YAML or directory for pair validation
elasticclaw workflow convert examples/workflows/github-issue.yaml
elasticclaw workflow convert ./wf.yaml --workspace ./ws -o wf.v2.yaml
elasticclaw workflow convert ./github-issue.yaml --in-place
```

The converter produces a draft with warnings. Key conversion rules:

- `provider` / `nix` / `docker` move to `execution`.
- `repositories` list becomes a named map; one placeholder `source_control` connection is created.
- `env.*.secret` and top-level `secrets` become `credentials.*`.
- Inline `env.*` values are dropped (v2 workspace does not embed env values).
- `webhook_secrets` are not represented yet.
- v1 `stages` become v2 `states`.
- `entry: true` becomes `initial_state`.
- `on_enter.inject` becomes an `agent.task` effect.
- `on_enter.add_labels` / `remove_labels` are not auto-converted.
- `on_enter.run` (shell/CI hooks) are not auto-converted; model as `exec.run` effects for deterministic shell execution, or as CI pipeline effects / `agent.task` for agent-driven work.
- v1 dependency-update actions map to the `dependency.update` effect. Add `permissions: write` to the target repository and ensure the execution provider grants `dependency_update`.
- v1 triggers (`pr_merged`, `pr_closed`, `pr_opened`) map to v2 transition events.
- `message_contains`, `message_matches`, `[DONE]`, `[READY_TO_COMMIT]` are never trusted in v2.
- The converted workflow is always `enabled: false` (a draft).
- v1-only fields (`integration`, `trigger`, `inputs`, `volumes`, `concurrency_group`) are not represented in v2 and are reported as warnings.

Always review the conversion warnings and pair-validate the workspace + workflow before enabling.

## Validation

The server validates v2 documents when saving through the API or CLI. You can also reason about validity by checking:

1. Schema version is `2` or `v2`.
2. No unknown top-level keys.
3. All resource names are valid.
4. All references resolve (credentials → connections, pipelines → connections, workflow policies → workspace resources).
5. `from` states exist, `to` states exist, terminal states have no outgoing edges.
6. Transition/event `when` clauses do not overlap for the same state + event.
7. Effects reference real pipelines/connections and required capabilities are not restricted.
   - `exec.run` requires an execution provider with `execute_command`.
   - `dependency.update` requires an execution provider with `dependency_update`.
8. Facts written by the workflow are in `work.*` or `custom.*`.
9. Repositories that write code or open PRs use `permissions: write`.

## Common pitfalls

- **Embedding secrets in YAML**: `credentials.*.secret` must be a secret *name*, never a PEM block or token value. Multi-line or `BEGIN` strings are rejected.
- **Inventing capabilities**: `capability_restrictions` cannot grant a capability the provider does not have.
- **Repository list in workflow**: `delivery.pull_requests` has no `repositories` field. Authority comes from the workspace.
- **Text markers as control**: `[DONE]` and `message_contains` are inert prose in v2. Use explicit transitions, events, or commands.
- **Missing phases**: An `enabled: true` workflow requires every state to declare a `phase`.
- **Overlapping transitions**: Two transitions from the same state on the same event with overlapping `when` clauses are rejected.
- **Writing protected facts**: `assert`/`set` cannot write `ci.*`, `pull_request.*`, `review.*`, `effects.*`, `workflow.*`, `operator.*`.
- **Unknown fields**: v2 parsing uses `KnownFields(true)`, so typos at any level fail loudly.
- **Capability mismatch**: `ci.trigger` requires the CI connection to have `trigger_run: true`. If the workspace restricted it to `false`, the effect is rejected.
- **Execution capability mismatch**: `exec.run` requires the workspace `execution.provider` to grant `execute_command`; `dependency.update` requires `dependency_update`. Restricting these to `false` rejects the effect.
- **Script working directory in `exec.run`**: The command runs in the workspace root. If the script is part of the workspace files, prefix it with `cd "$HOME/.openclaw/workspace" && bash scripts/my-script.sh` so it is found in the deployed workspace.
- **Repository permissions for `dependency.update`**: The target repository must use `permissions: write` in the workspace, otherwise the effect cannot push a branch or open a PR.
- **Protected `exec.*` facts**: Workflows may read `exec.last_run.*` and `exec.dependency_update.*` but cannot write them with `assert`/`set`.

## Runtime and activation notes

- A workflow must set `enabled: true` to be activated automatically by the hub. The hub gates activation on successful assembly of the workspace's organizational context (knowledge sources, credentials, etc.); failures keep the run suspended rather than silently proceeding.
- Workflow state is durable and versioned. The hub preserves state across claw failover and reconnects via a typed `workflow.sync` snapshot on the control channel.
- Delivery is dynamic: the claw may submit a `delivery` manifest, but the hub verifies every PR through the workspace's source-control connections before it becomes a `VerifiedPullRequest` that the state machine observes.
- CI and review events are guarded by delivery revisions. A policy evaluation is tied to the PR head SHA at the time it was observed, so new commits re-trigger evaluation rather than reusing stale evidence.

## Reference: control protocol

v2 runs communicate over a dedicated typed control channel (`control.v2`) separate from the conversation WebSocket. The bridge must register and advertise support for the `control.v2` protocol; otherwise the hub will reject the run. Conversation text is never a control input.

Hub → claw messages:

- `workflow.sync` — current state snapshot on connect/reconnect
- `agent.task.assign` — start a durable agent task
- `agent.task.cancel` — cancel an in-flight agent task
- `exec.run.assign` — execute a deterministic shell command
- `dependency.update.assign` — execute a deterministic dependency-update pass
- `artifact.request` — request a required artifact
- `checkpoint.request` — request a checkpoint
- `run.suspend` / `run.resume` / `run.terminate` — lifecycle commands

Claw → hub messages:

- `agent.task.started` / `agent.task.heartbeat` / `agent.task.completed` / `agent.task.failed` — task lifecycle
- `exec.run.started` / `exec.run.heartbeat` / `exec.run.completed` / `exec.run.failed` — command execution lifecycle
- `dependency.update.started` / `dependency.update.heartbeat` / `dependency.update.completed` / `dependency.update.failed` — dependency update lifecycle
- `plan.submitted` — a plan has been submitted for review
- `delivery.submitted` — a delivery manifest (e.g., PR URLs) has been submitted
- `pull_request.claimed` — a PR has been claimed for delivery
- `help.requested` — operator help requested

As a workflow author you do not write these messages directly; they are produced by the hub runtime. However, understand that v2 workflows are **event-driven and deterministic**, not chat-driven.

## Quick checklist for a new v2 workflow

1. Define the workspace with repositories, credentials, source-control, CI pipelines, and review/issue connections.
2. Create a workflow with `schema_version: 2`, `name`, `initial_state`, and `states`.
3. Add `phase` to every state if `enabled: true`.
4. Mark terminal states with `terminal: true`.
5. Add transitions for hub-owned events (`pull_request.verified_open`, `ci.policy.evaluated`, `review.policy.evaluated`, `pull_request.merged`).
6. Add commands for operator actions (`cancel`, `retry`).
7. Define CI and review policies if you use `delivery.pull_requests`.
8. Add custom `events` clauses for provider-specific signals.
9. Keep `assert`/`set` facts under `work.*` or `custom.*`.
10. Use `exec.run` for deterministic shell scripts and `dependency.update` for automated dependency updates.
11. Set `permissions: write` on repositories that will be modified by effects.
12. Pair-validate workspace + workflow before enabling.
