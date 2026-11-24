package main

import (
	"errors"

	"github.com/elasticclaw/elasticclaw/pkg/harness/openclaw"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// errorReplyMessage builds the hub message for a failed turn: the same payload
// as an ordinary reply plus error_class.
func errorReplyMessage(role, content string, class types.TurnErrorClass) hubMsg {
	return hubMsg{
		Type:    "message",
		Payload: mustJSON(map[string]interface{}{"role": role, "content": content, "error_class": class}),
	}
}

// turnErrorClass returns the class the harness attached to a failed turn and
// classifies the text only when the error carries none. A usage limit is a
// quota only when the failure is definite, so a replayed error re-checks the
// text for the non-definite class.
func turnErrorClass(err error, definite bool) types.TurnErrorClass {
	var te *TurnError
	if errors.As(err, &te) && te.Class != "" && (definite || te.Class != types.TurnErrorQuota) {
		return te.Class
	}
	return openclaw.ClassifyErrorText(err.Error(), definite)
}
