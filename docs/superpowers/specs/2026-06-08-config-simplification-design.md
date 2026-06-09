# Config Simplification Design

## Goal

Make new ElasticClaw deployments and new workspace workflows easy to configure without breaking existing factories, templates, CLI commands, stored external files, or runtime behavior.

The new experience must not expose "factory" as the primary user-facing concept. New setup copy uses "workflow", "workspace", and "workflow pattern". "Legacy factory" appears only in migration and compatibility surfaces.

Implementation uses the current v1 model:

- A workspace defines runtime access, files, repositories, env, secrets, provider, and model defaults.
- A workflow defines triggers, manual inputs, lifecycle stages, issue tracker actions, labels, preflight checks, and concurrency.
- Legacy factories remain supported and migratable where parity can be proven, but new product surfaces guide users to workspaces and workflows.

## Background

The codebase already moved toward workspaces and workflows:

- `elasticclaw factory` is hidden and deprecated in favor of workflows.
- `elasticclaw template` and `elasticclaw create` are hidden and deprecated in favor of workspace push and workflow trigger.
- Workspaces are stored externally under the hub config directory, with workflows stored beside each workspace.
- Runtime workflow creation already resolves provider, model, secrets, workspace files, repository access, tags, concurrency, and async provisioning.

The main issue is not missing capability. The issue is fragmented setup:

- Hub-level config, workspace config, workflow YAML, workspace-managed secrets, integrations, GitHub Apps, webhooks, and manual trigger inputs are configured in separate places.
- CLI can push workspaces and workflows, but it cannot create workflow YAML from guided choices.
- UI can view and toggle workflows, but it cannot create a complete workflow.
- Doctor validates many legacy factory concerns, but workflow-specific readiness checks are incomplete.
- Runtime failures still catch mistakes that could be detected before saving or triggering a workflow.

## Non-Goals

- Do not remove legacy factories.
- Do not change the persisted v1 workflow schema.
- Do not require users to use the UI; CLI-first and GitOps-style flows remain supported.
- Do not introduce a new database source of truth for workspace/workflow definitions.
- Do not add a generic visual workflow builder in this phase.
- Do not require network calls to third-party services for basic validation. Network probes are optional checks.

## User Outcomes

### New Deployment

A user who installed the hub can create the first usable automation without hand-writing all config:

1. Configure provider and model.
2. Create a workspace through workspace setup, or select an existing workspace.
3. Add repositories and required secrets.
4. Select issue source or manual workflow.
5. Generate a workflow from a known pattern.
6. Validate readiness.
7. Save and optionally trigger a test run.

### Existing Workspace

A user with a configured workspace can add a new workflow without touching unrelated hub config:

1. Select the existing workspace.
2. Select a workflow template.
3. Fill trigger details and lifecycle choices.
4. Validate against existing workspace access, secrets, providers, and integrations.
5. Save the workflow under the workspace.

### Legacy Factory

A user with existing factories can continue running them unchanged, then migrate one at a time:

1. List legacy factories.
2. Convert a factory into a workspace workflow.
3. Validate generated YAML.
4. Save as a disabled workflow draft after validation.
5. Enable the workflow when ready.

## Design Principles

- Keep v1 workflow YAML as the source of truth.
- Generate plain files that users can review and commit.
- Validate before write when possible, and validate again before runtime.
- Use small named workflow patterns instead of asking users to design stages from scratch.
- Preserve existing commands and endpoints.
- Prefer shared backend validation over duplicating logic in CLI and UI.
- Make generated YAML boring, explicit, and easy to edit.
- Make failures actionable with exact missing fields, paths, and settings page targets.

## Recommended Approach

Build a workflow setup experience in four layers:

1. Shared backend package for workflow patterns, rendering, and validation.
2. CLI commands for create, validate, and migrate.
3. Hub API endpoints for generate, validate, save, and migrate preview.
4. UI wizard that calls the same API and stores the same YAML.

This keeps runtime behavior stable while making setup dramatically easier.

## Workflow Patterns

Add built-in workflow patterns. Each pattern renders v1 workflow YAML and is safe to edit afterward outside the UI wizard.

