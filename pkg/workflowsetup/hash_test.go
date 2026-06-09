package workflowsetup

import (
	"strings"
	"testing"
)

func TestConfigHashIsDeterministicSHA256(t *testing.T) {
	config := "name: demo\n"

	first := ConfigHash(config)
	second := ConfigHash(config)

	if first != second {
		t.Fatalf("ConfigHash returned %q then %q for the same config", first, second)
	}
	want := "sha256:8789e7eabb7ba5922a5087c25d315a1e5fbb6b0f97510862579d348428dbd9d4"
	if first != want {
		t.Fatalf("ConfigHash = %q, want %q", first, want)
	}
	if !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("ConfigHash = %q, want sha256 prefix", first)
	}
	if len(strings.TrimPrefix(first, "sha256:")) != 64 {
		t.Fatalf("ConfigHash = %q, want 64 hex chars after prefix", first)
	}
}

func TestConfigHashChangesWhenConfigChanges(t *testing.T) {
	if ConfigHash("name: demo\n") == ConfigHash("name: other\n") {
		t.Fatal("ConfigHash returned the same hash for different configs")
	}
}
