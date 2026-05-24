package hub

import "strings"

const figmaAPIEnvVar = "FIGMA_API_KEY"

const figmaAPIMarkdown = `# Figma API

This agent has access to the Figma REST API through the ` + "`FIGMA_API_KEY`" + ` environment variable.

Use the token only through request headers. Do not print it, write it to files, include it in logs, or commit it.

## Authentication

Figma REST endpoints use ` + "`https://api.figma.com`" + ` and accept personal access tokens in the ` + "`X-Figma-Token`" + ` header.

` + "```bash" + `
curl -sS \
  -H "X-Figma-Token: $FIGMA_API_KEY" \
  "https://api.figma.com/v1/files/<file_key>"
` + "```" + `

## Finding File Keys and Node IDs

A Figma URL usually looks like this:

` + "```text" + `
https://www.figma.com/design/<file_key>/<file_name>?node-id=123-456
` + "```" + `

Use the path segment after ` + "`/design/`" + ` or ` + "`/file/`" + ` as the file key. Use the ` + "`node-id`" + ` query parameter for targeted node requests. In API calls, node IDs usually use a colon, so ` + "`123-456`" + ` from a URL becomes ` + "`123:456`" + `.

## Useful Requests

Fetch the full file JSON:

` + "```bash" + `
curl -sS \
  -H "X-Figma-Token: $FIGMA_API_KEY" \
  "https://api.figma.com/v1/files/<file_key>" \
  -o figma-file.json
` + "```" + `

Fetch selected nodes:

` + "```bash" + `
curl -sS \
  -H "X-Figma-Token: $FIGMA_API_KEY" \
  "https://api.figma.com/v1/files/<file_key>/nodes?ids=<node_id>"
` + "```" + `

Render frames or nodes as images:

` + "```bash" + `
curl -sS \
  -H "X-Figma-Token: $FIGMA_API_KEY" \
  "https://api.figma.com/v1/images/<file_key>?ids=<node_id>&format=png&scale=2"
` + "```" + `

Inspect variables:

` + "```bash" + `
curl -sS \
  -H "X-Figma-Token: $FIGMA_API_KEY" \
  "https://api.figma.com/v1/files/<file_key>/variables/local"
` + "```" + `

## How to Use This Well

Prefer targeted node requests over fetching an entire large file when a Figma URL includes a node ID. Use the file JSON to inspect frames, text, component names, styles, layout constraints, fills, strokes, spacing, and design tokens. Use the images endpoint when you need a visual reference for a specific frame or component.

If an API call fails with 401 or 403, the token may be missing scopes, expired, or lack access to the file. If it fails with 429, reduce request volume and batch node/image requests where possible.
`

const figmaToolsNote = `

## Figma API Access

` + "`FIGMA_API_KEY`" + ` is available in the environment. Use ` + "`FIGMA_API.md`" + ` for the Figma REST API workflow, including authentication, file key parsing, node requests, image rendering, and token safety rules.
`

func injectFigmaAPIDocs(files map[string]string, env map[string]string) map[string]string {
	if strings.TrimSpace(env[figmaAPIEnvVar]) == "" {
		return files
	}
	if files == nil {
		files = make(map[string]string)
	}
	if strings.TrimSpace(files["FIGMA_API.md"]) == "" {
		files["FIGMA_API.md"] = figmaAPIMarkdown
	}
	if !strings.Contains(files["TOOLS.md"], "FIGMA_API.md") {
		existing := strings.TrimRight(files["TOOLS.md"], "\n")
		note := strings.TrimLeft(figmaToolsNote, "\n")
		if existing == "" {
			files["TOOLS.md"] = note
		} else {
			files["TOOLS.md"] = existing + "\n\n" + note
		}
	}
	return files
}
