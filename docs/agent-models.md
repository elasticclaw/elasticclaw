# Configure principal and subagent models

In **Settings → Workspaces**, open **Agents** for a workflow. Choose the principal's
credential and model, then the subagents' credential, model, and concurrency limit.
A different credential can select a different provider. Credentials are managed in
Settings; this form stores their names, never their secrets.

When subagents inherit settings, choose **Customize subagents** before editing.
Customization replaces the entire inherited subagent block and starts from the
principal's model and credential with the default concurrency limit.

**Use inherited settings** removes the entire subagent override, including its
concurrency limit, so the workflow/template defaults apply again. **Use main agent**
explicitly selects the principal's model and credential while keeping any authored
concurrency limit. These are separate choices; clearing fields does not remove an
authored subagent override.

The manual run dialog also offers **Override agents for this run**. Changes there
apply only to that run. Saving workflow settings affects future runs; existing
claws retain their resolved model and credential references when queued, restarted,
or restored. Credential secrets can still be refreshed or rotated by the hub.

## YAML and API

Templates and workflows (v1 and v2 workflows) accept these top-level fields:

```yaml
default_model: anthropic/your-main-model
llm_key: coordinator
subagents:
  model: openai/your-worker-model
  llm_key: workers
  max_concurrent: 3
```

Replace the example model IDs with IDs supported by your provider and installed
OpenClaw version. The hub checks credential availability and provider compatibility;
it does not make a paid inference request to verify a model ID.

- No `subagents` block preserves existing inheritance behavior.
- `subagents: {}` explicitly selects inheritance from the principal, replacing a
  template's child configuration when set on a workflow.
- If the child credential is omitted, it inherits the principal's credential.
- Naming the same credential as the principal is equivalent to omitting the child
  credential: an omitted child model inherits the principal's resolved model.
- If a different child credential is selected and the child model is omitted,
  the hub uses that credential's default model.
- `max_concurrent` accepts 1–32. Omit it to use OpenClaw's existing/default limit.

The workflow PATCH endpoint accepts the same fields inside `agents`:

```json
{
  "agents": {
    "llm_key": "coordinator",
    "subagents": {"llm_key": "workers", "max_concurrent": 3}
  }
}
```

PATCH replaces the workflow's authored agent settings. Manual trigger requests
accept an optional `agents` override alongside `inputs`; omitted settings inherit
the workflow/template. Sending a per-run `llm_key` without `default_model` resets
the main model to that credential's default instead of retaining the workflow's
model. Workflow v2 agent edits preserve its states and transitions.

## Runtime support

The first implementation supports different providers within the native OpenClaw
runtime. It restores the selected API/OAuth credentials and configures both models
through the shared bootstrap path for each infrastructure provider.

Mixed model/credential selections involving the Codex runtime are rejected until
cross-runtime delegation is validated. Existing Codex inheritance is preserved.
Two different credentials using the same provider environment variable are also
rejected, because the runtime cannot bind them independently in this configuration.

### Daytona restart compatibility

Daytona now honors a persisted model that matches the selected credential's
provider when restarting a claw. Previously, bootstrap selected the credential's
default model again. A persisted model from a different provider still falls back
to the selected credential's compatible default.

## Timeline

The timeline recognizes native `sessions_spawn` calls and distinguishes requested
models from runtime-resolved models. A successful spawn receipt confirms launch,
not completion of the child's work. It is shown separately from completed
synchronous `Task` calls. Child lifecycle events are not yet forwarded by the
bridge; launch receipts do not establish whether a child is still running.
