# ElasticClaw Workspace & Workflow v2 Schema Reference

This is a reference for the deterministic `schema_version: 2` YAML format used by `elasticclaw-config.yaml` (workspace) and workflow files. It replaces the transcript-driven v1 format.

- Workspace v2 declares authority: repositories, credentials, source-control connections, CI pipelines, issue trackers, review systems, and knowledge sources.
- Workflow v2 declares a deterministic state machine: states, transitions, commands, events, CI/review policies, and delivery constraints. It does not embed repository lists or credentials.
- Freeform chat markers (`[DONE]`, `message_contains`) are never trusted control signals in v2.

## Schema version

Use `schema_version: 2`. `v2` is also accepted (case-insensitive). Integer `2` is the canonical form.

## Workspace v2 (`elasticclaw-config.yaml`)

Top-level keys are strict. Unknown fields are rejected.

```yaml
schema_version: 2
name: <required workspace name>

repositories:
  <name>:
    provider: github
    repository: owner/repo
    source_control: <connection name>   # optional, but required for PR/CI wiring
    checkout:
      ref: default | main | <sha>       # optional
      depth: full | <number>            # optional

execution:
  provider: <provider name>             # e.g. daytona, docker, noop
  nix: true | false
  docker: true | false
  tools:
    - git
    - gh
  capability_restrictions:              # narrow-only, like connection restrictions
    execute_command: false
    dependency_update: false

credentials:
  <name>:
    secret: <SECRET_NAME>               # secret *name* reference, never the value

source_control:
  connections:
    <name>:
      provider: github
      credentials: <credential name>
      capability_restrictions:
        <cap>: false                   # only narrow capabilities; true is rejected if provider lacks it

ci:
  connections:
    <name>:
      provider: github_actions | depot | jenkins
      source_control: <source-control connection name>
      credentials: <credential name>
      base_url: <url>                   # required for jenkins
      capability_restrictions:
        trigger_run: false
        cancel_run: false
  pipelines:
    <name>:
      connection: <ci connection name>
      repository: <repository name>
      workflow: <file>                  # github_actions: e.g. ci.yml
      project: <project>                # depot
      pipeline: <pipeline>              # depot
      job: <job>                        # jenkins

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
      paths: [RELATIVE_PATH, ...]              # required for workspace_files / repository_files
      query: <string>                          # retrieval
      parameters: {}                           # retrieval
```

### Resource naming rules

Map keys (`repositories`, `credentials`, `connections`, `pipelines`, `sources`, workflow `states`, `transitions`, `commands`, `policies`) must match:

```regex
^[A-Za-z0-9][A-Za-z0-9_.-]*$
```

Secret references must match:

```regex
^[A-Za-z_][A-Za-z0-9_]*$
```

### Supported providers and default capabilities

| Provider type | Default capabilities |
| --- | --- |
| `github_actions`, `depot`, `jenkins` | `observe_runs`, `observe_checks`, `trigger_run`, `retry_run`, `cancel_run`, `fetch_logs`, `reconcile` |
| `github` (source-control / review) | `observe_runs`, `observe_checks`, `fetch_logs`, `reconcile` |
| `linear`, `greptile` | `observe_runs`, `reconcile` |

`capability_restrictions` may only narrow (set `false`). Setting `true` for a capability the provider does not have is rejected. Unknown capability names are rejected.

### Knowledge source rules

- `workspace_files`: `scope: organization`, relative `paths` inside the workspace files.
- `repository_files`: `scope: repository`, relative `paths`, optional `repositories` list (defaults to relevant workspace repos). Each repository name must exist in `repositories`.
- `retrieval`: `scope: organization | repository`, required `connection` pointing to a `knowledge.connections` entry, plus `query` / `parameters`.
- Paths must be non-empty, relative, and not contain `..`.

## Workflow v2

Top-level keys are strict. Unknown fields are rejected.

