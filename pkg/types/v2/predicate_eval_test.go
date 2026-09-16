package v2_test

import (
	"testing"

	v2 "github.com/elasticclaw/elasticclaw/pkg/types/v2"
)

func TestMatchPredicateRestrictedLanguage(t *testing.T) {
	facts := map[string]interface{}{
		"ci": map[string]interface{}{
			"status":   "satisfied",
			"attempt":  float64(2),
			"optional": false,
		},
	}
	tests := []struct {
		name      string
		predicate map[string]interface{}
		want      bool
	}{
		{"nested equals", map[string]interface{}{"ci": map[string]interface{}{"status": map[string]interface{}{"equals": "satisfied"}}}, true},
		{"numeric json yaml equality", map[string]interface{}{"ci": map[string]interface{}{"attempt": map[string]interface{}{"equals": float64(2)}}}, true},
		{"in", map[string]interface{}{"ci": map[string]interface{}{"status": map[string]interface{}{"in": []interface{}{"pending", "satisfied"}}}}, true},
		{"not in", map[string]interface{}{"ci": map[string]interface{}{"status": map[string]interface{}{"not_in": []interface{}{"failed"}}}}, true},
		{"exists false", map[string]interface{}{"ci": map[string]interface{}{"missing": map[string]interface{}{"exists": false}}}, true},
		{"all", map[string]interface{}{"all": []interface{}{
			map[string]interface{}{"ci": map[string]interface{}{"status": "satisfied"}},
			map[string]interface{}{"ci": map[string]interface{}{"optional": false}},
		}}, true},
		{"any false", map[string]interface{}{"any": []interface{}{
			map[string]interface{}{"ci": map[string]interface{}{"status": "failed"}},
			map[string]interface{}{"ci": map[string]interface{}{"status": "pending"}},
		}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v2.MatchPredicate(tc.predicate, facts)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("matched = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMatchPredicateDottedKeysResolveNestedFacts(t *testing.T) {
	// Facts load from flat dotted keys (exec.last_run.succeeded) into nested
	// maps. Guards written with dotted predicate keys must resolve as paths.
	facts := map[string]interface{}{
		"exec": map[string]interface{}{
			"last_run": map[string]interface{}{
				"succeeded": true,
				"exit_code": float64(0),
			},
			"dependency_update": map[string]interface{}{
				"succeeded": true,
			},
		},
	}
	tests := []struct {
		name      string
		predicate map[string]interface{}
		want      bool
	}{
		{"dotted key with nested operator map", map[string]interface{}{
			"exec.last_run": map[string]interface{}{
				"succeeded": map[string]interface{}{"equals": true},
			}}, true},
		{"fully dotted key with operator", map[string]interface{}{
			"exec.last_run.succeeded": map[string]interface{}{"equals": true}}, true},
		{"fully dotted key wrong value", map[string]interface{}{
			"exec.last_run.exit_code": map[string]interface{}{"equals": float64(1)}}, false},
		{"dotted key mismatching object value", map[string]interface{}{
			"exec.last_run": map[string]interface{}{
				"succeeded": map[string]interface{}{"equals": false},
			}}, false},
		{"nested form still matches", map[string]interface{}{
			"exec": map[string]interface{}{
				"last_run": map[string]interface{}{
					"succeeded": map[string]interface{}{"equals": true},
				},
			}}, true},
		{"missing dotted path", map[string]interface{}{
			"exec.last_run.error": map[string]interface{}{"exists": true}}, false},
		{"dotted path stops at scalar", map[string]interface{}{
			"exec.last_run.succeeded.deeper": map[string]interface{}{"exists": true}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := v2.MatchPredicate(tc.predicate, facts)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("matched = %v, want %v", got, tc.want)
			}
		})
	}
}
