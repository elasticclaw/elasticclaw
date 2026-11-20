package main

import "github.com/elasticclaw/elasticclaw/pkg/types"

// errorReplyMessage builds the hub message for a failed turn: the same payload
// as an ordinary reply plus error_class.
func errorReplyMessage(role, content string, class types.TurnErrorClass) hubMsg {
	return hubMsg{
		Type:    "message",
		Payload: mustJSON(map[string]interface{}{"role": role, "content": content, "error_class": class}),
	}
}
