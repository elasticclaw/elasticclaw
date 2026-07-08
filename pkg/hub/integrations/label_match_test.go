package integrations

import "testing"

func TestLabelsAllowed(t *testing.T) {
	tests := []struct {
		name     string
		current  []string
		required []string
		excluded []string
		want     bool
	}{
		{
			name:     "required labels all present",
			current:  []string{"agent-ready", "backend"},
			required: []string{"agent-ready"},
			want:     true,
		},
		{
			name:     "missing required label fails",
			current:  []string{"backend"},
			required: []string{"agent-ready"},
			want:     false,
		},
		{
			name:     "excluded label absent passes",
			current:  []string{"agent-ready"},
			excluded: []string{"Bug"},
			want:     true,
		},
		{
			name:     "excluded label present fails",
			current:  []string{"agent-ready", "bug"},
			excluded: []string{"Bug"},
			want:     false,
		},
		{
			name:     "matching is case-insensitive and trims whitespace",
			current:  []string{" Agent-Ready ", "BUG"},
			required: []string{"agent-ready"},
			excluded: []string{" bug "},
			want:     false,
		},
		{
			name:     "same label required and excluded does not match",
			current:  []string{"bug"},
			required: []string{"Bug"},
			excluded: []string{" bug "},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := labelsAllowed(tt.current, tt.required, tt.excluded); got != tt.want {
				t.Fatalf("labelsAllowed(%v, %v, %v) = %v, want %v", tt.current, tt.required, tt.excluded, got, tt.want)
			}
		})
	}
}
