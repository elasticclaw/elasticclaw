package main

import (
	"fmt"
	"io"
)

func classifyBridgeArgs(args []string) (command string, bootstrap bool) {
	if len(args) == 0 {
		return "daemon", false
	}
	switch args[0] {
	case "linear", "control":
		return args[0], false
	case "help", "--help", "-h":
		if len(args) == 1 {
			return "help", false
		}
	case "version", "--version", "-v":
		if len(args) == 1 {
			return "version", false
		}
	default:
		for _, arg := range args {
			if arg != "--bootstrap" {
				return "invalid", false
			}
		}
		return "daemon", true
	}
	return "invalid", false
}

func printBridgeUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage: claw-bridge [--bootstrap]
       claw-bridge version | linear [args...] | control [args...]
       claw-bridge help | --help | -h

--bootstrap  Set up the sandbox before starting the daemon.
The daemon is started by the sandbox supervisor and must not be run by hand.`)
}

func (q *msgQueue) addTurnHeartbeat(payload map[string]interface{}) {
	payload["turn_active"] = q.activeTurns.Load() > 0
}
