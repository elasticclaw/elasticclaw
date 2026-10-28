package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestClassifyBridgeArgs(t *testing.T) {
	for _, tt := range []struct {
		args      []string
		command   string
		bootstrap bool
	}{
		{nil, "daemon", false},
		{[]string{"--bootstrap"}, "daemon", true},
		{[]string{"help"}, "help", false},
		{[]string{"--help"}, "help", false},
		{[]string{"-h"}, "help", false},
		{[]string{"version"}, "version", false},
		{[]string{"--version"}, "version", false},
		{[]string{"-v"}, "version", false},
		{[]string{"linear", "--help"}, "linear", false},
		{[]string{"control", "status"}, "control", false},
		{[]string{"daemon"}, "invalid", false},
		{[]string{"--unknown"}, "invalid", false},
		{[]string{"--bootstrap", "--unknown"}, "invalid", false},
		{[]string{"version", "--unknown"}, "invalid", false},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			command, bootstrap := classifyBridgeArgs(tt.args)
			if command != tt.command || bootstrap != tt.bootstrap {
				t.Fatalf("classify(%v) = %q, %v; want %q, %v", tt.args, command, bootstrap, tt.command, tt.bootstrap)
			}
		})
	}
}

func TestBridgeCLIExitsBeforeBootstrap(t *testing.T) {
	if os.Getenv("BRIDGE_CLI_TEST_HELPER") == "1" {
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{"claw-bridge"}, os.Args[i+1:]...)
				main()
				os.Exit(0)
			}
		}
		os.Exit(99)
	}
	for _, arg := range []string{"help", "--help", "-h", "--unknown"} {
		t.Run(arg, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestBridgeCLIExitsBeforeBootstrap$", "--", arg)
			cmd.Env = append(os.Environ(), "BRIDGE_CLI_TEST_HELPER=1", "ELASTICCLAW_BOOTSTRAP=1", "ELASTICCLAW_HUB_URL=", "ELASTICCLAW_CLAW_ID=", "ELASTICCLAW_CLAW_TOKEN=")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if arg == "--unknown" {
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
					t.Fatalf("invalid argument exit = %v, want 2", err)
				}
				if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage:") {
					t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
				}
			} else if err != nil || stderr.Len() != 0 || !strings.Contains(stdout.String(), "sandbox supervisor") {
				t.Fatalf("help: err=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
		})
	}
}

func assertTurnHeartbeat(t *testing.T, q *msgQueue, want bool) {
	t.Helper()
	payload := map[string]interface{}{}
	q.addTurnHeartbeat(payload)
	var heartbeat struct {
		TurnActive *bool `json:"turn_active"`
	}
	if err := json.Unmarshal(mustJSON(payload), &heartbeat); err != nil || heartbeat.TurnActive == nil || *heartbeat.TurnActive != want {
		t.Fatalf("heartbeat=%s, err=%v, want turn_active=%v", mustJSON(payload), err, want)
	}
}

func TestTurnHeartbeatIncludesQueuedAndReplayedInput(t *testing.T) {
	q := &msgQueue{}
	assertTurnHeartbeat(t, q, false)
	q.pushInput("queued", "work while gateway is not ready")
	assertTurnHeartbeat(t, q, true)
	replayQueued(q, func(string, string) error { return nil }, func(hubMsg) error { return nil }, func(id, content string) {
		if id != "queued" {
			t.Fatalf("unexpected replay %q", id)
		}
		assertTurnHeartbeat(t, q, true)
		// The replay handler owns a reference before the queue releases its own.
		q.activeTurns.Add(1)
	})
	assertTurnHeartbeat(t, q, true)
	q.activeTurns.Add(-1)
	q.pushReply("completed reply awaiting redelivery")
	assertTurnHeartbeat(t, q, false)
}

func TestTurnHeartbeatReleasesDroppedInputs(t *testing.T) {
	q := &msgQueue{}
	for i := 0; i < msgQueueMax+1; i++ {
		q.pushInput("queued", "pending")
	}
	if got := q.activeTurns.Load(); got != int64(msgQueueMax) {
		t.Fatalf("active turns after overflow = %d, want %d", got, msgQueueMax)
	}
	for i := range q.msgs {
		q.msgs[i].queuedAt = time.Now().Add(-msgQueueTTL - time.Second)
	}
	q.drain()
	assertTurnHeartbeat(t, q, false)
}
