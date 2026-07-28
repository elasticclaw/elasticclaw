// Package convert migrates authored workspace and workflow documents between
// schema versions. Converters are registered as (kind, from, to) so v3 and
// later paths can be added without changing the CLI surface.
package convert

import (
	"fmt"
	"sort"
	"strings"

	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

// Kind identifies which document family is being converted.
type Kind string

const (
	KindWorkspace Kind = "workspace"
	KindWorkflow  Kind = "workflow"
)

// Result is the output of a successful conversion.
type Result struct {
	// Output is the converted document as YAML bytes (trailing newline).
	Output []byte
	// From is the detected or requested source schema version (normalized).
	From string
	// To is the target schema version (normalized).
	To string
	// Warnings are human-readable notes about lossy or incomplete mappings.
	// Callers should surface these; conversion can succeed with warnings.
	Warnings []string
}

// Options control conversion behavior.
type Options struct {
	// From is the source schema version. Empty means auto-detect from document.
	From string
	// To is the target schema version. Required (CLI defaults to "2").
	To string
	// WorkspaceYAML is optional workspace v2 YAML used when converting a
	// workflow so policy/resource refs can be pair-validated when present.
	WorkspaceYAML []byte
	// Validate runs post-conversion schema validation (default true).
	Validate *bool
}

func shouldValidate(opts Options) bool {
	if opts.Validate == nil {
		return true
	}
	return *opts.Validate
}

type converterFunc func(data []byte, opts Options) (Result, error)

type route struct {
	kind Kind
	from string
	to   string
	fn   converterFunc
}

var routes []route

func register(kind Kind, from, to string, fn converterFunc) {
	routes = append(routes, route{kind: kind, from: normalizeVersion(from), to: normalizeVersion(to), fn: fn})
}

func init() {
	register(KindWorkspace, "v1", "2", convertWorkspaceV1ToV2)
	register(KindWorkflow, "v1", "2", convertWorkflowV1ToV2)
}

// SupportedTargets lists target versions available for a kind from a source.
func SupportedTargets(kind Kind, from string) []string {
	from = normalizeVersion(from)
	var out []string
	for _, r := range routes {
		if r.kind == kind && r.from == from {
			out = append(out, r.to)
		}
	}
	sort.Strings(out)
	return out
}

// NormalizeVersion maps authored forms to a canonical conversion key.
// "v1"/"1" → "v1"; "2"/"v2" → "2".
func NormalizeVersion(v string) string { return normalizeVersion(v) }

func normalizeVersion(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	switch v {
	case "", "v1", "1":
		if v == "" {
			return ""
		}
		return "v1"
	case "2", "v2":
		return "2"
	case "3", "v3":
		return "3"
	default:
		// Keep "v1"-style and bare numbers as-is when already canonical-ish.
		if strings.HasPrefix(v, "v") {
			return v
		}
		return v
	}
}

// DetectVersion reads schema_version from YAML.
func DetectVersion(data []byte) (string, error) {
	raw, err := v2.DetectSchemaVersion(data)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(raw) == "" {
		// Historical docs omit schema_version; treat as v1.
		return "v1", nil
	}
	return normalizeVersion(raw), nil
}

// Convert migrates a document of the given kind from opts.From (or auto) to opts.To.
func Convert(kind Kind, data []byte, opts Options) (Result, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return Result{}, fmt.Errorf("%s: empty document", kind)
	}
	to := normalizeVersion(opts.To)
	if to == "" {
		return Result{}, fmt.Errorf("--to is required (supported targets depend on source version)")
	}

	from := normalizeVersion(opts.From)
	if from == "" {
		detected, err := DetectVersion(data)
		if err != nil {
			return Result{}, err
		}
		from = detected
	}

	if from == to {
		return Result{}, fmt.Errorf("%s is already schema version %s", kind, displayVersion(from))
	}

	// Direct route.
	if fn := findRoute(kind, from, to); fn != nil {
		opts.From = from
		opts.To = to
		return fn(data, opts)
	}

	// Future: multi-hop (v1→v2→v3). For now only direct edges are registered.
	return Result{}, fmt.Errorf("no converter for %s %s → %s (supported from %s: %v)",
		kind, displayVersion(from), displayVersion(to), displayVersion(from), SupportedTargets(kind, from))
}

func findRoute(kind Kind, from, to string) converterFunc {
	for _, r := range routes {
		if r.kind == kind && r.from == from && r.to == to {
			return r.fn
		}
	}
	return nil
}

func displayVersion(v string) string {
	if v == "v1" {
		return "v1"
	}
	return v
}

func appendWarning(warnings *[]string, format string, args ...interface{}) {
	*warnings = append(*warnings, fmt.Sprintf(format, args...))
}
