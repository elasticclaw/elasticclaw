package v2

import (
	"fmt"
	"sort"
	"strings"
)

// Restricted predicate operators allowed in event-clause `when` trees.
var allowedPredicateOps = map[string]bool{
	"equals":     true,
	"not_equals": true,
	"in":         true,
	"not_in":     true,
	"exists":     true,
	"all":        true,
	"any":        true,
}

// ValidatePredicateTree walks a when/assert-style map and ensures only the
// restricted predicate language is used at operator positions.
// Field maps like {conclusion: {equals: success}} are allowed.
func ValidatePredicateTree(path string, tree map[string]interface{}) error {
	if tree == nil {
		return nil
	}
	return validatePredicateNode(path, tree)
}

func validatePredicateNode(path string, node interface{}) error {
	switch n := node.(type) {
	case map[string]interface{}:
		// Either a logical combinator (all/any) or a field -> constraint map,
		// or a single operator map (equals/in/…).
		if len(n) == 1 {
			for k, v := range n {
				if allowedPredicateOps[k] {
					return validateOperator(path+"."+k, k, v)
				}
				// field name -> nested constraint
				return validatePredicateNode(path+"."+k, v)
			}
		}
		// Multiple keys: treat as all-of field constraints unless they are all/any only.
		hasAll := false
		hasAny := false
		for k, v := range n {
			if k == "all" {
				hasAll = true
				if err := validateOperator(path+".all", "all", v); err != nil {
					return err
				}
				continue
			}
			if k == "any" {
				hasAny = true
				if err := validateOperator(path+".any", "any", v); err != nil {
					return err
				}
				continue
			}
			if allowedPredicateOps[k] && k != "all" && k != "any" {
				// Multiple top-level ops without field context — allow equals-style only alone.
				return fmt.Errorf("%s: multiple top-level predicate operators without field context", path)
			}
			if err := validatePredicateNode(path+"."+k, v); err != nil {
				return err
			}
		}
		_ = hasAll
		_ = hasAny
		return nil
	case []interface{}:
		for i, item := range n {
			if err := validatePredicateNode(fmt.Sprintf("%s[%d]", path, i), item); err != nil {
				return err
			}
		}
		return nil
	default:
		// Scalar leaf (used as equals value) — ok at leaves.
		return nil
	}
}

