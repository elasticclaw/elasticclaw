package main

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestHarnessRunTurnForwardsEvents(t *testing.T) {
	for _, method := range []string{"SendMessage", "RunTurn"} {
		t.Run(method, func(t *testing.T) {
			gs := startLifecycleEndGateway(t, map[string]interface{}{"phase": "end"})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var chunks []string
			var activities []agentActivity
			sink := EventSink{
				OnChunk:    func(chunk string) { chunks = append(chunks, chunk) },
				OnActivity: func(activity agentActivity) { activities = append(activities, activity) },
			}
			var reply string
			var err error
			if method == "SendMessage" {
				reply, err = gs.SendMessage(ctx, "repair", "turn-1", sink.OnChunk, sink.OnActivity)
			} else {
				var h Harness = gs
				reply, err = h.RunTurn(ctx, "repair", "turn-1", sink)
			}
			if err != nil || reply != "partial" {
				t.Fatalf("%s = %q, %v; want completed reply", method, reply, err)
			}
			if !reflect.DeepEqual(chunks, []string{"partial"}) {
				t.Fatalf("chunks = %v; want [partial]", chunks)
			}
			wantActivities := []agentActivity{{Kind: "model_started", Message: "Waiting on model response"}}
			if !reflect.DeepEqual(activities, wantActivities) {
				t.Fatalf("activities = %+v; want %+v", activities, wantActivities)
			}
		})
	}
}
