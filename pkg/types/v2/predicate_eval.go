package v2

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// MatchPredicate evaluates the restricted predicate language against facts.
// It is intentionally data-only: callers provide durable facts/event payloads,
// never conversation text or executable expressions.
func MatchPredicate(predicate map[string]interface{}, facts map[string]interface{}) (bool, error) {
	if len(predicate) == 0 {
		return true, nil
	}
	return matchPredicateMap(predicate, facts)
}

func matchPredicateMap(predicate, facts map[string]interface{}) (bool, error) {
	for key, expected := range predicate {
		switch key {
		case "all":
			items, ok := expected.([]interface{})
			if !ok {
				return false, fmt.Errorf("all requires a list")
			}
			for _, item := range items {
				child, ok := item.(map[string]interface{})
				if !ok {
					return false, fmt.Errorf("all item must be a predicate map")
				}
				matched, err := matchPredicateMap(child, facts)
				if err != nil || !matched {
					return matched, err
				}
			}
		case "any":
			items, ok := expected.([]interface{})
			if !ok {
				return false, fmt.Errorf("any requires a list")
			}
			matchedAny := false
			for _, item := range items {
				child, ok := item.(map[string]interface{})
				if !ok {
					return false, fmt.Errorf("any item must be a predicate map")
				}
				matched, err := matchPredicateMap(child, facts)
				if err != nil {
					return false, err
				}
				matchedAny = matchedAny || matched
			}
			if !matchedAny {
				return false, nil
			}
		default:
			actual, exists := facts[key]
			matched, err := matchConstraint(expected, actual, exists)
			if err != nil || !matched {
				return matched, err
			}
		}
	}
	return true, nil
}

func matchConstraint(expected, actual interface{}, exists bool) (bool, error) {
	constraint, isMap := expected.(map[string]interface{})
	if !isMap {
		return exists && valuesEqual(actual, expected), nil
	}

	hasOperator := false
	for op, value := range constraint {
		switch op {
		case "equals":
			hasOperator = true
			if !exists || !valuesEqual(actual, value) {
				return false, nil
			}
		case "not_equals":
			hasOperator = true
			if exists && valuesEqual(actual, value) {
				return false, nil
			}
		case "in", "not_in":
			hasOperator = true
			items, ok := value.([]interface{})
			if !ok {
				return false, fmt.Errorf("%s requires a list", op)
			}
			found := false
			for _, item := range items {
				found = found || (exists && valuesEqual(actual, item))
			}
			if (op == "in" && !found) || (op == "not_in" && found) {
				return false, nil
			}
		case "exists":
			hasOperator = true
			want, ok := value.(bool)
			if !ok {
				return false, fmt.Errorf("exists requires a boolean")
			}
			if exists != want {
				return false, nil
			}
		}
	}
	if hasOperator {
		for key := range constraint {
			if !isConstraintOperator(key) {
				return false, fmt.Errorf("unsupported predicate operator %q", key)
			}
		}
		return true, nil
	}
	if !exists {
		return false, nil
	}
	actualMap, ok := actual.(map[string]interface{})
	if !ok {
		return false, nil
	}
	return matchPredicateMap(constraint, actualMap)
}

func isConstraintOperator(value string) bool {
	switch value {
	case "equals", "not_equals", "in", "not_in", "exists":
		return true
	default:
		return false
	}
}

func valuesEqual(a, b interface{}) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	if left, ok := numericValue(a); ok {
		if right, ok := numericValue(b); ok {
			return left == right
		}
	}
	// YAML and JSON decoders use different concrete numeric types. Canonical
	// JSON makes semantically equal scalar/collection values comparable.
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func numericValue(value interface{}) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int8:
		return float64(number), true
	case int16:
		return float64(number), true
	case int32:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint:
		return float64(number), true
	case uint8:
		return float64(number), true
	case uint16:
		return float64(number), true
	case uint32:
		return float64(number), true
	case uint64:
		return float64(number), true
	case float32:
		return float64(number), true
	case float64:
		return number, true
	default:
		return 0, false
	}
}
