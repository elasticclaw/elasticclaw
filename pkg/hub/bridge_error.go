package hub

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/pipeline"
	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// Bridge transport errors (NEXT-725).
//
// claw-bridge answers a failed turn with the error text as the turn body (see
// types.BridgeTransportError). The hub read that as the agent talking, and the
// initial-plan gate — which only asks "is this message long enough to be a
// real attempt?" — answered every one of them with another correction. On claw
// 1572c4e4 the sandbox disk filled up: the ENOSPC reply was 158 characters,
// comfortably over the gate's 120-character floor, so the hub re-sent the
// correction, the bridge burned its full 60-minute turn cap failing again, and
// the pair looped with the agent never producing a single word of its own.
//
// No existing recovery path could break it. claw_retry waits for an unhealthy
// gateway (the gateway was fine — the DISK was full), the busy-turn watchdog
// fires at 70 minutes but the bridge closes the turn at 60, idle auto-resume
// requires no turn in flight, and the no-progress watchdog needed three
// look-alike turns at an hour each. Worse, the word ENOSPC never reached the
// hub's journal at all, so nobody could have known without opening the claw's
// chat.
//
// bridgeErrorPauseThreshold is deliberately low. Every cycle costs up to an
// hour, and the pause is cheap and reversible: a human sends any message and
// resumeNoProgressAfterUserInput lifts it. A single error is genuinely
// transient sometimes ("LLM request failed: network connection error", after
// which the agent carried on), so 1 would stop healthy claws; 2 costs at most
// one wasted cycle and still catches a stuck sandbox on its second try. The
// streak resets on any real turn, so an occasional error never accumulates.
const bridgeErrorPauseThreshold = 2

// Recovery from a transient gateway hang (NEXT-1293). The pause above assumed a
// human must fix the sandbox, but a gateway that hung for a minute under a heavy
// process recovers by itself, and the latch then sat for ~3h until someone
// typed. A pause whose triggering error was a gateway connectivity failure is
// tagged bridge_error_pause_kind='gateway'. It is lifted automatically once
// heartbeats report the gateway healthy AND ready continuously for
// bridgeErrorRecoveryHealthyFor, counted from after the pause, at most
// bridgeErrorAutoResumeMax times per unit of work. The counter resets only on a
// stage transition (claimPipelineStageTransition) and a sandbox replacement
// (resetClawForRetry). It deliberately survives a session loss, unlike
// idle_resume_count: a gateway hang is exactly what rotates the session, so
// resetting there would refund the budget on every flap. The cap preserves the
// NEXT-725 guarantee: a sandbox that keeps failing still stops and then waits
// for a human. Every other error (ENOSPC, "Agent run failed", credential or
// handshake rejections) is never auto-lifted, and neither is a gateway pause
// carried across a sandbox replacement (resetClawForRetry clears the kind).
const (
	bridgeErrorPauseKindGateway   = "gateway"
	bridgeErrorRecoveryHealthyFor = 3 * time.Minute
	bridgeErrorAutoResumeMax      = 2
)

