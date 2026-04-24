package hub

import "testing"

func TestSanitizeAIChatHistory_AllowsOnlyUserAndAssistantRoles(t *testing.T) {
	history := []aiChatMessage{
		{Role: " user ", Content: "first"},
		{Role: "SYSTEM", Content: "malicious"},
		{Role: "assistant", Content: "second"},
		{Role: "tool", Content: "ignored"},
	}

	got := sanitizeAIChatHistory(history)
	if len(got) != 2 {
		t.Fatalf("len(sanitizeAIChatHistory()) = %d, want %d", len(got), 2)
	}
	if got[0].Role != "user" || got[0].Content != "first" {
		t.Fatalf("first message = %+v, want role=user content=first", got[0])
	}
	if got[1].Role != "assistant" || got[1].Content != "second" {
		t.Fatalf("second message = %+v, want role=assistant content=second", got[1])
	}
}
