package workflowsetup

import (
	"strings"
	"testing"
)

func TestValidateSlugAcceptsAllowedPattern(t *testing.T) {
	tests := []string{
		"a",
		"0",
		"abc",
		"abc-123",
		"abc_123",
		"a" + strings.Repeat("b", 62),
	}

	for _, name := range tests {
		if err := ValidateSlug(name); err != nil {
			t.Fatalf("ValidateSlug(%q): %v", name, err)
		}
	}
}

func TestValidateSlugRejectsDisallowedPattern(t *testing.T) {
	tests := []string{
		"",
		"A",
		"-abc",
		"_abc",
		"abc.",
		"abc def",
		"abc/def",
		"a" + strings.Repeat("b", 63),
	}

	for _, name := range tests {
		if err := ValidateSlug(name); err == nil {
			t.Fatalf("ValidateSlug(%q) succeeded, want error", name)
		}
	}
}

func TestSlugCandidateLowercasesAndKeepsAllowedCharacters(t *testing.T) {
	got := SlugCandidate(" My Workflow_01! ")
	want := "my-workflow_01"
	if got != want {
		t.Fatalf("SlugCandidate = %q, want %q", got, want)
	}
	if err := ValidateSlug(got); err != nil {
		t.Fatalf("ValidateSlug(%q): %v", got, err)
	}
}
