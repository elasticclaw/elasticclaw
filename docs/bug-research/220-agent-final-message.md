# Bug Research: Agent Not Sending Final Messages (Issue #220)

## Research Summary

This document contains the research findings for issue #220, where agents sometimes complete work but fail to send the final `[DONE]` signal to the hub.

## Research Comment

The full research has been posted as a comment on issue #220:
https://github.com/elasticclaw/elasticclaw/issues/220#issuecomment-4462957133

## Key Findings

### Potential Failure Modes

1. **WebSocket Write Failure on Final Message** (pkg/hub/server.go:1716-1718)
   - The `[DONE]` detection happens AFTER the message is broadcast to users
   - If the claw disconnects between sending the message and the hub processing it, the `handleClawDoneSignal` goroutine may not complete

2. **Streaming State Race Condition** (web/hooks/use-hub.ts:224-248)
   - The `finalizeTypewriter` callback commits messages to state only after typewriter drains
   - If the claw disconnects or the WS closes during streaming, the final message may never be committed

3. **Bridge Message Queue Behavior** (cmd/claw-bridge/main.go:1616-1640)
   - When the bridge sends a message to the hub, if the write fails, it queues the response for replay
   - However, the queue only replays on reconnect — if the claw is terminated before reconnect, the message is lost

4. **No Explicit "Final Message" Acknowledgment**
   - The hub detects `[DONE]` via string matching on message content
   - If the message content is truncated or malformed during transmission, the pattern won't match

### Root Cause Analysis

The most likely root causes, in order of probability:

1. **Race between message delivery and claw termination** — The claw may be terminated before the final message is fully processed
2. **WebSocket connection instability** — If the bridge→hub WebSocket drops during final message send
3. **Typewriter finalization race** — In the web UI, if the claw disconnects before the typewriter drains

### Fix Options

1. **Add explicit message acknowledgment** — Have the hub send an ACK when it receives and processes a `[DONE]` signal
2. **Store pending DONE signals in DB** — When a claw sends `[DONE]`, immediately store a "pending_done" record
3. **Improve bridge message reliability** — Ensure the bridge waits for hub ACK before considering a message delivered
4. **Add logging/metrics** — Add structured logging around `[DONE]` detection to confirm the failure mode

**Recommended approach:** Option 4 first (to confirm the failure mode), then Option 1 (explicit ACK) for the actual fix.

## Confidence Summary

| Aspect | Confidence | Notes |
|--------|------------|-------|
| **Bug exists** | High | Reporter observation + code analysis confirms multiple failure paths |
| **Root cause understood** | Medium | Multiple potential causes; need metrics/logging to confirm |
| **Fix approach** | Medium | Options are viable but need validation first |
| **Risk of proposed fix** | Low-Medium | All options are relatively safe |

---
*Research conducted by bug-research agent on 2026-05-15*
