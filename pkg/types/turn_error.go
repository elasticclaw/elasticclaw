package types

// TurnErrorClass says what kind of failure ended a bridge-reported turn. It travels as
// the hub message's error_class next to the human-readable ⚠️ text.
type TurnErrorClass string

const (
	TurnErrorTransient           TurnErrorClass = "transient"            // intermittent provider failure; the session is intact (malformed tool-call JSON)
	TurnErrorModelTransport      TurnErrorClass = "model_transport"      // the model connection dropped or idled out
	TurnErrorHarnessConnectivity TurnErrorClass = "harness_connectivity" // the bridge could not talk to the local harness
	TurnErrorContextOverflow     TurnErrorClass = "context_overflow"
	TurnErrorSessionLost         TurnErrorClass = "session_lost"
	TurnErrorAuth                TurnErrorClass = "auth"
	TurnErrorQuota               TurnErrorClass = "quota"
	TurnErrorFatal               TurnErrorClass = "fatal"
)