Pattern metadata is served by the backend. The frontend must consume pattern metadata and defaults from the backend instead of duplicating template defaults in TypeScript.

### `github-issue`

For GitHub Issues label/status driven work.

Required inputs:

- Workspace name.
- Workflow name.
- Repository, exactly `owner/repo`.
- Trigger event: default `issue_labeled`.
- Trigger label: default `agent-ready`.
- Issue state: default `open`.
- Labelers: default `*`.
- Manual trigger enabled: default true.
- Concurrency group: default `global`.

When manual trigger is enabled, generated YAML must include:

```yaml
inputs:
  - name: issue_number
    type: number
    required: true
    min: 1
    description: GitHub issue number to run this workflow against.
```

This is required because the existing manual GitHub issue workflow endpoint requires `issue_number` and exactly one exact trigger repository.

Generated lifecycle:

- `working`: entry stage; remove trigger label, add working label, inject issue context.
- `pre_commit`: optional run command; default disabled unless user selects preflight.
- `pr_opened`: triggered by `[DONE]`; add review label and remove working label.
- `merged`: triggered by `pr_merged`; add done label and terminal.
- `closed_no_merge`: triggered by `pr_closed`; add fallback label, inject instruction, terminal.

### `linear-status`

For Linear issue status driven work.

Required inputs:

- Workspace name.
- Workflow name.
- Linear workspace connection name.
- Team key optional.
- Trigger status.
- Working status optional.
- Done status optional.
- Canceled status optional.
- Concurrency group.

Generated lifecycle:

- `working`: entry stage; optionally move issue to working status; inject issue context.
- `pr_opened`: triggered by `[DONE]`; inject PR watch guidance.
- `merged`: triggered by `pr_merged`; optionally move issue to done status; terminal.
- `closed_no_merge`: triggered by `pr_closed`; optionally move issue to canceled status; terminal.

### `shortcut-status`

For Shortcut story workflow state driven work.

Required inputs:

- Workspace name.
- Workflow name.
- Shortcut workspace connection name.
- Trigger state.
- Working state optional.
- Done state optional.
- Canceled state optional.
- Concurrency group.

Generated lifecycle mirrors `linear-status`, using Shortcut move issue behavior.

### `manual-task`

For manual workflows that are not tied to an external issue source.

Required inputs:

- Workspace name.
- Workflow name.
- Input fields.
- Optional name pattern.
- Concurrency group.

Generated lifecycle:

- `working`: entry stage; inject manual trigger context.
- `pre_commit`: optional run command.
- `complete`: triggered by `[DONE]`; terminal.

## Workspace Defaults

`elasticclaw workspace create` remains available, and the CLI setup flow may scaffold a new workspace when explicitly requested.

The first UI release is scoped to existing workspaces. If no workspace exists, Settings should show a primary empty-state action that sends the user to workspace setup instructions or a separate workspace setup route. The workflow wizard must not create or update an existing workspace inline unless a separate create-only workspace transaction is specified.

Generated workspace config should remain close to the existing schema:

```yaml
schema_version: v1
name: example
provider: replicated
repositories:
  - repo: owner/repo
    permissions: write
env: {}
```

The wizard must not require a new workspace when an existing workspace is suitable. Adding a workflow to an existing workspace is the common path after initial setup.

If a future UI workspace creation path is added, it must be create-only. It must not call existing workspace save/update behavior for an already existing workspace because `saveExternalWorkspace` removes authored workspace files before rewriting from the submitted file set.

## Validation Model

Create a shared validation service that returns structured checks, not just a single error string.

Validation must evaluate the same effective inputs the runtime will use:

- Parse `elasticclaw-config.yaml` as `types.WorkspaceConfig` for name, repositories, env names, secrets, and webhook secret names.
- Parse the same raw `elasticclaw-config.yaml` as `types.TemplateConfig` through `config.ParseTemplateConfig` for provider, default model, LLM key, env secret refs, instance type, nix, docker, and runtime defaults.
- Keep the raw workspace config in `workspace.Files["elasticclaw-config.yaml"]` for any create simulation.
- Use provider precedence exactly as runtime does: `workflow.provider > workspace runtime provider > hub default provider`.
- Validate the normalized workflow clone and parse the effective `PipelineYAML` that runtime would execute.

