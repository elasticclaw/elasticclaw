package hub

import (
	"database/sql"
	"encoding/json"
	"strings"
)

// loadManualTriggerInputs reads the manual trigger inputs stored in a claw's
// TRIGGER_INPUTS.json file. Returns nil if the claw was not manually triggered.
func (s *Server) loadManualTriggerInputs(clawID string) map[string]string {
	filesJSON, tagsJSON, err := s.st().Claws().TemplateFilesTags(s.base(), clawID)
	if err != nil {
		if err != sql.ErrNoRows {
			logf("[manual-trigger] failed to load claw %s: %v", clawID[:8], err)
		}
		return nil
	}

	// Check for manual trigger tag
	var tags []string
	if err := json.Unmarshal([]byte(tagsJSON), &tags); err == nil {
		found := false
		for _, t := range tags {
			if t == "manual-trigger" {
				found = true
				break
			}
		}
		if !found {
			return nil
		}
	}

	var files map[string]string
	if err := json.Unmarshal([]byte(filesJSON), &files); err != nil {
		logf("[manual-trigger] failed to parse template_files for claw %s: %v", clawID[:8], err)
		return nil
	}

	// Parse inputs from TRIGGER_INPUTS.json (reliable JSON, not fragile markdown)
	inputsJSON, ok := files["TRIGGER_INPUTS.json"]
	if !ok {
		// Fallback: try old CONTEXT.md parsing for backwards compat
		return s.loadManualTriggerInputsFromContextMD(files)
	}

	var inputs map[string]string
	if err := json.Unmarshal([]byte(inputsJSON), &inputs); err != nil {
		logf("[manual-trigger] failed to parse TRIGGER_INPUTS.json for claw %s: %v", clawID[:8], err)
		return nil
	}
	if len(inputs) == 0 {
		return nil
	}
	return inputs
}

// loadManualTriggerInputsFromContextMD is the legacy markdown parser for
// backwards compatibility with claws created before TRIGGER_INPUTS.json existed.
func (s *Server) loadManualTriggerInputsFromContextMD(files map[string]string) map[string]string {
	ctx, ok := files["CONTEXT.md"]
	if !ok {
		return nil
	}

	inputs := make(map[string]string)
	// Simple parser: look for "- **name**: value" lines in the Inputs section
	// This is fragile — values containing "**: " will be mis-split.
	lines := strings.Split(ctx, "\n")
	inInputs := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "## Inputs" {
			inInputs = true
			continue
		}
		if inInputs && strings.HasPrefix(line, "- **") {
			line = strings.TrimPrefix(line, "- **")
			parts := strings.SplitN(line, "**: ", 2)
			if len(parts) == 2 {
				inputs[parts[0]] = parts[1]
			}
		}
		if inInputs && line != "" && !strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "  -") {
			break
		}
	}
	if len(inputs) == 0 {
		return nil
	}
	return inputs
}
