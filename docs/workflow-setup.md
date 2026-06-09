# Workflow Setup

ElasticClaw's primary setup path is workspace plus workflow pattern. A workspace defines the repositories, files, provider, model defaults, environment, and declared secrets. A workflow defines the trigger, access expectations, lifecycle stages, manual inputs, and readiness checks for one repeatable workstream.

Use this path for new automation:

1. Create or choose a workspace under `.elasticclaw/workspaces/<workspace>/`.
2. Render a workflow pattern into `.elasticclaw/workspaces/<workspace>/workflows/<workflow>.yaml`.
3. Validate the workflow locally or through the UI.
4. Save or push the workflow when critical checks are clear.

Built-in workflow patterns:

- `github-issue`: start from GitHub issue events, usually labels, with optional manual issue-number runs.
- `linear-status`: start from Linear status changes and move the issue through PR lifecycle states.
- `shortcut-status`: start from Shortcut story state changes and move the story through PR lifecycle states.
- `manual-task`: start only from a manual trigger with configured inputs.

Example workflow YAML files live in `examples/workflows/`.

## CLI

Render a pattern to local YAML with `workflow create`:

```bash
elasticclaw workflow create github-issue \
  --workspace engineering \
  --repo elasticclaw/elasticclaw \
  --name issue-triage \
  --label agent-ready \
  --create-workspace
```

For Linear and Shortcut patterns, pass the issue tracker workspace:

```bash
elasticclaw workflow create linear-status \
  --workspace engineering \
  --integration-workspace product \
  --name linear-agent

elasticclaw workflow create shortcut-status \
  --workspace engineering \
  --integration-workspace product \
  --name shortcut-agent
```

Validate a workflow before saving or pushing:

```bash
elasticclaw workflow validate \
  --workspace engineering \
  .elasticclaw/workspaces/engineering/workflows/issue-triage.yaml
```

`workflow validate` checks YAML shape, workflow schema, trigger source count, manual inputs, stages, pipeline triggers, and compatibility such as `move_issue` only being used by issue tracker workflows. It returns a non-zero exit code when any critical diagnostic is present.

Use `workflow setup` when you want one command to render and validate:

```bash
elasticclaw workflow setup github-issue \
  --workspace engineering \
  --repo elasticclaw/elasticclaw \
  --name issue-triage \
  --label agent-ready \
  --create-workspace
```

`workflow setup` writes local YAML and validates it against the local workspace config. It does not collect or write hub secrets.

After review, push workflow definitions with the existing workflow push path:

```bash
elasticclaw workflow push \
  --workspace engineering \
  .elasticclaw/workspaces/engineering/workflows/issue-triage.yaml
```

## UI Wizard

The web UI workflow wizard uses the same setup API and walks through five steps:

- `Pattern`: choose `github-issue`, `linear-status`, `shortcut-status`, or `manual-task`.
- `Access`: review workspace repositories, provider/model readiness, issue tracker connections, GitHub App availability, declared secrets, and concurrency groups.
- `Trigger`: configure the issue source, labels or status, optional manual trigger, and manual inputs.
- `Lifecycle`: configure working, PR opened, merged, and closed-without-merge actions such as labels, issue status moves, done signal, and optional pre-commit command.
- `Review`: inspect rendered YAML and diagnostics before saving.

`Save with warnings` preserves the authored workflow YAML when warnings remain. Warnings call out configuration that may need attention, but they do not block saving. Critical diagnostics block a ready workflow because the workflow cannot be safely run as authored.

`Saved as draft` means the workflow YAML was preserved but cannot be considered runnable yet. This is expected when warnings remain, prerequisites are missing, or the workflow is intentionally saved disabled while the team finishes setup.

`Ready to run` means the rendered workflow has no critical diagnostics and the readiness checks required for execution are satisfied. From that state, users can save the workflow and use the manual trigger when `enable_manual_trigger` is configured.

Manual trigger runs use the workflow's `inputs` list. A GitHub issue workflow that enables manual runs must include a required numeric `issue_number` input with `min: 1`.

## Warnings And Critical Diagnostics

Diagnostics are grouped by severity:

- `critical`: blocks ready state and should block rollout. Examples include invalid YAML, unknown workflow fields, no trigger source, unsupported pipeline triggers, missing GitHub issue `issue_number` input for manual runs, or incompatible `move_issue` actions.
- `warning`: allows save, but requires review before rollout. Examples include trigger overlap, missing optional lifecycle actions, or setup choices that may run but are incomplete.
- `info`: documents context or non-blocking readiness details.

Treat critical diagnostics as release blockers. Treat warnings as review items: they may be acceptable for a draft, but should be explicitly accepted or resolved before enabling a production workflow.

## Legacy Factory Conversion

Legacy factory support remains available for existing installations, but it is not the primary setup path for new users. Conversion is preview-first and conservative.

Convert a local legacy factory to workflow YAML:

```bash
elasticclaw factory convert legacy-bugfix \
  --workspace engineering \
  --output .elasticclaw/workspaces/engineering/workflows/legacy-bugfix.yaml
```

Preview conversion through the setup API:

```bash
curl -X POST \
  -H "Authorization: Bearer $ELASTICCLAW_TOKEN" \
  -H "Content-Type: application/json" \
  "$ELASTICCLAW_URL/api/workflow-setup/factories/legacy-bugfix/convert-preview" \
  -d '{"workspace":"engineering"}'
```

Supported legacy integrations in this release:

- `github-issues`
- `linear`
- `shortcut`

Refused cases:

- GitHub pull request factories.
- `github` factories that are not GitHub Issues.
- `external` webhook factories.
- Any integration outside `github-issues`, `linear`, and `shortcut`.
- Conversion where template files, workspace files, provider, secrets, trigger fields, or pipeline stages cannot be reconciled with conservative parity.

Conversion writes the new workflow disabled by default when it succeeds. It does not delete, disable, or mutate the legacy factory automatically. Operators should validate the converted workflow, compare lifecycle behavior, and only then decide whether to stop using the old configuration.

## Rollout Checklist

Phase 0, local authoring:

- Choose the workspace and workflow pattern.
- Render with `elasticclaw workflow create` or the UI wizard.
- Review the generated YAML in `.elasticclaw/workspaces/<workspace>/workflows/`.
- Confirm no real secrets are present in YAML.

Phase 1, validation:

- Run `elasticclaw workflow validate --workspace <workspace> <workflow.yaml>`.
- Resolve every critical diagnostic.
- Review warnings and record accepted warnings.
- Run example parsing tests when examples changed: `go test ./pkg/workflowsetup -run TestWorkflowExamples -count=1`.

Phase 2, backend gate:

- Run `make test`.
- Confirm workflow setup, factory conversion, workspace parsing, and trigger overlap tests pass.
- Do not proceed with critical diagnostics or failing backend tests.

Phase 3, frontend gate:

- Run `cd web && npm run lint`.
- If the repository has an acknowledged lint baseline, record the baseline separately from new workflow setup warnings.
- Verify the wizard labels `Saved as draft` and `Ready to run` match readiness state.

Phase 4, controlled rollout:

- Save or push the workflow disabled or as a draft first when prerequisites are still being completed.
- Enable one workflow in one workspace.
- Start with manual trigger if available, then allow issue tracker triggers.
- Watch run creation, lifecycle stage transitions, PR handoff, and cleanup.
- Keep the legacy factory untouched until the workflow has proven equivalent in production.

Phase 5, broader rollout:

- Repeat validation for each workspace and pattern.
- Re-run backend and frontend gates before release.
- Document accepted warnings and any baseline lint exceptions.
- Remove or retire old operational references only after production workflows are stable.
