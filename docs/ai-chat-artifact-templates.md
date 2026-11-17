# AI Chat artifact templates and workspace design system

Place these optional files beside `<managedDir>/ai_chat.yaml`. No YAML keys are needed:

```text
ai_chat/design-system.html
ai_chat/templates/one-pager.html
ai_chat/templates/status-report.html
```

The hub reads them on each call. Only regular files are accepted, not symlinks. Workspaces without these files keep the existing generation behavior.

## Design system

`design-system.html` is a complete HTML document and a standalone component catalog, up to 200 KB. Include at least one nonempty `<style data-ds>` block. Those blocks are concatenated and injected literally into generated pages, including free mode. Styles without `data-ds` can style the catalog itself and are not injected. HTTPS Google Fonts stylesheet links are also injected.

Use inline SVG or data URIs for brand assets. Add catalog elements such as:

```html
<div class="ds-card" data-ds-component="card" data-ds-usage="Use for grouped content.">
  <h2>Example heading</h2><p>Example content</p>
</div>
```

The model receives a compact guide of CSS classes and up to 40 components (names up to 60 characters, usage trimmed to 300, examples trimmed to 800 bytes, total guide up to 16 KB). It is instructed to use those classes without copying the design system's colors or typography. The hub inserts the stylesheet at the start of the head, after an initial charset declaration when present, so layout CSS can override it. Every published version freezes that design system snapshot and stores its raw-file SHA-256.

## Templates

Each complete HTML file is limited to 50 KB. Its filename without `.html` is the ID and must match `^[a-z0-9][a-z0-9-]{0,39}$`. The hub offers at most 20 valid templates, sorted by ID.

```html
<!doctype html>
<html><head><title>One-pager</title>
<meta name="template:description" content="A concise proposal and next steps.">
<meta name="template:when" content="Use for executive briefs or proposals.">
<style data-template-layout>.sections { display: grid; gap: 1rem; }</style>
</head><body class="ds-page"><main class="sections">
<section class="ds-card" data-slot="summary" data-slot-hint="Summarize the proposal.">
  <h1>Sample proposal</h1><p>Replace this sample content.</p>
</section>
<section data-slot="metric" data-slot-repeat data-slot-hint="One evidenced metric.">42 sample users</section>
<section data-slot="risks" data-slot-optional>Sample risk</section>
</main></body></html>
```

Title is required (1–60 characters); `template:description` is required (1–200); `template:when` is optional (up to 300). These values have whitespace collapsed. Use design system classes for appearance and at most one `<style data-template-layout>` for layout. Other style blocks are invalid.

At least one `data-slot` is required. Slot IDs are unique and match `^[a-z0-9][a-z0-9_-]{0,39}$`. Optional `data-slot-hint` is up to 300 characters. The presence of `data-slot-repeat` allows repetition; `data-slot-optional` allows omission. Include sample content so the template renders as a standalone preview. Generation replaces all sample content, follows hints, and keeps section order.

## Selection, warnings and API

Choice precedence is client pin (`artifactTemplate` on messages, `template` on artifact actions), model `start_artifact.template`, the latest version's template on revisions, then free. Omitted, empty or `auto` means no pin; `free` explicitly disables templates but still applies the design system. An unknown or invalid ID falls back to free and adds a card notice. Changing an existing artifact's template creates a new version and asks the model to preserve all content, facts and numbers.

An invalid design system is ignored with a warning and a card notice. Invalid templates are skipped with warnings; extra valid templates beyond the cap are also skipped. The existing artifact host/URL allowlist and sandbox remain unchanged. External brand asset hosts are not allowed.

`GET /api/ai-chat/sources?workspace=W` includes `artifactKit` when configured. `GET /api/ai-chat/artifact-kit?workspace=W` returns the kit with signed preview URLs when signing is available. Preview capabilities last 15 minutes, are scoped to tenant, workspace and template, and are revoked when AI Chat is turned off. `/artifact-templates/{id}/preview` serves the current template with the current valid design system and the same CSP/sandbox as raw artifacts. Consumers should use a sandboxed iframe without `allow-same-origin`.

Artifact cards expose the resolved `template`, its generation-time `templateName`, and any `notices`. Artifact metadata exposes the version's `template` and the current template name (or ID if removed). Free artifacts omit both fields. Legacy versions remain free.

See `pkg/hub/aichat/testdata/artifact-kit` for a complete catalog and two templates.