### Request Shape

```json
{
  "workspace": {
    "name": "engineering",
    "config": "schema_version: v1\n..."
  },
  "workflow": {
    "name": "github-issue",
    "config": "schema_version: v1\n..."
  },
  "mode": "create",
  "options": {
    "networkChecks": false
  }
}
```

For existing workspace validation, the backend can load the workspace from external storage when `workspace.config` is omitted.

### Response Shape

```json
{
  "ok": false,
  "summary": {
    "critical": 1,
    "warning": 2,
    "info": 3
  },
  "checks": [
    {
      "id": "provider.configured",
      "category": "runtime",
      "severity": "critical",
      "ok": false,
      "blocking": true,
      "step": "runtime",
      "fieldPath": "workspace.provider",
      "title": "Provider daytona is not configured",
      "detail": "The workspace resolves provider daytona, but hub providers has no daytona entry.",
      "fixTarget": "/settings/runtimes",
      "fixLabel": "Configure runtime provider",
      "retryable": true
    }
  ]
}
```

Each render and validate response includes a `configHash` for the rendered or validated YAML. Save is enabled only when the latest validation hash matches the current YAML and no render or validate request is pending. The UI must ignore stale async render/validate responses.

### Validation Checks

Core checks:

- Workspace YAML parses.
- Workspace name is valid and path-safe.
- Workspace repository entries are valid `owner/repo`.
- Workflow YAML parses.
- Workflow normalizes to one supported integration.
- Workflow name is valid and path-safe.
- Workflow has exactly one trigger source unless it is manual-only.
- Workflow stages parse through the pipeline parser.
- Workflow has exactly one entry stage.
- Stage IDs are unique.
- Trigger references are syntactically valid.
- `gate_result.stage` references an existing gate stage.
- `output_matches.output` references a known `run.output` or `judge.output`.
- `move_issue` is only accepted for integrations that can move issues.
- Manual trigger inputs validate by type, enum, regex, min, and max.
- Names must use the setup slug grammar `^[a-z0-9][a-z0-9_-]{0,62}$`; generation lowercases names; save blocks case-insensitive collisions.
- GitHub Issue workflows with `enable_manual_trigger: true` include required `issue_number` input.

Hub readiness checks:

- `claw_token` exists.
- At least one provider exists.
- Resolved provider exists in `hubCfg.Providers`.
- Resolved provider is configured, has required credentials, and is provisionable by workflow runtime.
- At least one LLM key exists.
- Resolved model is non-empty.
- Referenced concurrency group exists or uses `global`.
- All secret refs resolve from workspace-managed secrets or hub secrets: workspace `env.*.secret`, workspace `secret_refs`, workflow `secret_refs`, provider credentials, and LLM credentials.
- Required issue tracker connection exists for Linear, Shortcut, or GitHub Issues.
- Webhook signing secret exists for automatic issue-source workflows unless the user explicitly chooses manual-only. Missing webhook verification is a critical readiness failure.
- GitHub issue manual workflow has exactly one exact repository, not only an org wildcard.
- Shortcut automatic workflows must either use a runtime path that supports workspace-managed Shortcut trackers or report a critical readiness failure when only workspace-managed Shortcut configuration exists.

Provisionable provider matrix:

- `replicated`, `daytona`, and `exedev` are provisionable by workflow runtime.
- `noop` is test-only and only valid behind `ELASTICCLAW_NOOP_PROVIDER`.
- `docker` is accepted by config schema but must not pass workflow readiness until workflow runtime supports it.

Conflict checks:

- Workflow name does not collide case-insensitively in the same workspace.
- Workflow trigger does not overlap another enabled workflow in the same workspace when disjointness can be proven.
- Legacy factory trigger overlap is reported as a warning, not a blocker.
- Workspace push will not remove managed secrets, GitHub Apps, or issue tracker managed files.

Trigger overlap checks must use normalized effective matchers shared with runtime. If disjointness cannot be proven, report a warning rather than a critical failure. GitHub Issues overlap must account for event, repo selector intersection, states, labels, labelers, and `assigned_to`.

