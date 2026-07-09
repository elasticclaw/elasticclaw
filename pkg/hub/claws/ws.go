package claws

// The claw bridge WebSocket handler, moved verbatim from pkg/hub server.go
// (handleClawWS and its helpers) in the phase-2 claws extraction. The only
// changes are the mechanical ones the package move forces: exported Conn
// field names, registry/waiter access through the injected hooks, and the
// pipeline-trigger block behind Deps.EvaluatePipelineMessageTriggers (it
// builds workflows-package types that claws must not import).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// HandleClawWS is the /claw/ws endpoint: bridge registration, heartbeats,
// streaming chunks, message finalization, file/volume acks and the reverse
// HTTP proxy.
func (s *Service) HandleClawWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}

	ctx := r.Context()

	// First message must be registration
	var reg types.WSMessage
	if err := wsjson.Read(ctx, conn, &reg); err != nil || reg.Type != "register" {
		conn.Close(websocket.StatusPolicyViolation, "expected register")
		return
	}

	payload, _ := json.Marshal(reg.Payload)
	var rp types.RegisterPayload
	if err := json.Unmarshal(payload, &rp); err != nil {
		conn.Close(websocket.StatusPolicyViolation, "invalid register payload")
		return
	}

	tenantID, err := s.tenantByClawToken(rp.Token)
	if err != nil {
		logfCtx(r.Context(), "[claw ws] invalid token for claw %.8s: received_len=%d configured_len=%d err=%v", rp.ClawID, len(rp.Token), len(s.hubCfg().ClawToken), err)
		conn.Close(websocket.StatusPolicyViolation, "invalid token")
		return
	}

	clawID := rp.ClawID
	if clawID == "" {
		clawID = uuid.New().String()
	}

	// Check if this is a status channel registration BEFORE any DB upsert.
	// Status channels must not mutate claw DB state (rp.GatewayReady is nil,
	// so initialStatus would incorrectly overwrite 'starting'/'bootstrap_needed').
	isStatusChannel := rp.Channel == "status"

	var currentStatus string
	bootstrapOK, provider := s.st.Claws().BootstrapInfo(r.Context(), clawID, tenantID)

	if !isStatusChannel {
		// Upsert claw and keep terminal/watching states sticky across reconnects.
		desiredStatus := initialStatus(rp.GatewayReady)
		if !AllowWakeBeforeBootstrap(provider, bootstrapOK) {
			desiredStatus = "starting"
		}
		currentStatus = desiredStatus
		if status, err := s.st.Claws().RegisterUpsert(r.Context(), clawID, tenantID, rp.Name, rp.Template, desiredStatus, now()); err == nil {
			currentStatus = status
		}
		if currentStatus == "deleted" {
			conn.Close(websocket.StatusPolicyViolation, "claw deleted")
			return
		}
	} else {
		// For status channel, just read current status from DB
		currentStatus = s.st.Claws().StatusForTenant(r.Context(), clawID, tenantID)
	}

	registrationTags, _ := s.st.Claws().Tags(r.Context(), clawID, tenantID)
	allowWake := AllowWakeBeforeBootstrap(provider, bootstrapOK)

	if isStatusChannel {
		// Status channel connects to existing claw
		if existing, ok := s.reg.Get(clawID); ok {
			existing.Mu.Lock()
			existing.StatusConn = conn
			existing.Mu.Unlock()
			logfCtx(r.Context(), "[bridge] ✓ status channel connected: %s (%s)", rp.Name, clawID[:8])
			_ = wsjson.Write(ctx, conn, types.WSMessage{Type: "registered", Payload: map[string]string{"claw_id": clawID, "channel": "status"}})
			// Simple read loop for status channel — just keepalive
			for {
				var msg types.WSMessage
				if err := wsjson.Read(ctx, conn, &msg); err != nil {
					if existing2, ok2 := s.reg.Get(clawID); ok2 {
						existing2.Mu.Lock()
						existing2.StatusConn = nil
						existing2.Mu.Unlock()
					}
					return
				}
				s.metrics.wsMessage("in", "claw")
				if msg.Type == "status_pong" {
					if existing2, ok2 := s.reg.Get(clawID); ok2 {
						existing2.Mu.Lock()
						existing2.LastStatusAt = time.Now()
						existing2.Mu.Unlock()
					}
				}
			}
		}
		conn.Close(websocket.StatusPolicyViolation, "main channel not connected")
		return
	}

	cc := &Conn{ClawID: clawID, TenantID: tenantID, WS: conn, GatewayReady: gatewayReadyBool(rp.GatewayReady), Tags: registrationTags, LastUserMessageAt: time.Now(), LastStatusAt: time.Now()}
	var hasQueuedMessages bool
	s.reg.Do(func(conns map[string]*Conn) {
		if old, ok := conns[clawID]; ok {
			old.Mu.RLock()
			cc.StatusConn = old.StatusConn
			cc.LastStatusAt = old.LastStatusAt
			// Copy message queue from old connection to preserve queued messages
			if len(old.MessageQueue) > 0 {
				cc.MessageQueue = make([]types.HubMessage, len(old.MessageQueue))
				copy(cc.MessageQueue, old.MessageQueue)
			}
			old.Mu.RUnlock()
		}
		// Capture whether we have queued messages before unlocking
		hasQueuedMessages = len(cc.MessageQueue) > 0
		conns[clawID] = cc
	})

	logfCtx(r.Context(), "[bridge] ✓ connected: %s (%s) gateway_ready=%v", rp.Name, clawID[:8], cc.GatewayReady)

	// submit runs fn on the hub's bounded WS worker pool (phase-2 item 2.3:
	// the former unbounded go-per-message spawn). On overflow it closes the
	// connection with StatusOverloaded so a malicious or looping client
	// cannot exhaust the hub; callers must stop handling the connection
	// when it returns false.
	submit := func(fn func()) bool {
		if s.pool.TrySubmit(fn) {
			return true
		}
		logfCtx(r.Context(), "[claw ws] worker pool exhausted; closing %s (%s)", rp.Name, clawID[:8])
		conn.Close(StatusOverloaded, "hub worker pool exhausted")
		return false
	}

	// Ack
	_ = wsjson.Write(ctx, conn, types.WSMessage{Type: "registered", Payload: map[string]string{"claw_id": clawID}})

	// Broadcast initial status to user sessions
	s.broadcastToUsers(tenantID, types.WSMessage{Type: "claw_status", Payload: map[string]string{"claw_id": clawID, "status": currentStatus}})

	// Drain any queued messages that were copied from the old connection.
	// This must happen after the connection is live but before the read loop starts.
	// We call it synchronously (not in a goroutine) to avoid racing with new user messages.
	if hasQueuedMessages {
		s.sendNextQueuedMessage(cc)
	}

	// Initialize entry pipeline stage only after bridge connects so on_enter inject
	// can be delivered over WS.
	if allowWake && cc.GatewayReady && currentStatus == "connected" {
		s.startWorkflowAfterVolumes(ctx, cc, clawID)
	}
	if allowWake && cc.GatewayReady && currentStatus == "connected" && !s.hasRecentCheckpoint(clawID, time.Hour) {
		if !submit(func() { s.requestBootstrapCheckpoint(clawID) }) {
			return
		}
	}

	// Read loop — claw sends messages back to users
	defer func() {
		var partialContent string
		var partialMsgID string
		s.reg.Do(func(conns map[string]*Conn) {
			// Flush any partial streaming buffer as an interrupted message
			if partialCC, ok := conns[clawID]; ok && partialCC.StreamingBuf.Len() > 0 {
				partialContent = partialCC.StreamingBuf.String() + " [interrupted]"
				partialMsgID = partialCC.StreamingMsgID
				if partialMsgID == "" {
					partialMsgID = uuid.New().String()
				}
				partialCC.StreamingBuf.Reset()
				partialCC.StreamingMsgID = ""
			}
			delete(conns, clawID)
		})
		if partialContent != "" {
			interruptedAt := now()
			_ = s.st.Messages().Upsert(s.baseCtx(), types.HubMessage{
				ID: partialMsgID, ClawID: clawID, TenantID: tenantID, Role: "claw",
				Content: partialContent, CreatedAt: interruptedAt,
			})
			s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: types.HubMessage{
				ID: partialMsgID, ClawID: clawID, TenantID: tenantID, Role: "claw",
				Content: partialContent, CreatedAt: interruptedAt,
			}})
		}
		// Clear typing indicator so the UI doesn't show a stuck "typing" state
		// if the claw disconnects mid-response.
		s.broadcastToUsers(tenantID, types.WSMessage{
			Type: "agent_typing",
			Payload: map[string]string{
				"claw_id": clawID,
				"status":  "idle",
			},
		})
		currentStatus := s.st.Claws().Status(s.baseCtx(), clawID)
		// Don't overwrite terminal/watching states — idle means the claw sent [DONE]
		// and is watching for PR merge; deleted means it's being cleaned up.
		if currentStatus != "completed" && currentStatus != "deleted" && currentStatus != "idle" {
			_ = s.st.Claws().MarkOffline(s.baseCtx(), clawID, now())
			s.broadcastToUsers(tenantID, types.WSMessage{Type: "claw_status", Payload: map[string]string{"claw_id": clawID, "status": "offline"}})
		}
		logfCtx(r.Context(), "[bridge] ✗ disconnected: %s (%s)", rp.Name, clawID[:8])
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.st.Claws().TouchLastSeen(ctx, clawID, now())
		default:
			var msg types.WSMessage
			conn.SetReadLimit(32 << 20) // 32MB (file uploads ride this channel)
			if err := wsjson.Read(ctx, conn, &msg); err != nil {
				return
			}
			s.metrics.wsMessage("in", "claw")
			if msg.Type == "heartbeat" {
				payload, _ := json.Marshal(msg.Payload)
				var hb struct {
					GatewayHealthy bool  `json:"gateway_healthy"`
					GatewayReady   *bool `json:"gateway_ready,omitempty"`
					ContextUsage   int   `json:"context_usage"`
				}
				if err := json.Unmarshal(payload, &hb); err == nil {
					var wakeConn *Conn
					var shouldWake bool
					var shouldWarnContext bool
					var prevUsage int
					if cc, ok := s.reg.Get(clawID); ok {
						cc.Mu.Lock()
						// Log only on status changes, not every heartbeat
						prevUsage = cc.ContextUsage
						cc.ContextUsage = hb.ContextUsage
						// Promote from 'starting' to 'connected' once gateway is ready.
						// nil means field absent (old bridge) — treat as ready.
						if gatewayReadyBool(hb.GatewayReady) {
							rowsUpdated, _ := s.st.Claws().PromoteStartingToConnected(ctx, clawID)
							cc.GatewayReady = true
							if rowsUpdated > 0 {
								s.broadcastToUsers(tenantID, types.WSMessage{
									Type:    "claw_status",
									Payload: map[string]string{"claw_id": clawID, "status": "connected"},
								})
								logfCtx(r.Context(), "[bridge] ✓ ready: %s (%s)", rp.Name, clawID[:8])
								shouldWake = true
								wakeConn = cc
								submit(func() { s.requestBootstrapCheckpoint(clawID) })
							}
						}
						if !hb.GatewayHealthy {
							cc.GatewayUnhealthyCount++
							if cc.GatewayUnhealthyCount == 1 {
								logfCtx(r.Context(), "[heartbeat] %s (%s): gateway unhealthy", rp.Name, clawID[:8])
							} else if cc.GatewayUnhealthyCount%4 == 0 {
								logfCtx(r.Context(), "[heartbeat] %s (%s): gateway unhealthy for %d consecutive checks", rp.Name, clawID[:8], cc.GatewayUnhealthyCount)
							}
							if cc.GatewayUnhealthyCount == 4 && !cc.StreamingStartedAt.IsZero() {
								submit(func() {
									s.injectHubMessageByID(clawID, "[hub] The gateway has been unresponsive for about a minute. If you're stuck in a long operation, consider sending [DONE] and starting fresh.")
								})
							}
						}
						// Log context usage on every heartbeat when it crosses the 80% threshold,
						// regardless of gateway health — don't silence diagnostics during outages.
						if hb.ContextUsage != prevUsage && (hb.ContextUsage >= 80 || prevUsage >= 80) {
							logfCtx(r.Context(), "[heartbeat] %s (%s): context_usage=%d%%", rp.Name, clawID[:8], hb.ContextUsage)
						}
						if hb.GatewayHealthy && cc.GatewayUnhealthyCount > 0 {
							logfCtx(r.Context(), "[heartbeat] %s (%s): gateway recovered after %d unhealthy checks", rp.Name, clawID[:8], cc.GatewayUnhealthyCount)
							cc.GatewayUnhealthyCount = 0
						}
						// Inject context warning once per streaming turn when usage is >=95%
						if !cc.StreamingStartedAt.IsZero() &&
							hb.ContextUsage >= 95 &&
							!cc.ContextWarningSent {
							cc.ContextWarningSent = true
							shouldWarnContext = true
						}
						cc.Mu.Unlock()
					}
					s.heartbeatWorkflowVolumeLeases(clawID)
					if shouldWarnContext {
						warnCC := s.reg.Lookup(clawID)
						if warnCC != nil {
							submit(func() {
								s.injectHubMessage(ctx, warnCC, "[hub] Context window is nearly full. Summarize your progress briefly and send [DONE] with any PR URL, or ask the user what to do next.")
							})
						}
					}
					if shouldWake {
						s.startWorkflowAfterVolumes(ctx, wakeConn, clawID)
					}
					// Check for streaming turn timeout (12 minutes)
					cc, ok := s.reg.Get(clawID)
					if ok {
						cc.Mu.Lock()
						if !cc.StreamingStartedAt.IsZero() &&
							!cc.StreamingTimeoutSent &&
							time.Since(cc.StreamingStartedAt) > 12*time.Minute {
							cc.StreamingTimeoutSent = true
							cc.Mu.Unlock()
							submit(func() {
								s.injectHubMessage(ctx, cc, "[hub] Your current response has been running for over 12 minutes. Please wrap up and send your response.")
							})
						} else {
							cc.Mu.Unlock()
						}
					}
				}
			} else if msg.Type == "agent_activity" {
				if activity, payload, ok := NormalizeAgentActivityPayload(msg.Payload); ok {
					if err := s.flushStreamingSegment(clawID, tenantID, cc); err != nil {
						logfCtx(r.Context(), "[agent_activity] flush streaming segment for %s: %v", clawID[:8], err)
					}
					if IsBusyAgentActivity(activity) {
						cc.Mu.Lock()
						if cc.StreamingStartedAt.IsZero() {
							cc.StreamingStartedAt = time.Now()
							cc.StreamingTimeoutSent = false
							cc.ContextWarningSent = false
						}
						cc.Mu.Unlock()
					}
					createdAt := now()
					activity["claw_id"] = clawID
					activity["created_at"] = createdAt.Format(time.RFC3339Nano)
					content := activityContent(activity)
					if content != "" && !isUnhelpfulActivityContent(activity, content) {
						format := "activity:" + string(payload)
						_ = s.st.Messages().Insert(ctx, types.HubMessage{
							ID: uuid.New().String(), ClawID: clawID, TenantID: tenantID,
							Role: "activity", Content: content, Format: format, CreatedAt: createdAt,
						})
					}
					s.broadcastToUsers(tenantID, types.WSMessage{
						Type:    "agent_activity",
						Payload: activity,
					})
					s.handleInitialPlanActivity(clawID, tenantID, activity)
				}
			} else if msg.Type == "chunk" {
				// Streaming chunk — forward to users immediately AND buffer server-side
				payload, _ := json.Marshal(msg.Payload)
				var chunk struct {
					Content string `json:"content"`
				}
				if err := json.Unmarshal(payload, &chunk); err == nil && chunk.Content != "" {
					s.broadcastToUsers(tenantID, types.WSMessage{
						Type:    "chunk",
						Payload: map[string]string{"claw_id": clawID, "content": chunk.Content},
					})
					// Buffer chunk and upsert partial message to DB so refreshes don't lose it
					cc, ok := s.reg.Get(clawID)
					if ok {
						cc.Mu.Lock()
						if cc.StreamingMsgID == "" {
							cc.StreamingMsgID = uuid.New().String()
							cc.StreamingTimeoutSent = false
							cc.ContextWarningSent = false
						}
						if cc.StreamingStartedAt.IsZero() {
							cc.StreamingStartedAt = time.Now()
						}
						cc.StreamingBuf.WriteString(chunk.Content)
						msgID := cc.StreamingMsgID
						bufContent := cc.StreamingBuf.String()
						cc.Mu.Unlock()
						// Upsert — insert on first chunk, update content on subsequent
						_ = s.st.Messages().Upsert(ctx, types.HubMessage{
							ID: msgID, ClawID: clawID, TenantID: tenantID, Role: "claw",
							Content: bufContent, CreatedAt: now(),
						})
					}
				}
			} else if msg.Type == "message" {
				// Complete message — finalize the buffered stream or store fresh
				payload, _ := json.Marshal(msg.Payload)
				var hm types.HubMessage
				if err := json.Unmarshal(payload, &hm); err != nil {
					continue
				}
				hm.ClawID = clawID
				hm.TenantID = tenantID
				hm.Role = "claw"
				hm.CreatedAt = now()
				// Always clean up streaming state first, even for empty messages.
				// Use the outer cc (this goroutine's connection), not a fresh lookup.
				// If the claw reconnected, a new handleClawWS goroutine handles the new cc.
				cc.Mu.Lock()
				persistContent := hm.Content
				skipPersist := false
				if cc.StreamingMsgID != "" {
					hm.ID = cc.StreamingMsgID
					if cc.StreamingBuf.Len() > 0 {
						persistContent = cc.StreamingBuf.String()
					}
				} else {
					hm.ID = uuid.New().String()
					skipPersist = cc.StreamingSplit
				}
				cc.FinishTurnLocked()
				cc.Mu.Unlock()
				// Drop empty messages — never store or broadcast
				if strings.TrimSpace(hm.Content) == "" {
					// Clear typing indicator first — always clear even if no queued messages
					s.broadcastToUsers(tenantID, types.WSMessage{
						Type: "agent_typing",
						Payload: map[string]string{
							"claw_id": clawID,
							"status":  "idle",
						},
					})
					// Drain queue using this goroutine's cc (the outer cc from line 1449).
					// If the claw reconnected, a new handleClawWS goroutine handles the new cc.
					s.sendNextQueuedMessage(cc)
					s.drainPendingCheckpoint(clawID)
					continue
				}
				if !skipPersist && strings.TrimSpace(persistContent) != "" {
					_ = s.st.Messages().Upsert(ctx, types.HubMessage{
						ID: hm.ID, ClawID: hm.ClawID, TenantID: hm.TenantID, Role: hm.Role,
						Content: persistContent, CreatedAt: hm.CreatedAt,
					})
					s.broadcastToUsers(tenantID, types.WSMessage{Type: "message", Payload: hm})
				}
				s.handleInitialPlanResponse(clawID, tenantID, hm.Content)
				// Evaluate pipeline triggers. If a pipeline explicitly owns a
				// [DONE] trigger, let it handle that signal instead of the
				// legacy factory PR-URL completion path below. The block lives
				// behind a hub hook because it builds workflows-package
				// pipeline contexts.
				pipelineHandledDone := s.deps.EvaluatePipelineMessageTriggers(clawID, hm.Content)
				// Clear typing indicator now that response is complete
				s.broadcastToUsers(tenantID, types.WSMessage{
					Type: "agent_typing",
					Payload: map[string]string{
						"claw_id": clawID,
						"status":  "idle",
					},
				})
				// Check for [DONE] signal from a factory-created claw
				if strings.Contains(hm.Content, "[DONE]") {
					if !submit(func() {
						if _, err := s.requestCheckpoint(s.baseCtx(), clawID, "done", "hub", false, s.deps.CheckpointRequestTimeout); err != nil {
							logfCtx(r.Context(), "[checkpoint] done request for %s failed: %v", shortID(clawID), err)
						}
					}) {
						return
					}
					if !pipelineHandledDone {
						if !submit(func() { s.handleClawDoneSignal(clawID, hm.Content) }) {
							return
						}
					}
				}
				// Check for [TERMINATE] signal - allows claw to manage its own lifecycle
				if strings.Contains(hm.Content, "[TERMINATE]") {
					if !submit(func() { s.handleClawTerminateSignal(clawID, hm.Content) }) {
						return
					}
				}
				// Detect and store any PR URLs mentioned by the agent
				if !submit(func() { s.scanMessageForPRs(clawID, hm.Content) }) {
					return
				}
				// Detect tool error loops and inject a corrective message
				if DetectToolLoop(hm.Content) {
					loopCC := s.reg.Lookup(clawID)
					if loopCC != nil {
						if !submit(func() {
							s.injectHubMessage(ctx, loopCC, "[hub] You've hit the same tool error 3+ times in a row. Stop retrying. Take a completely different approach or ask for help.")
						}) {
							return
						}
					}
				}
				// Check for queued messages and send the next one.
				// Use this goroutine's cc (the outer cc from line 1449).
				// If the claw reconnected, a new handleClawWS goroutine handles the new cc.
				s.sendNextQueuedMessage(cc)
				s.drainPendingCheckpoint(clawID)
			} else if msg.Type == "file_ack" {
				raw, _ := json.Marshal(msg.Payload)
				var ack types.FileAck
				if err := json.Unmarshal(raw, &ack); err == nil && ack.RequestID != "" {
					s.fileAckMu.Lock()
					ch := s.fileAckWaiters()[ack.RequestID]
					delete(s.fileAckWaiters(), ack.RequestID)
					s.fileAckMu.Unlock()
					if ch != nil {
						select {
						case ch <- ack:
						default:
						}
					}
				}
			} else if msg.Type == "file_read_resp" {
				raw, _ := json.Marshal(msg.Payload)
				var resp types.FileReadResp
				if err := json.Unmarshal(raw, &resp); err == nil && resp.RequestID != "" {
					s.fileAckMu.Lock()
					ch := s.fileReadWaiters()[resp.RequestID]
					delete(s.fileReadWaiters(), resp.RequestID)
					s.fileAckMu.Unlock()
					if ch != nil {
						select {
						case ch <- resp:
						default:
						}
					}
				}
			} else if msg.Type == "volume_attach_ack" {
				raw, _ := json.Marshal(msg.Payload)
				var ack types.VolumeAttachAck
				if err := json.Unmarshal(raw, &ack); err == nil && ack.RequestID != "" {
					s.fileAckMu.Lock()
					ch := s.volumeAttachWaiters()[ack.RequestID]
					delete(s.volumeAttachWaiters(), ack.RequestID)
					s.fileAckMu.Unlock()
					if ch != nil {
						select {
						case ch <- ack:
						default:
						}
					}
				}
			} else if msg.Type == "volume_sync_ack" {
				raw, _ := json.Marshal(msg.Payload)
				var ack types.VolumeSyncAck
				if err := json.Unmarshal(raw, &ack); err == nil && ack.RequestID != "" {
					s.fileAckMu.Lock()
					ch := s.volumeSyncWaiters()[ack.RequestID]
					delete(s.volumeSyncWaiters(), ack.RequestID)
					s.fileAckMu.Unlock()
					if ch != nil {
						select {
						case ch <- ack:
						default:
						}
					}
				}
			} else if msg.Type == "http_proxy_req" {
				// Proxy an HTTP request from the bridge to the hub's internal API.
				// This allows tools in the sandbox to reach hub APIs without a public URL.
				proxyFn := func(rawPayload json.RawMessage, conn *websocket.Conn) func() {
					return func() {
						var req struct {
							ReqID  string            `json:"req_id"`
							Method string            `json:"method"`
							Path   string            `json:"path"`
							Query  string            `json:"query"`
							Body   string            `json:"body"`
							Header map[string]string `json:"header"`
						}
						if err := json.Unmarshal(rawPayload, &req); err != nil {
							logfCtx(ctx, "[hub-proxy] bad req payload: %v", err)
							return
						}
						logfCtx(ctx, "[hub-proxy] req req_id=%s %s %s?%s", req.ReqID, req.Method, req.Path, req.Query)
						// Build an internal HTTP request
						urls := req.Path
						if req.Query != "" {
							urls += "?" + req.Query
						}
						httpReq, err := http.NewRequestWithContext(ctx, req.Method, "http://localhost"+urls, strings.NewReader(req.Body))
						if err != nil {
							logfCtx(ctx, "[hub-proxy] build request failed req_id=%s err=%v", req.ReqID, err)
							s.sendHTTPProxyRes(ctx, conn, req.ReqID, 400, "bad request")
							return
						}
						for k, v := range req.Header {
							httpReq.Header.Set(k, v)
						}
						// Inject claw_token auth so withAuth middleware passes
						s.cfgMu.RLock()
						clawToken := s.hubCfg().ClawToken
						s.cfgMu.RUnlock()
						httpReq.Header.Set("X-Claw-Token", clawToken)
						// Execute against internal mux
						w := &proxyResponseWriter{header: make(http.Header)}
						s.mux().ServeHTTP(w, httpReq)
						if w.status == 0 {
							w.status = 200
						}
						logfCtx(ctx, "[hub-proxy] res req_id=%s status=%d body_len=%d", req.ReqID, w.status, len(w.body))
						s.sendHTTPProxyRes(ctx, conn, req.ReqID, w.status, string(w.body))
					}
				}
				if !submit(proxyFn(mustJSONRaw(msg.Payload), conn)) {
					return
				}
			}
		}
	}
}

func (s *Service) sendHTTPProxyRes(ctx context.Context, conn *websocket.Conn, reqID string, status int, body string) {
	_ = wsjson.Write(ctx, conn, map[string]interface{}{
		"type":    "http_proxy_res",
		"payload": map[string]interface{}{"req_id": reqID, "status": status, "body": body},
	})
}

// proxyResponseWriter captures an HTTP handler's response.
type proxyResponseWriter struct {
	header http.Header
	status int
	body   []byte
}

func (w *proxyResponseWriter) Header() http.Header {
	return w.header
}
func (w *proxyResponseWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return len(b), nil
}
func (w *proxyResponseWriter) WriteHeader(status int) {
	w.status = status
}

// mustJSONRaw mirrors the pkg/hub helper of the same name (a duplicate is
// kept here so claws does not reach back into the hub package).
func mustJSONRaw(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return json.RawMessage(b)
}
