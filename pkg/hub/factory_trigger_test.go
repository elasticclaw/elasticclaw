package hub

import (
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestValidateFactoryInputs_MinMax(t *testing.T) {
	min1 := 1.0
	max10 := 10.0

	tests := []struct {
		name      string
		inputs    []types.FactoryInput
		values    map[string]interface{}
		wantErr   bool
		errSubstr string
	}{
		{
			name: "negative below min",
			inputs: []types.FactoryInput{
				{Name: "pr_number", Type: "number", Required: true, Min: &min1},
			},
			values: map[string]interface{}{
				"pr_number": -3.0,
			},
			wantErr:   true,
			errSubstr: "below minimum",
		},
		{
			name: "zero below min",
			inputs: []types.FactoryInput{
				{Name: "pr_number", Type: "number", Required: true, Min: &min1},
			},
			values: map[string]interface{}{
				"pr_number": 0.0,
			},
			wantErr:   true,
			errSubstr: "below minimum",
		},
		{
			name: "valid at min",
			inputs: []types.FactoryInput{
				{Name: "pr_number", Type: "number", Required: true, Min: &min1},
			},
			values: map[string]interface{}{
				"pr_number": 1.0,
			},
			wantErr: false,
		},
		{
			name: "valid above min",
			inputs: []types.FactoryInput{
				{Name: "pr_number", Type: "number", Required: true, Min: &min1},
			},
			values: map[string]interface{}{
				"pr_number": 5.0,
			},
			wantErr: false,
		},
		{
			name: "above max",
			inputs: []types.FactoryInput{
				{Name: "rating", Type: "number", Required: true, Max: &max10},
			},
			values: map[string]interface{}{
				"rating": 15.0,
			},
			wantErr:   true,
			errSubstr: "above maximum",
		},
		{
			name: "valid within range",
			inputs: []types.FactoryInput{
				{Name: "rating", Type: "number", Required: true, Min: &min1, Max: &max10},
			},
			values: map[string]interface{}{
				"rating": 5.0,
			},
			wantErr: false,
		},
		{
			name: "string number below min",
			inputs: []types.FactoryInput{
				{Name: "pr_number", Type: "number", Required: true, Min: &min1},
			},
			values: map[string]interface{}{
				"pr_number": "-3",
			},
			wantErr:   true,
			errSubstr: "below minimum",
		},
		{
			name: "string number valid",
			inputs: []types.FactoryInput{
				{Name: "pr_number", Type: "number", Required: true, Min: &min1},
			},
			values: map[string]interface{}{
				"pr_number": "5",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateFactoryInputs(tt.inputs, tt.values)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errSubstr)
				}
				if tt.errSubstr != "" && !contains(err.Error(), tt.errSubstr) {
					t.Fatalf("expected error containing %q, got: %v", tt.errSubstr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