Optional network checks:

- Provider credential can authenticate.
- GitHub token/App can access selected repo.
- Issue tracker token can read the selected workspace/team.
- Webhook URL uses public URL when automatic trigger is enabled.

## Backend Architecture

### New Package

Create `pkg/workflowsetup`.

Responsibilities:

- Define workflow setup requests and responses.
- Render built-in workflow patterns.
- Validate workspace/workflow config without mutating disk.
- Convert legacy factory config into workflow YAML.
- Produce structured diagnostics that CLI, API, UI, and doctor can share.

The package should depend on `pkg/types` and `pkg/config`. It should not depend on `*hub.Server` or any package under `pkg/hub`.

Before shared validation imports the pipeline parser, move `pkg/hub/pipeline` to a neutral package such as `pkg/workflow/pipeline` or `pkg/pipeline`. `pkg/workflowsetup`, `pkg/hub`, CLI, and doctor may import the neutral parser.

### Suggested Interfaces

```go
type Environment interface {
    Snapshot() SetupEnvironmentSnapshot
    LoadWorkspace(name string) (*types.WorkspaceConfig, error)
    LoadWorkflowRaw(workspace, name string) (string, error)
    WorkspaceSecretNames(name string) ([]string, error)
    WorkspaceIssueTrackers(name string) ([]IssueTrackerRef, error)
    WorkspaceGitHubApps(name string) ([]GitHubAppRef, error)
    ListFactories() ([]FactoryRef, error)
    LoadFactory(name string) (*types.FactoryConfig, string, error)
}
```

The hub package implements this interface using existing external storage and managed workspace helpers.

`Snapshot()` returns a copied, sanitized snapshot built under the hub lock. It must include only:

- `clawTokenSet`.
- Provider names, provider type, provisionable status, and credential presence booleans.
- Default provider/model and LLM key metadata without API keys.
- Concurrency groups.
- Hub secret names only, never values.
- Issue tracker and GitHub App presence metadata without tokens or private keys.

`workflowsetup` must never receive a mutable `*types.HubConfig`, raw secret values, API keys, provider tokens, or private keys.

### API Endpoints

Add these endpoints:

- `GET /api/workflow-setup/patterns`
- `GET /api/workflow-setup/workspaces/{workspace}/context`
- `POST /api/workflow-setup/render`
- `POST /api/workflow-setup/validate`
- `POST /api/workflow-setup/save`
- `POST /api/workflow-setup/factories/{factory}/convert-preview`

This namespace avoids reserving workflow names such as `save` or `validate`.

All workflow setup endpoints are configuration endpoints. They require web admin auth or an explicit future configuration-write scope. Responses never include raw secrets, tokens, API keys, provider credentials, or private keys.

Pattern list response must include TypeScript-aligned metadata for:

- Pattern ID.
- Human label.
- Description.
- Required fields.
- Advanced fields.
- Defaults.
- Validation field paths.

Setup context response returns masked data required by the wizard:

- Workspace repositories and env names.
- Workspace and hub secret names.
- Webhook secret names.
- Issue tracker refs.
- GitHub App refs.
- Provider/model readiness metadata.
- Concurrency groups.

Render response returns `{workflowName, config, configHash, warnings}`.

Validate response returns `{ok, configHash, summary, checks}`.

Save request is:

```json
{
  "workspace": "engineering",
  "workflow": {
    "name": "github-issue",
    "config": "schema_version: v1\n..."
  },
  "mode": "create",
  "validatedConfigHash": "sha256:...",
  "allowWarnings": false
}
```

Save must re-run validation. Critical failures always block save. Warnings allow save only after explicit `allowWarnings: true`. Network checks default off and render as `not_checked`, not passed.

Guided save must persist the authored workflow YAML bytes after validation. It must not persist a normalized or marshaled `WorkflowConfig` unless the request explicitly asks for canonicalization. Derived runtime fields such as `integration`, `trigger_status`, `labels`, and `pipeline_yaml` must not be added to authored YAML unless they were present in the input.

### Existing Endpoint Compatibility

