// Package v2 implements workspace and workflow schema version 2 foundations
// from RFC issue #544 (Phase 1): parse, structural validation, pair validation,
// and immutable content revisions. V1 schemas remain in package types.
package v2

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// SchemaVersionV2 is the canonical authored form used in issue #544 examples.
const SchemaVersionV2 = "2"

// IsV2 reports whether a schema_version value identifies workspace/workflow v2.
// Accepts "2", 2, "v2" (case-insensitive). Does not treat "v1" or empty as v2.
func IsV2(version string) bool {
	v := strings.TrimSpace(strings.ToLower(version))
	return v == "2" || v == "v2"
}

// DetectSchemaVersion reads schema_version from raw YAML without full decode.
// Returns the normalized string form ("v1", "2", …) or empty if absent.
func DetectSchemaVersion(data []byte) (string, error) {
	var probe struct {
		SchemaVersion yaml.Node `yaml:"schema_version"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return "", fmt.Errorf("parse schema_version: %w", err)
	}
	return normalizeSchemaVersionNode(&probe.SchemaVersion), nil
}

func normalizeSchemaVersionNode(n *yaml.Node) string {
	if n == nil || n.Kind == 0 {
		return ""
	}
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!int" || n.Tag == "!!float" {
			// Keep integer form as decimal string without "v" prefix.
			if i, err := strconv.ParseInt(n.Value, 10, 64); err == nil {
				return strconv.FormatInt(i, 10)
			}
		}
		return strings.TrimSpace(n.Value)
	default:
		return strings.TrimSpace(n.Value)
	}
}

// SchemaVersionString normalizes a decoded schema_version field that may arrive
// as string or numeric YAML into a comparable string.
func SchemaVersionString(raw interface{}) string {
	switch v := raw.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strings.TrimSpace(fmt.Sprint(v))
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}
