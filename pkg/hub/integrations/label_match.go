package integrations

import "strings"

func labelsAllowed(currentLabels []string, requiredLabels []string, excludedLabels []string) bool {
	return labelSetAllowed(labelSetFromSlice(currentLabels), requiredLabels, excludedLabels)
}

func labelSetAllowed(current map[string]bool, requiredLabels []string, excludedLabels []string) bool {
	normalized := make(map[string]bool, len(current))
	for label, present := range current {
		if !present {
			continue
		}
		key := normalizeLabel(label)
		if key != "" {
			normalized[key] = true
		}
	}
	for _, required := range requiredLabels {
		key := normalizeLabel(required)
		if key == "" {
			continue
		}
		if !normalized[key] {
			return false
		}
	}
	for _, excluded := range excludedLabels {
		key := normalizeLabel(excluded)
		if key == "" {
			continue
		}
		if normalized[key] {
			return false
		}
	}
	return true
}

func labelSetFromSlice(labels []string) map[string]bool {
	set := make(map[string]bool, len(labels))
	for _, label := range labels {
		key := normalizeLabel(label)
		if key != "" {
			set[key] = true
		}
	}
	return set
}

func normalizeLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}
