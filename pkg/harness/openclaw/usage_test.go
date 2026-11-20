package openclaw

import (
	"math"
	"reflect"
	"testing"
)

func usagePtr[T any](v T) *T { return &v }

func TestUsageAccumulator(t *testing.T) {
	a := NewUsageAccumulator("epoch")
	for _, tc := range []struct {
		name, session   string
		in, out         *int
		cost            *float64
		wantIn, wantOut int
		wantCost        *float64
	}{
		{"first", "a", usagePtr(100), usagePtr(20), usagePtr(.5), 100, 20, usagePtr(.5)},
		{"repeated identical run", "a", usagePtr(100), usagePtr(20), usagePtr(.5), 100, 20, usagePtr(.5)},
		{"cost correction", "a", nil, nil, usagePtr(.7), 100, 20, usagePtr(.7)},
		{"nil cost", "a", nil, nil, nil, 100, 20, nil},
		{"new run", "a", usagePtr(200), usagePtr(30), usagePtr(.8), 300, 50, usagePtr(1.5)},
		{"missing input", "a", nil, usagePtr(40), nil, 500, 90, nil},
		{"late price", "a", nil, nil, usagePtr(.9), 500, 90, usagePtr(2.4)},
		{"unpriced new run", "a", usagePtr(300), usagePtr(50), usagePtr(0.0), 800, 140, usagePtr(2.4)},
		{"new session", "b", nil, usagePtr(10), usagePtr(.2), 0, 10, usagePtr(.2)},
		{"previous session resumes its totals", "a", usagePtr(100), nil, nil, 900, 190, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := a.Observe(tc.session, tc.in, tc.out, tc.cost, "model", "provider", usagePtr(70), usagePtr(8))
			if s.Epoch != "epoch" || s.SessionKey != tc.session || s.Model != "model" || s.ModelProvider != "provider" || *s.CacheReadTokens != 70 || *s.CacheWriteTokens != 8 {
				t.Fatalf("metadata = %+v", s)
			}
			if s.InputTokens == nil || s.OutputTokens == nil || *s.InputTokens != tc.wantIn || *s.OutputTokens != tc.wantOut {
				t.Fatalf("totals = %+v, want %d/%d", s, tc.wantIn, tc.wantOut)
			}
			if (s.CostUSD == nil) != (tc.wantCost == nil) || (s.CostUSD != nil && math.Abs(*s.CostUSD-*tc.wantCost) > 1e-9) {
				t.Fatalf("cost = %v, want %v", s.CostUSD, tc.wantCost)
			}
			if repeated := a.Observe(tc.session, tc.in, tc.out, tc.cost, "model", "provider", usagePtr(70), usagePtr(8)); !reflect.DeepEqual(s, repeated) {
				t.Fatalf("repeated sample changed snapshot: %+v / %+v", s, repeated)
			}
		})
	}
}

func TestUsageAccumulatorEmptyAndSnapshotOwnership(t *testing.T) {
	a := NewUsageAccumulator("epoch")
	empty := a.Observe("a", nil, nil, nil, "", "", nil, nil)
	if empty.InputTokens != nil || empty.OutputTokens != nil || empty.CostUSD != nil {
		t.Fatalf("empty sample = %+v", empty)
	}
	in, cost := 100, .5
	first := a.Observe("a", &in, nil, &cost, "", "", nil, nil)
	in, cost = 200, .8
	second := a.Observe("a", &in, nil, &cost, "", "", nil, nil)
	if *first.InputTokens != 100 || *first.OutputTokens != 0 || *first.CostUSD != .5 || *second.InputTokens != 300 {
		t.Fatalf("snapshots changed after observation: %+v / %+v", first, second)
	}
	reset := a.Observe("b", nil, nil, nil, "", "", nil, nil)
	if reset.InputTokens != nil || reset.OutputTokens != nil {
		t.Fatalf("new session retained token fields: %+v", reset)
	}
}
