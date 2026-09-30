package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReservedAndResultCap(t *testing.T) {
	for _, name := range []string{EmitBlock, AskInterview, OfferActions, SetPanel} {
		if !Reserved(name) {
			t.Fatal(name)
		}
	}
	registry, err := NewReadRegistry()
	if err != nil || len(registry.Definitions()) != 0 {
		t.Fatal(registry, err)
	}
	for _, text := range []string{"short", strings.Repeat("a", 40000), strings.Repeat("界", 20000)} {
		got := CapResult(text)
		if len(got) > MaxResultBytes || !utf8.ValidString(got) {
			t.Fatalf("invalid truncation: %d", len(got))
		}
		if len(text) <= MaxResultBytes && text != got {
			t.Fatal("short value changed")
		}
	}
	if got := CapStored(strings.Repeat("a", MaxStoredResultBytes+1)); len(got) > MaxStoredResultBytes || !strings.HasSuffix(got, "[truncated]") {
		t.Fatalf("stored result not capped: %d", len(got))
	}
}
