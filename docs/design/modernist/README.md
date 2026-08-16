# Modernist design system — reference specs

Imported from the Claude Design project "Modernist" (`templates/elasticclaw/`).
These files are the source of truth for the web UI restyle. They are static
mockups — the relative `<link>` paths inside the HTML files are not meant to
resolve here.

- `styles.css` — design-system tokens + component classes (the whole system).
- `theme.json` — the compact seed the tokens were generated from. To replicate
  a future design system: regenerate/replace `styles.css` from a new theme and
  re-tune only the "Modernist tokens" layer in `web/app/globals.css`.
- `app.css` — ElasticClaw layout scaffolding (layout only; colors/type/radius
  all come from the tokens). Adds the 3 status colors (`--status-ok/warn/bad`).
- `login.html`, `setup.html`, `board.html`, `conversation.html`,
  `analytics.html`, `settings.html` — the six target screens.

The app shell stays dark-only (`data-theme="dark"` equivalent), matching the
mockups; the light palette ships in the tokens but is not user-switchable.
