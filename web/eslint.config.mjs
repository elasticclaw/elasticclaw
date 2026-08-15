import next from "eslint-config-next";

// Raw Tailwind palette classes (bg-green-500, text-red-400, …) bypass the
// Industry design system: status must come from the status-* tokens and every
// other color from the semantic tokens or the industry-*/neutral-* ramps.
// Severity is "warn" for now so the existing offenders stay visible without
// blocking CI; a follow-up commit flips it to "error" once they are cleared.
const RAW_PALETTE_CLASS =
  "/\\b(?:bg|text|border|fill|stroke|from|to|ring)-(?:red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose|slate|gray|zinc|stone)-\\d{2,3}\\b/";

const RAW_PALETTE_MESSAGE =
  "Raw Tailwind palette color. Use the Industry semantic tokens (bg-primary, text-muted-foreground), the status-* tokens for state, or the industry-*/neutral-* ramps. See web/DESIGN.md.";

/** @type {import("eslint").Linter.Config[]} */
const config = [
  ...next,
  {
    files: ["app/**/*.{ts,tsx,js,jsx}", "components/**/*.{ts,tsx,js,jsx}"],
    rules: {
      "no-restricted-syntax": [
        "warn",
        {
          selector: `Literal[value=${RAW_PALETTE_CLASS}]`,
          message: RAW_PALETTE_MESSAGE,
        },
        {
          selector: `TemplateElement[value.raw=${RAW_PALETTE_CLASS}]`,
          message: RAW_PALETTE_MESSAGE,
        },
      ],
    },
  },
];

export default config;
