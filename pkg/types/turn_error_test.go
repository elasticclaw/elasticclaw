package types

import (
	"encoding/json"
	"testing"
)

func TestHubMessageErrorClass(t *testing.T) {
	for _, class := range []TurnErrorClass{"", TurnErrorModelTransport, TurnErrorClass("future_class")} {
		data, err := json.Marshal(HubMessage{Content: "reply", ErrorClass: class})
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		if _, present := fields["error_class"]; present != (class != "") {
			t.Fatalf("error_class presence for %q: %s", class, data)
		}
		var message HubMessage
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatal(err)
		}
		if message.ErrorClass != class {
			t.Fatalf("round trip class = %q, want %q", message.ErrorClass, class)
		}
	}
}