Keep the existing `POST /api/workspaces/{workspace}/workflows` behavior. The new save endpoint is a guided wrapper that validates and then delegates to existing save logic.

Do not change existing `POST /api/factories`, `GET /api/factories`, or factory runtime behavior.

## CLI Design

### `elasticclaw workflow create`

Create workflow YAML locally.

Example:

```bash
elasticclaw workflow create github-issue \
  --workspace elasticclaw \
  --name github-issue \
  --repo elasticclaw/elasticclaw \
  --label agent-ready \
  --manual \
  --output .elasticclaw/workspaces/elasticclaw/workflows/github-issue.yaml
```

Default output path:

```text
.elasticclaw/workspaces/<workspace>/workflows/<workflow>.yaml
```

If that workspace directory does not exist locally, the command should offer a clear error with the exact `workspace create` command. A non-interactive flag `--create-workspace` can scaffold the workspace first.

The command must use the same pattern definitions as `pkg/workflowsetup`. It should not maintain separate YAML templates that can drift from API/UI output.

### `elasticclaw workflow validate`

Validate a local workflow against a local or remote workspace.

Examples:

```bash
elasticclaw workflow validate --workspace elasticclaw .elasticclaw/workspaces/elasticclaw/workflows/github-issue.yaml
elasticclaw workflow validate --workspace elasticclaw --remote github-issue
```

Output:

- Human-readable checklist by default.
- JSON output under existing `--json`.
- Exit code `0` when no critical checks fail.
- Exit code `1` when any critical check fails.

The JSON shape should match the API validation response so CI jobs and UI tests can reuse the same fixtures.

### `elasticclaw workflow setup`

Guided non-UI flow.

Behavior:

- Prompts for pattern and required fields.
- Generates YAML.
- Runs validation.
- Writes local file.
- Optionally pushes.

This command should be conservative. It should not write hub config secrets from terminal prompts in the first release.

### `elasticclaw factory convert`

Convert legacy factory to workflow YAML.

Example:

```bash
elasticclaw factory convert legacy-bugfix --workspace engineering --output .elasticclaw/workspaces/engineering/workflows/legacy-bugfix.yaml
```

Behavior:

- Reads external factory config from hub or local `.elasticclaw/factories`.
- Converts only supported factory integrations in the first implementation: GitHub Issues, Linear, and Shortcut.
- Reports unsupported GitHub Pull Request factories, external issue sources, or ambiguous factories as preview-only with a critical diagnostic.
- Converts supported fields to workflow v1.
- Converts `pipeline.yaml` to `stages` only after validating it through the neutral pipeline parser.
- Validates that the factory runtime template will remain available to the converted workflow. If the legacy factory relied on `factory.Template`, conversion must either copy the resolved template files into the target workspace or refuse conversion with a critical diagnostic.
- Writes generated workflow disabled by default unless `--enabled` is provided.
- Does not delete or disable the legacy factory.

Factory conversion is never a blind copy. The tool must prove that trigger semantics, provider/runtime defaults, issue source, template files, and pipeline stages are preserved. When parity cannot be proven, it returns a draft preview and explains which manual step is required.

## UI Design

Add a "New Workflow" action in the Workflows header inside workspace settings.

The first UI release should be a full settings subroute or full-height sheet with a sticky footer, not a small modal. It must remain usable on mobile, preserve drafts while users leave for settings fixes, and avoid any inline workflow graph editing.

### Flow

1. Pattern
   - Choose where work starts: GitHub Issue, Linear Status, Shortcut Status, or Manual Task.
   - Pattern cards show concise labels and backend-provided descriptions.
2. Workspace
   - Existing workspace is selected from the current workspace context when launched inside a workspace.
   - When launched from a workspace settings route, the current workspace is pinned and this step becomes a compact confirmation, with "Change workspace" as a secondary action.
   - If no workspace exists, show an empty-state action to workspace setup instead of opening the workflow wizard.
   - No first-release inline create/edit path for workspaces.
3. Access
   - Show only resources required by the selected pattern.
   - Group resources as available, missing, and not used.
   - Missing resources link to the correct settings page and preserve the wizard draft.
4. Trigger
   - Pattern-specific fields.
   - Validation starts after pattern and workspace are selected, then refreshes after each field change.
