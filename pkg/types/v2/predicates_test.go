package v2

import "testing"

func TestClausesOverlapEqualsVsIn(t *testing.T) {
	a := map[string]interface{}{
		"conclusion": map[string]interface{}{"equals": "success"},
	}
	b := map[string]interface{}{
		"conclusion": map[string]interface{}{"in": []interface{}{"success", "failure"}},
	}
	ok, w := ClausesOverlap(a, b)
	if !ok {
		t.Fatal("expected overlap")
	}
	if w == nil || w.Value != "success" {
		t.Fatalf("witness = %#v, want success", w)
	}
}

func TestClausesOverlapDisjointEquals(t *testing.T) {
	a := map[string]interface{}{
		"conclusion": map[string]interface{}{"equals": "success"},
	}
	b := map[string]interface{}{
		"conclusion": map[string]interface{}{"equals": "failure"},
	}
	ok, _ := ClausesOverlap(a, b)
	if ok {
		t.Fatal("expected no overlap")
	}
}

func TestValidatePredicateTreeAllowsRestrictedOps(t *testing.T) {
	tree := map[string]interface{}{
		"all": []interface{}{
			map[string]interface{}{"pipeline": map[string]interface{}{"equals": "depot-container"}},
			map[string]interface{}{"conclusion": map[string]interface{}{"in": []interface{}{"failure", "cancelled"}}},
		},
	}
	if err := ValidatePredicateTree("when", tree); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePredicateTreeRejectsEmptyIn(t *testing.T) {
	tree := map[string]interface{}{
		"x": map[string]interface{}{"in": []interface{}{}},
	}
	if err := ValidatePredicateTree("when", tree); err == nil {
		t.Fatal("expected error for empty in list")
	}
}
