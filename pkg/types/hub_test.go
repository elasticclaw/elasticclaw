package types

import (
	"encoding/json"
	"testing"
)

func TestRegisterPayloadReady(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    string
	}{
		{`{}`, "null"},
		{`{"gateway_ready":true}`, "true"},
		{`{"gateway_ready":false}`, "false"},
		{`{"harness_ready":true}`, "true"},
		{`{"harness_ready":false}`, "false"},
		{`{"harness_ready":false,"gateway_ready":true}`, "false"},
		{`{"harness_ready":true,"gateway_ready":false}`, "true"},
		{`{"harness_ready":null,"gateway_ready":false}`, "false"},
	} {
		t.Run(tc.payload, func(t *testing.T) {
			var p RegisterPayload
			if err := json.Unmarshal([]byte(tc.payload), &p); err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(p.Ready())
			if err != nil || string(got) != tc.want {
				t.Fatalf("Ready() = %s, %v; want %s", got, err, tc.want)
			}
		})
	}
}