func validateOperator(path, op string, value interface{}) error {
	if !allowedPredicateOps[op] {
		return fmt.Errorf("%s: unsupported predicate operator %q (allowed: equals, not_equals, in, not_in, exists, all, any)", path, op)
	}
	switch op {
	case "equals", "not_equals":
		// any scalar
		return nil
	case "in", "not_in":
		list, ok := value.([]interface{})
		if !ok {
			return fmt.Errorf("%s: %s requires a list", path, op)
		}
		if len(list) == 0 {
			return fmt.Errorf("%s: %s list cannot be empty", path, op)
		}
		return nil
	case "exists":
		switch value.(type) {
		case bool, nil:
			return nil
		default:
			return fmt.Errorf("%s: exists requires a boolean", path)
		}
	case "all", "any":
		list, ok := value.([]interface{})
		if !ok {
			return fmt.Errorf("%s: %s requires a list of predicates", path, op)
		}
		for i, item := range list {
			if err := validatePredicateNode(fmt.Sprintf("%s[%d]", path, i), item); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%s: unsupported predicate operator %q", path, op)
	}
}

// fieldConstraint is a simplified constraint on one field for overlap analysis.
type fieldConstraint struct {
	// kind: equals | not_equals | in | not_in | exists | unknown
	kind   string
	equals interface{}
	set    map[string]struct{} // for in/not_in; stringified values
	exists *bool
}

// OverlapWitness describes a concrete value that satisfies two clauses.
type OverlapWitness struct {
	Field string
	Value string
}

// ClausesOverlap reports whether two when-predicate trees may both match the
// same payload. If analysis cannot prove disjointness, they are treated as
// overlapping (conservative).
func ClausesOverlap(a, b map[string]interface{}) (bool, *OverlapWitness) {
	if len(a) == 0 && len(b) == 0 {
		return true, &OverlapWitness{Field: "*", Value: "<empty>"}
	}
	// Flatten field constraints from both trees.
	fa := collectFieldConstraints(a)
	fb := collectFieldConstraints(b)

	// If either tree uses unknown structure (combinators we cannot flatten),
	// treat as overlapping.
	if fa == nil || fb == nil {
		return true, &OverlapWitness{Field: "*", Value: "<unanalyzable>"}
	}

	// Fields only in one side do not prevent overlap.
	// For each field present in both, constraints must be pairwise satisfiable.
	// Also synthesize a witness value for one shared field when possible.
	var witness *OverlapWitness
	for field, ca := range fa {
		cb, ok := fb[field]
		if !ok {
			continue
		}
		okOverlap, w := constraintsOverlap(field, ca, cb)
		if !okOverlap {
			return false, nil
		}
		if witness == nil && w != nil {
			witness = w
		}
	}
	if witness == nil {
		// No conflicting fields; pick any constraint value as witness.
		for field, ca := range fa {
			if s := witnessFromConstraint(field, ca); s != nil {
				witness = s
				break
			}
		}
		if witness == nil {
			for field, cb := range fb {
				if s := witnessFromConstraint(field, cb); s != nil {
					witness = s
					break
				}
			}
		}
		if witness == nil {
			witness = &OverlapWitness{Field: "*", Value: "<any>"}
		}
	}
	return true, witness
}

func collectFieldConstraints(tree map[string]interface{}) map[string]fieldConstraint {
	out := map[string]fieldConstraint{}
	if !flattenPredicates("", tree, out) {
		return nil
	}
	return out
}

// flattenPredicates extracts field-level constraints. Returns false if unanalyzable.
func flattenPredicates(prefix string, node interface{}, out map[string]fieldConstraint) bool {
	m, ok := node.(map[string]interface{})
	if !ok {
		return true
	}
	// Logical all: conjunction — merge all children.
	if all, ok := m["all"]; ok && len(m) == 1 {
		list, ok := all.([]interface{})
		if !ok {
			return false
		}
		for _, item := range list {
			if !flattenPredicates(prefix, item, out) {
				return false
			}
		}
		return true
	}
	// any: disjunction — unanalyzable for precise overlap without DNF expansion.
	// Conservative: mark unanalyzable so ClausesOverlap treats as overlapping.
	if _, ok := m["any"]; ok {
		return false
	}

	// Single operator at this level without field: unanalyzable here.
	if len(m) == 1 {
		for k, v := range m {
			if allowedPredicateOps[k] {
				// bare op without field
				return false
			}
			// field -> constraint
			field := k
			if prefix != "" {
				field = prefix + "." + k
			}
			return absorbConstraint(field, v, out)
		}
	}

	// Multiple field keys: conjunction.
	for k, v := range m {
		if allowedPredicateOps[k] {
			return false
		}
		field := k
		if prefix != "" {
			field = prefix + "." + k
		}
		if !absorbConstraint(field, v, out) {
			return false
		}
	}
	return true
}

func absorbConstraint(field string, node interface{}, out map[string]fieldConstraint) bool {
	m, ok := node.(map[string]interface{})
	if !ok {
		// Nested object field path
		if nested, ok := node.(map[string]interface{}); ok {
			return flattenPredicates(field, nested, out)
		}
		// Treat bare scalar as equals
		out[field] = fieldConstraint{kind: "equals", equals: node}
		return true
	}
	// Nested field map without ops
	if len(m) == 1 {
		for k, v := range m {
			if allowedPredicateOps[k] {
				c, ok := parseConstraint(k, v)
				if !ok {
					return false
				}
				// Merge with existing via conjunction
				if prev, exists := out[field]; exists {
					merged, ok := conjoinConstraints(prev, c)
					if !ok {
						// Unsatisfiable self-conjunction — still store impossible? mark unanalyzable
						return false
					}
					out[field] = merged
				} else {
					out[field] = c
				}
				return true
			}
			// deeper nesting
			return flattenPredicates(field, m, out)
		}
	}
	// Multiple keys under field: either multiple ops or nested fields.
	onlyOps := true
	for k := range m {
		if !allowedPredicateOps[k] {
			onlyOps = false
			break
		}
	}
	if onlyOps {
		var acc *fieldConstraint
		for k, v := range m {
			c, ok := parseConstraint(k, v)
			if !ok {
				return false
			}
			if acc == nil {
				acc = &c
			} else {
				merged, ok := conjoinConstraints(*acc, c)
				if !ok {
					return false
				}
				acc = &merged
			}
		}
		if acc != nil {
			out[field] = *acc
		}
		return true
	}
	return flattenPredicates(field, m, out)
}

func parseConstraint(op string, value interface{}) (fieldConstraint, bool) {
	switch op {
	case "equals":
		return fieldConstraint{kind: "equals", equals: value}, true
	case "not_equals":
		return fieldConstraint{kind: "not_equals", equals: value}, true
	case "in":
		list, ok := value.([]interface{})
		if !ok {
			return fieldConstraint{}, false
		}
		set := map[string]struct{}{}
		for _, item := range list {
			set[stringifyValue(item)] = struct{}{}
		}
		return fieldConstraint{kind: "in", set: set}, true
	case "not_in":
		list, ok := value.([]interface{})
		if !ok {
			return fieldConstraint{}, false
		}
		set := map[string]struct{}{}
		for _, item := range list {
			set[stringifyValue(item)] = struct{}{}
		}
		return fieldConstraint{kind: "not_in", set: set}, true
	case "exists":
		b, ok := value.(bool)
		if !ok {
			return fieldConstraint{}, false
		}
		return fieldConstraint{kind: "exists", exists: &b}, true
	default:
		return fieldConstraint{}, false
	}
}

func conjoinConstraints(a, b fieldConstraint) (fieldConstraint, bool) {
	// Conservative: only handle simple equals ∩ equals/in.
	if a.kind == "equals" && b.kind == "equals" {
		if stringifyValue(a.equals) == stringifyValue(b.equals) {
			return a, true
		}
		return fieldConstraint{}, false
	}
	if a.kind == "equals" && b.kind == "in" {
		if _, ok := b.set[stringifyValue(a.equals)]; ok {
			return a, true
		}
		return fieldConstraint{}, false
	}
	if b.kind == "equals" && a.kind == "in" {
		if _, ok := a.set[stringifyValue(b.equals)]; ok {
			return b, true
		}
		return fieldConstraint{}, false
	}
	// Otherwise treat as unanalyzable merge.
	return fieldConstraint{kind: "unknown"}, true
}

func constraintsOverlap(field string, a, b fieldConstraint) (bool, *OverlapWitness) {
	// unknown is always potentially overlapping
	if a.kind == "unknown" || b.kind == "unknown" {
		return true, &OverlapWitness{Field: field, Value: "<unknown>"}
	}
	// equals vs equals
	if a.kind == "equals" && b.kind == "equals" {
		if stringifyValue(a.equals) == stringifyValue(b.equals) {
			return true, &OverlapWitness{Field: field, Value: stringifyValue(a.equals)}
		}
		return false, nil
	}
	// equals vs in
	if a.kind == "equals" && b.kind == "in" {
		if _, ok := b.set[stringifyValue(a.equals)]; ok {
			return true, &OverlapWitness{Field: field, Value: stringifyValue(a.equals)}
		}
		return false, nil
	}
	if b.kind == "equals" && a.kind == "in" {
		if _, ok := a.set[stringifyValue(b.equals)]; ok {
			return true, &OverlapWitness{Field: field, Value: stringifyValue(b.equals)}
		}
		return false, nil
	}
	// equals vs not_in
	if a.kind == "equals" && b.kind == "not_in" {
		if _, ok := b.set[stringifyValue(a.equals)]; ok {
			return false, nil
		}
		return true, &OverlapWitness{Field: field, Value: stringifyValue(a.equals)}
	}
	if b.kind == "equals" && a.kind == "not_in" {
		if _, ok := a.set[stringifyValue(b.equals)]; ok {
			return false, nil
		}
		return true, &OverlapWitness{Field: field, Value: stringifyValue(b.equals)}
	}
	// equals vs not_equals
	if a.kind == "equals" && b.kind == "not_equals" {
		if stringifyValue(a.equals) == stringifyValue(b.equals) {
			return false, nil
		}
		return true, &OverlapWitness{Field: field, Value: stringifyValue(a.equals)}
	}
	if b.kind == "equals" && a.kind == "not_equals" {
		if stringifyValue(b.equals) == stringifyValue(a.equals) {
			return false, nil
		}
		return true, &OverlapWitness{Field: field, Value: stringifyValue(b.equals)}
	}
	// in vs in: intersection
	if a.kind == "in" && b.kind == "in" {
		for v := range a.set {
			if _, ok := b.set[v]; ok {
				return true, &OverlapWitness{Field: field, Value: v}
			}
		}
		return false, nil
	}
	// in vs not_in
	if a.kind == "in" && b.kind == "not_in" {
		for v := range a.set {
			if _, ok := b.set[v]; !ok {
				return true, &OverlapWitness{Field: field, Value: v}
			}
		}
		return false, nil
	}
	if b.kind == "in" && a.kind == "not_in" {
		for v := range b.set {
			if _, ok := a.set[v]; !ok {
				return true, &OverlapWitness{Field: field, Value: v}
			}
		}
		return false, nil
	}
	// not_equals / not_in / exists combinations: usually overlapping
	return true, &OverlapWitness{Field: field, Value: "<possible>"}
}

func witnessFromConstraint(field string, c fieldConstraint) *OverlapWitness {
	switch c.kind {
	case "equals":
		return &OverlapWitness{Field: field, Value: stringifyValue(c.equals)}
	case "in":
		vals := make([]string, 0, len(c.set))
		for v := range c.set {
			vals = append(vals, v)
		}
		sort.Strings(vals)
		if len(vals) > 0 {
			return &OverlapWitness{Field: field, Value: vals[0]}
		}
	}
	return nil
}

func stringifyValue(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", t)
	case int64:
		return fmt.Sprintf("%d", t)
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%v", t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// FormatOverlapError builds the multi-clause error text expected by the RFC.
func FormatOverlapError(eventOrContext, state string, clausePaths []string, witness *OverlapWitness) string {
	var b strings.Builder
	if state != "" {
		fmt.Fprintf(&b, "%s has overlapping clauses for state %q\n", eventOrContext, state)
	} else {
		fmt.Fprintf(&b, "%s has overlapping clauses\n", eventOrContext)
	}
	for i, p := range clausePaths {
		fmt.Fprintf(&b, "\nclause %d path: %s", i+1, p)
	}
	if witness != nil {
		fmt.Fprintf(&b, "\n\nboth clauses may match %s = %s", witness.Field, witness.Value)
	}
	return b.String()
}