```yaml
schema_version: 2
name: <required>
enabled: true | false        # default false if omitted; required for automatic runs
initial_state: <state name>  # required

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

- For the same `from` state + `on` event, `when` clauses must be provably disjoint. The validator rejects overlapping clauses with a witness value.
- The `on` field names an event kind (e.g. `pull_request.verified_open`, `ci.policy.evaluated`).
- Transitions with no `on` are evaluated on state entry.
- `from` may be a single string or a list.

### Commands

Commands are operator-initiated transitions (e.g. `cancel`, `retry`). They are graph edges like transitions but require authenticated invocation.

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
| --- | --- | --- |
| `agent.task` | `prompt` or `instructions`; optional `include_facts: [fact keys, ...]` | None (hub-managed) |
| `ci.trigger` | `pipeline: <pipeline name>` | Connection must have `trigger_run` |
| `ci.retry` | `pipeline: <pipeline name>` | Connection must have `retry_run` |
| `ci.cancel` | `pipeline: <pipeline name>` | Connection must have `cancel_run` |
| `issue.comment` | `connection: <issue tracker connection name>` | Named connection must exist in workspace |
| `exec.run` | `command: <shell command>`; optional `timeout: <duration>` | Workspace `execution` block must have `execute_command` capability |
| `dependency.update` | `ecosystems: [go, npm, ...]`; optional `paths`, `exclude_paths`, `grouping`, `include_major`, `allow`, `ignore`, `timeout` | Workspace `execution` block must have `dependency_update` capability |

`agent.task` `include_facts` must be 1–20 unique non-empty fact keys. Conversation/transcript facts are forbidden.

`exec.run` and `dependency.update` are deterministic, bridge-executed effects. Their results are written to the protected `exec.*` fact namespace; the workflow reads them but cannot write them. Both effects are manual-retry-only and never auto-retried.

### Fact namespaces

Workflows may write only `work.*` or `custom.*` in `assert`, `set`, and event clause `assert`/`set`.

Protected namespaces (read-only, written by hub adapters):

- `ci.*`
- `pull_request.*`
- `review.*`
- `effects.*`
- `workflow.*`
- `operator.*`
- `exec.*`

Example:

```yaml
assert:
  work.needs_fix: true
set:
  custom.investigation_id: "abc-123"
```

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
- Workflows cannot declare a repository list under `delivery`. Repository authority comes from the workspace.

## Converting from v1 to v2

Use the CLI conversion path:

```bash
# Workspace
elasticclaw workspace convert .elasticclaw/workspaces/my-ws
elasticclaw workspace convert ./elasticclaw-config.yaml --to 2 -o v2.yaml

# Workflow
elasticclaw workflow convert examples/workflows/github-issue.yaml
elasticclaw workflow convert ./wf.yaml --workspace ./ws -o wf.v2.yaml
```

Key conversion rules:

- `provider` / `nix` / `docker` move to `execution`.
- `repositories` list becomes a named map; one placeholder `source_control` connection is created.
- `env.*.secret` and top-level `secrets` become `credentials.*`.
- Inline `env.*` values are dropped.
- `webhook_secrets` are not represented yet.
- v1 `stages` become v2 `states`.
- `entry: true` becomes `initial_state`.
- `on_enter.inject` becomes an `agent.task` effect.
- `on_enter.add_labels` / `remove_labels` are not auto-converted.
- `on_enter.run` (shell/CI hooks) are not auto-converted; model as an `exec.run` effect after review and read the results from `exec.last_run.*` facts.
- `on_enter.dependency_updates` are not auto-converted; model as a `dependency.update` effect after review and read the results from `exec.dependency_update.*` facts.
- v1 triggers (`pr_merged`, `pr_closed`, `pr_opened`) map to v2 transition events.
- `message_contains`, `message_matches`, `[DONE]`, `[READY_TO_COMMIT]` are never trusted in v2.
- The converted workflow is always `enabled: false` (a draft).
- v1-only fields (`integration`, `trigger`, `inputs`, `volumes`, `concurrency_group`) are not represented in v2 and are reported as warnings.

Always review the conversion warnings and pair-validate the workspace + workflow before enabling.

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