5. Lifecycle
   - Labels/statuses and optional preflight command.
   - Advanced defaults are collapsed by default.
6. Review
   - Show generated YAML as read-only preview in the first UI release.
   - Offer copy YAML, not inline raw YAML editing.
   - Show the latest validation timestamp/hash.
   - Save disabled until critical failures are resolved.
7. Finish
   - Distinguish `Saved as draft` from `Ready to run`.
   - Show optional manual trigger form only when trigger prerequisites are satisfied.
   - Show test-run success only after the trigger endpoint returns success.
   - For GitHub Issue manual workflows, the trigger form must include required `issue_number`.

### UI Constraints

- The wizard does not edit raw hub secrets inline except through existing secrets pages.
- The wizard links to settings pages for missing provider/model/integration/secrets.
- The generated YAML is visible before save.
- Users can copy YAML and manage it outside the UI.
- Existing workflow list remains simple: enabled, manual trigger, integration, trigger summary, source.
- The Save button is disabled while render or validation is pending.
- The Save button is disabled when the current YAML hash does not match the latest successful validation hash.
- Warnings require an explicit "Save with warnings" confirmation.
- Critical diagnostics move focus to the first invalid field when possible and use `aria-invalid` plus a live validation region.
- Network checks default to `not_checked`; the UI must not display them as passed.
- Draft state survives navigation to settings fixes and return to the wizard.

### Empty States

Required empty states:

- No workspace: primary action to workspace setup.
- No workflow in selected workspace: primary action `New Workflow`.
- Missing required issue source: action to connect issue tracker or GitHub App.
- Missing required secret: action to add the named secret.
- Missing provider or model: action to runtime settings.

Every settings fix action should preserve the wizard draft and return the user to the same step when the missing item is resolved.

## Data Flow

### Create New Workflow From UI

1. Browser fetches patterns.
2. Browser fetches setup context for the selected workspace.
3. Browser posts selected pattern and fields to render endpoint.
4. Hub renders YAML through `pkg/workflowsetup`.
5. Browser posts YAML to validate endpoint.
6. Hub validates against the sanitized environment snapshot and workspace external storage.
7. Browser ignores stale render or validate responses whose hash no longer matches current form state.
8. Browser posts save request with `validatedConfigHash`.
9. Hub validates again and persists workflow under workspace external storage.
10. Browser refreshes workspace workflows.

Frontend API helpers should be added to `web/lib/api.ts` with TypeScript types that mirror backend request and response structs. The frontend must not infer hidden defaults that are not returned by the pattern metadata or render response.

### Create New Workflow From CLI

1. CLI renders local YAML from built-in pattern.
2. CLI validates locally where possible.
3. CLI calls hub validation for hub readiness checks.
4. CLI writes local file.
5. User pushes through existing `workflow push`, or `workflow setup --push` pushes for them.

### Manual Trigger

Manual trigger continues using existing endpoint:

```text
POST /api/workspaces/{workspace}/workflows/{workflow}/trigger
```

The setup flow only ensures the workflow has valid `inputs` and `enable_manual_trigger`.

For GitHub Issue workflows, setup also verifies that manual trigger can collect `issue_number` and that exactly one exact repository is configured. Finish must not claim a successful test run until the existing trigger endpoint returns success.

## Compatibility Plan

### Runtime

No runtime behavior change is required for existing factories or workflows.

### Persistence

Keep existing external storage:

- Workspaces under `<hub-config-dir>/workspaces/<workspace>`.
- Workflows under `<hub-config-dir>/workspaces/<workspace>/workflows`.
- Workspace-managed secrets and integrations under `.elasticclaw-managed`.
- Legacy factories under `<hub-config-dir>/factories`.

### Legacy Factory Support

Existing factories continue to:

- Load from external factory storage.
- Run through current pollers and webhooks.
- Resolve templates.
- Use current pipeline YAML.

The new surfaces should not advertise factory creation as the main path. They should provide conversion and compatibility messaging only.

Factory conversion must account for the runtime substrate difference:

