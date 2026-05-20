package hub

import (
	"strings"
	"testing"
)

func TestInjectFigmaAPIDocsRequiresFigmaKey(t *testing.T) {
	files := map[string]string{"TOOLS.md": "# Tools\n"}

	got := injectFigmaAPIDocs(files, map[string]string{"OTHER_KEY": "value"})

	if _, ok := got["FIGMA_API.md"]; ok {
		t.Fatalf("FIGMA_API.md was injected without %s", figmaAPIEnvVar)
	}
	if strings.Contains(got["TOOLS.md"], "Figma API Access") {
		t.Fatalf("TOOLS.md was changed without %s", figmaAPIEnvVar)
	}
}

func TestInjectFigmaAPIDocsAddsGuideAndToolsNote(t *testing.T) {
	files := map[string]string{"TOOLS.md": "# Tools\n"}

	got := injectFigmaAPIDocs(files, map[string]string{figmaAPIEnvVar: "token"})

	if !strings.Contains(got["FIGMA_API.md"], "X-Figma-Token") {
		t.Fatalf("FIGMA_API.md does not document Figma API authentication")
	}
	if !strings.Contains(got["TOOLS.md"], "FIGMA_API.md") {
		t.Fatalf("TOOLS.md does not point to FIGMA_API.md")
	}
}

func TestInjectFigmaAPIDocsDoesNotDuplicateToolsNote(t *testing.T) {
	files := map[string]string{"TOOLS.md": "# Tools\n"}

	got := injectFigmaAPIDocs(files, map[string]string{figmaAPIEnvVar: "token"})
	got = injectFigmaAPIDocs(got, map[string]string{figmaAPIEnvVar: "token"})

	if count := strings.Count(got["TOOLS.md"], "FIGMA_API.md"); count != 1 {
		t.Fatalf("TOOLS.md contains FIGMA_API.md %d times, want 1", count)
	}
}