// bridgeErrorIsGatewayConnectivity matches the claw-bridge strings that mean
// "could not talk to the local gateway" (cmd/claw-bridge/main.go). The
// reconnect wrapper ("...; gateway reconnect failed: <cause>") also carries
// causes that are not a hang: the gateway answering the handshake with a
// rejection, or a credential/disk failure. Those need a human, so any
// non-transient marker anywhere in the text wins over a connectivity marker.
func bridgeErrorIsGatewayConnectivity(errText string) bool {
	lower := strings.ToLower(errText)
	for _, marker := range []string{
		"connect rejected", "unauthorized", "forbidden", "auth", "credential",
		"no space left", "enospc", "agent run failed", "usage limit",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}
	for _, marker := range []string{"gateway disconnected", "dial gateway", "gateway reconnect failed"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// observeGatewayHealthLocked extends or breaks the healthy+ready run. Caller
// holds cc.mu.
func (cc *clawConn) observeGatewayHealthLocked(healthyAndReady bool) {
	if !healthyAndReady {
		cc.gatewayHealthySince = time.Time{}
	} else if cc.gatewayHealthySince.IsZero() {
		cc.gatewayHealthySince = now()
	}
}

// bridgeErrorLiftDue reports whether cc's pause is a gateway pause whose
// gateway has now been healthy and ready for the whole recovery window.
func (cc *clawConn) bridgeErrorLiftDue() bool {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	return cc.noProgressPaused && cc.bridgeErrorPauseGateway &&
		!cc.gatewayHealthySince.IsZero() && now().Sub(cc.gatewayHealthySince) >= bridgeErrorRecoveryHealthyFor
}

// maybeLiftBridgeErrorPause runs on every bridge heartbeat (the one place that
// already sees gateway health per connection, so no extra ticker or DB scan).
// The in-memory pre-check keeps the common case free of DB work and of
// noProgressMu. Everything after it runs under noProgressMu, which the pause,
// the human resume AND connection registration also hold (handleClawWS reads
// the persisted latch and publishes the new clawConn inside it). So the
// connection re-checked and cleared here is the live one: a bridge that
// reconnects either registers before the lift (and is the connection re-checked
// here, with its own fresh healthy window) or after it (and reads the cleared
// row), never in between with a stale paused copy.
func (s *Server) maybeLiftBridgeErrorPause(clawID string) {
	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc == nil || !cc.bridgeErrorLiftDue() {
		return
	}

	s.noProgressMu.Lock()
	s.mu.RLock()
	cc = s.claws[clawID]
	s.mu.RUnlock()
	if cc == nil || !cc.bridgeErrorLiftDue() {
		s.noProgressMu.Unlock()
		return
	}
	res, err := s.db.Exec(`UPDATE claws SET no_progress_paused=0, bridge_error_pause_kind='', bridge_error_auto_resumes=bridge_error_auto_resumes+1
		WHERE id=? AND no_progress_paused!=0 AND bridge_error_pause_kind=? AND bridge_error_auto_resumes<?`,
		clawID, bridgeErrorPauseKindGateway, bridgeErrorAutoResumeMax)
	if err != nil {
		s.noProgressMu.Unlock()
		log.Printf("[bridge-error] auto-resume claw %s: %v", shortID(clawID), err)
		return
	}
	if changed, _ := res.RowsAffected(); changed == 0 {
		s.noProgressMu.Unlock()
		return
	}
	cc.mu.Lock()
	cc.noProgressPaused = false
	cc.bridgeErrorPauseGateway = false
	cc.bridgeErrorStreak = 0
	cc.mu.Unlock()
	var n int
	_ = s.db.QueryRow(`SELECT bridge_error_auto_resumes FROM claws WHERE id=?`, clawID).Scan(&n)
	s.noProgressMu.Unlock()

	log.Printf("[bridge-error] claw %s: gateway healthy for 3m after transport-error pause, resuming automatically (%d/%d)",
		shortID(clawID), n, bridgeErrorAutoResumeMax)
	s.injectHubMessageByID(clawID, "[hub] The sandbox gateway recovered after transport errors interrupted your last turns. Nothing in the workspace was lost. Recover your state from the workspace (git status, git log --oneline -15, the current branch, any uncommitted changes) and continue the current stage without starting over.")
}

// bridgeErrorNoticeLimit caps how much of the error we repeat into the
// dashboard notice and the operator notification. The useful part ("no space
// left on device") is at the front; a gateway stack trace is not.
const bridgeErrorNoticeLimit = 400

// observeBridgeErrorTurn records one bridge-error turn for a claw and reports
// whether automatic continuation is paused as a result. The caller must route
// bridge-error turns here INSTEAD of observeCompletedTurn: this is not an
// agent outcome, and letting it into claw_turn_observations would let a single
// transport blip reset the repeated-outcome chain the no-progress watchdog is
// counting.
func (s *Server) observeBridgeErrorTurn(cc *clawConn, clawID, errText string, definite bool) bool {
	streak := 0
	alreadyPaused := false
	if cc != nil {
		cc.mu.Lock()
		// Only the bridge's own label counts toward stopping the claw; the
		// generic replay label still silences the plan gate. See
		// types.BridgeTransportErrorIsDefinite.
		if definite {
			cc.bridgeErrorStreak++
		}
		streak = cc.bridgeErrorStreak
		alreadyPaused = cc.noProgressPaused
		cc.mu.Unlock()
	}
	// Logged on EVERY bridge-error turn, not just at the threshold, and with
	// the error verbatim: the incident's root cause was one grep away
	// (`journalctl | grep ENOSPC` returned 0 matches) and nobody could reach it.
	log.Printf("[bridge-error] claw %s: turn failed in the bridge transport (%d in a row), agent never ran: %s",
		shortID(clawID), streak, errText)
	if alreadyPaused {
		return true
	}
	short := truncateRunes(errText, bridgeErrorNoticeLimit)
	// The WARNING is deliberately not gated on the pause threshold. Reaching the
	// threshold needs a SECOND turn, and nothing here guarantees one: with the
	// plan gate no longer answering, the next turn only exists if the idle
	// auto-resume fires — which is off when liveness.idle_resume is disabled,
	// when the claw is ineligible, or once its per-work-unit resume cap is spent. Tie
	// the alarm to that and a claw whose transport is dead can sit silent
	// forever, which is the failure this whole change exists to end. So: tell
	// the operator on the first one, stop the loop on the second.
	if streak == 1 {
		s.notifyBridgeError(clawID, short)
	}
	if streak < bridgeErrorPauseThreshold {
		return false
	}
	kind := ""
	if bridgeErrorIsGatewayConnectivity(errText) {
		kind = bridgeErrorPauseKindGateway
	}
	notice := fmt.Sprintf("[hub] Automatic continuation paused: the last %d turns never reached the agent — claw-bridge returned a transport error instead. Last error: %s. This is the sandbox or the gateway failing, not the agent's output; fix it and send a message to resume.", streak, short)
	if !s.pauseAutomaticContinuation(clawID, notice, kind) {
		// Someone else (or an earlier tick) already latched this claw. Still
		// paused — just not by us, so no second notification.
		return true
	}
	log.Printf("[bridge-error] claw %s: paused automatic continuation after %d consecutive bridge transport errors: %s",
		shortID(clawID), streak, errText)
	s.notifyBridgeErrorPause(clawID, streak, short)
	return true
}

// notifyBridgeErrorPause delivers the pause to the operator over the existing
// agent_idle lifecycle event.
//
// Reusing agent_idle rather than minting an event type is deliberate: the
// operator-visible fact is identical ("this agent is not moving and nobody
// told you"), the response is identical (go look), and agent_idle already has
// the toggle, the severity, the run-backed notifier pass and the claw pass
// wired up. A new type would mean a new toggle, a settings migration and a new
// row in every notification-config test to express a distinction the message
// body already carries — the same trade already argued for noProgressPaused,
// which is folded into this event for exactly these reasons.
//
// Ad-hoc (non run-backed) claws get no event here: their delivery pass keys on
// the claws.idle_since latch, which means "no turn finished for N minutes" and
// is untrue for a claw that is finishing a failed turn every hour. Faking it
// would misreport the duration in the message. Those claws are covered by the
// dashboard notice and the log line above.
func (s *Server) notifyBridgeErrorPause(clawID string, streak int, errText string) {
	s.recordBridgeErrorEvent(clawID, streak, errText, true)
}

// notifyBridgeError is the same alarm without a pause: the transport failed,
// and the operator hears about it before we know whether it will repeat.
func (s *Server) notifyBridgeError(clawID, errText string) {
	s.recordBridgeErrorEvent(clawID, 1, errText, false)
}

func (s *Server) recordBridgeErrorEvent(clawID string, streak int, errText string, paused bool) {
	if err := s.recordTaskRunEventForClaw(clawID, TaskRunEvent{
		// Keyed on the pause instant, so a second episode after a human
		// resumes is a second notification instead of dedupe-swallowed.
		EventKey:        taskRunEventAgentIdle + ":bridge-error:" + strconv.Itoa(streak) + ":" + strconv.FormatInt(epochMillis(now()), 10),
		Source:          taskRunSourceHub,
		EventType:       taskRunEventAgentIdle,
		ActorType:       taskRunActorSystem,
		InteractionRole: taskRunInteractionNeutral,
		Detail: map[string]any{
			"bridgeError":      errText,
			"bridgeErrorTurns": streak,
			"noProgressPaused": paused,
		},
		OccurredAt: now(),
	}); err != nil {
		log.Printf("[bridge-error] record notification event for claw %s: %v", shortID(clawID), err)
	}
}

// observeTurnOutcome routes one completed turn to the consumer that owns it
// and reports whether automatic continuation is now paused, plus whether the
// turn was a bridge transport error.
//
// The split is the whole fix. A bridge-error turn deliberately never reaches
// observeCompletedTurn: it is not an agent outcome, and letting it into
// claw_turn_observations would let one transport blip reset the
// repeated-outcome chain the no-progress watchdog is counting — the watchdog
// would then need three MORE look-alike turns, at up to an hour each.
// Conversely, any turn the agent actually authored proves the transport works,
// so it clears the bridge-error streak.
func (s *Server) observeTurnOutcome(cc *clawConn, clawID, messageID, content string) (paused bool, bridgeErrTurn bool) {
	errText, definite, bridgeErrTurn := types.BridgeTransportErrorIsDefinite(content)
	if bridgeErrTurn {
		// A provider usage limit arrives on this path but is not this path's
		// problem. The transport worked — it delivered the rejection — and the
		// bridge-error response (pause, then tell the operator the sandbox or
		// the gateway is failing) would hand them a diagnosis that is simply
		// untrue and no way to act on the real one. Route it to the limit
		// handler, which knows the block ends on a schedule.
		//
		// Only the bridge's OWN label counts, for the reason
		// BridgeTransportErrorIsDefinite exists: the generic "⚠️ error:" prefix
		// is short enough for an agent to open a turn with while REPORTING a
		// provider message it saw, and this branch parks every claw on the key.
		// A claw talking about a cap must not be able to declare one.
		if limit, ok := types.ParseLLMUsageLimit(errText); ok && definite {
			s.handleLLMUsageLimit(cc, clawID, limit)
			return true, true
		}
		return s.observeBridgeErrorTurn(cc, clawID, errText, definite), true
	}
	if cc != nil {
		cc.mu.Lock()
		cc.bridgeErrorStreak = 0
		cc.mu.Unlock()
	}
	// An authored turn also proves the provider answered, which is what a
	// released usage limit is waiting to hear.
	s.observeAuthoredTurnForLLMLimit(clawID)
	return s.observeCompletedTurn(clawID, messageID, content), false
}

// turnMaySignal is pipeline.MessageSignals with the transport check in front.
// A bridge-error body is written by claw-bridge, not by the agent, so a
// gateway that folds agent output into its error text must not be able to
// complete ([DONE]) or tear down ([TERMINATE]) a claw whose turn never ran.
func turnMaySignal(content, token string, bridgeErrTurn bool) bool {
	if bridgeErrTurn {
		return false
	}
	return pipeline.MessageSignals(content, token)
}