- Legacy factory runtime resolves template files from `factory.Template`.
- Workflow runtime uses workspace files, especially `workspace.Files`.
- Conversion must copy or require equivalent workspace files before a converted workflow can be considered ready.
- Converted workflows are disabled by default and should be saved as drafts until validation confirms runtime parity.

### Existing Workflows

Existing workflow YAML remains valid. New validation can report warnings, but should not block existing workflows unless the user explicitly saves through the new guided endpoint.

## Error Handling

Validation diagnostics must include:

- Stable ID.
- Category.
- Severity.
- Blocking boolean.
- Step.
- Field path.
- Human title.
- Specific detail.
- Fix target when available.
- Fix label when available.
- Retryable boolean.
- Status for optional checks, including `not_checked`.

Examples:

- `workspace.repo.invalid`
- `workflow.trigger.missing`
- `workflow.stage.entry.none`
- `workflow.stage.duplicate_id`
- `workflow.gate.unknown_stage`
- `runtime.provider.missing`
- `runtime.llm.no_key`
- `secret.workspace.missing`
- `integration.github_issues.no_token`
- `conflict.trigger.overlap`

The UI should group checks by category and show critical checks first.

Diagnostics must be safe to render. They should not echo raw YAML blocks, injected prompt content, secret values, tokens, provider credentials, or private keys.

Every critical diagnostic should answer three questions:

- What failed.
- Why it matters for save or runtime.
- Where the user can fix it.

## Testing Strategy

### Unit Tests

Add unit coverage for `pkg/workflowsetup`:

- Pattern rendering for each built-in pattern.
- YAML parse and normalization.
- Effective `PipelineYAML` parse after normalization.
- Entry stage validation.
- Duplicate stage ID validation.
- Trigger overlap detection.
- Secret reference validation.
- Provider and LLM readiness validation.
- Provider provisionable matrix, including `docker` not passing readiness.
- Workspace config parsed as both `WorkspaceConfig` and `TemplateConfig`.
- Sanitized environment snapshot never contains secret values.
- Raw authored workflow YAML is preserved on guided save.
- Legacy factory conversion with template runtime parity.
- Legacy factory conversion refusal when parity cannot be proven.

### API Tests

Add hub tests for:

- Pattern list endpoint.
- Setup context endpoint masks all sensitive values.
- Render endpoint.
- Validate endpoint with critical failures.
- Validate endpoint with warnings only.
- Render and validate responses include stable hashes for the returned YAML.
- Save endpoint blocks critical failures.
- Save endpoint requires `allowWarnings` for warnings.
- Save endpoint rejects stale `validatedConfigHash`.
- Save endpoint persists authored YAML without derived runtime fields.
- Save endpoint preserves existing workspace-managed files.
- Convert preview does not mutate legacy factory.
- Convert preview reports unsupported integrations as critical diagnostics.

### CLI Tests

Add CLI tests for:

- `workflow create github-issue` writes expected YAML.
- `workflow validate` returns non-zero for critical failure.
- JSON validation output.
- `factory convert` produces disabled workflow by default.
- `factory convert` refuses unsupported integrations unless preview-only output is requested.

### UI Tests

Add focused component tests where existing tooling supports it:

- Wizard renders pattern choices.
- No-workspace empty state shows workspace setup action.
- Existing workspace launch preselects and pins that workspace.
- Missing provider check disables save.
- Missing webhook signing secret blocks automatic workflows.
- Generated YAML preview updates when trigger fields change.
- Stale validation responses do not enable save.
- Warning-only validation requires explicit confirmation.
- Backend errors render actionable diagnostics.
- Settings fix navigation preserves and resumes draft state.
- GitHub Issue manual trigger form requires `issue_number`.
- Save calls validate before persist.
- Keyboard navigation, focus management, live validation region, and mobile layout.

Manual browser verification is required for the wizard after implementation.

## Rollout Plan

### Phase 1: Shared Validation And CLI Generation

Deliver:

- Neutral pipeline parser package that both hub runtime and `pkg/workflowsetup` can import.
- `pkg/workflowsetup`.
- Sanitized setup environment snapshot from hub.
- `workflow create`.
- `workflow validate`.
- Workflow-focused doctor checks.

This phase makes GitOps and CLI users productive quickly.

