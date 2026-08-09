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