### Phase 2: API And UI Wizard

Deliver:

- Pattern/render/validate/save endpoints.
- Setup context endpoint.
- New Workflow wizard in settings for existing workspaces.
- No-workspace empty state that sends users to workspace setup.
- Validation diagnostics UI.

This phase makes first-time setup substantially easier.

### Phase 3: Factory Conversion

Deliver:

- `factory convert`.
- Convert preview endpoint.
- Template/workspace parity checks for converted workflows.
- UI conversion action from legacy factory surfaces if still shown.

This phase reduces legacy factory dependence without breaking existing installations.

Factory conversion should ship after the shared validator can prove runtime parity. It should not be bundled into Phase 1 just to expose a command quickly.

## Risks And Mitigations

### Risk: Validation Diverges From Runtime

Mitigation:

- Use shared parsing and normalization functions.
- Validate pipeline with the same neutral pipeline parser used by runtime.
- Keep provider resolution order aligned with `createClawFromWorkflow`.
- Validate workspace config as both persisted workspace schema and runtime template config.

### Risk: Wizard Becomes A Complex Workflow Builder

Mitigation:

- Limit first release to named patterns.
- Always expose YAML preview for advanced edits.
- Do not implement arbitrary stage graph editing.

### Risk: Existing Workflows Start Failing New Checks

Mitigation:

- New checks are advisory for existing workflows unless user saves through guided flow.
- Existing push endpoint keeps current validation behavior.
- Doctor warnings do not stop runtime.

### Risk: Factory Conversion Produces Different Behavior

Mitigation:

- Conversion preview only in first release.
- Generated workflow disabled by default.
- Legacy factory remains untouched.
- Tests compare converted pipeline stages and trigger filters.
- Conversion refuses unsupported integrations and any case where the legacy template cannot be represented in workspace files.

### Risk: Secret Handling Leaks Values

Mitigation:

- Validation only returns secret names and presence booleans.
- UI does not reveal secret values.
- Existing managed secret endpoints remain responsible for writes.
- Setup APIs require web admin auth or an explicit configuration-write scope.

### Risk: Guided Save Overwrites Authored Config

Mitigation:

- Save the authored YAML bytes after validation.
- Do not marshal normalized structs unless canonicalization is explicitly requested.
- Do not create or update an existing workspace inline from the workflow wizard.

### Risk: UI Enables Save From Stale Validation

Mitigation:

- Include `configHash` in render, validate, and save requests.
- Disable save during pending render/validate.
- Revalidate server-side immediately before persist.

## Acceptance Criteria

- A user can generate a GitHub Issues workflow from CLI without hand-writing YAML.
- A user can validate a workflow and receive structured, actionable failures.
- A user can add a workflow to an existing workspace from UI without editing hub YAML.
- The UI has a no-workspace first-run path that does not create or mutate a workspace implicitly.
- The UI can save a warning-only draft only after explicit confirmation.
- The UI cannot save when validation is stale relative to the current YAML.
- Critical runtime blockers are caught before save.
- Existing `factory`, `template`, `workspace`, and `workflow push` behavior remains compatible.
- Legacy factories continue to run unchanged.
- Generated workflows use v1 schema and existing runtime creation path.
- Factory conversion preview does not mutate existing factories.
- Factory conversion either preserves runtime template behavior in workspace files or reports a critical diagnostic.
- Setup API responses never expose raw secret values or credentials.

## Subagent Review Results

This spec was reviewed by four specialized subagents. Their critical findings were incorporated as requirements above:

- Backend developer: API auth boundary, raw YAML save semantics, workspace parsing against runtime behavior, path-safe slug grammar, factory conversion limits, and secret-safe responses.
- Frontend developer: TypeScript-aligned API contract, setup context endpoint, stale validation handling, read-only YAML preview, warning confirmation, accessibility, and mobile verification.
- Software architect: legacy factory template substrate, sanitized immutable environment snapshot, neutral pipeline parser, provider precedence, and non-conflicting endpoint namespace.
- Product designer: first-run empty state, workflow-pattern language instead of factory language, draft preservation, readiness states, access grouping, and clearer success definitions.
