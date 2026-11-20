package hub

// heartbeatHarnessFields holds the heartbeat runtime status under both the
// neutral harness_* names and the legacy gateway_* names.
type heartbeatHarnessFields struct {
	GatewayHealthy      bool    `json:"gateway_healthy"`
	GatewayReady        *bool   `json:"gateway_ready,omitempty"`
	RestartCount        int     `json:"restart_count"`
	GatewaySessionKey   *string `json:"gateway_session_key"`
	HarnessHealthy      *bool   `json:"harness_healthy"`
	HarnessReady        *bool   `json:"harness_ready,omitempty"`
	HarnessRestartCount *int    `json:"harness_restart_count"`
	HarnessSessionKey   *string `json:"harness_session_key"`
}

// normalizeHarnessFields prefers harness fields; gateway_* names (and
// restart_count) are still read for bridges older than this release.
func (hb *heartbeatHarnessFields) normalizeHarnessFields() {
	if hb.HarnessHealthy != nil {
		hb.GatewayHealthy = *hb.HarnessHealthy
	}
	if hb.HarnessReady != nil {
		hb.GatewayReady = hb.HarnessReady
	}
	if hb.HarnessRestartCount != nil {
		hb.RestartCount = *hb.HarnessRestartCount
	}
	if hb.HarnessSessionKey != nil {
		hb.GatewaySessionKey = hb.HarnessSessionKey
	}
}
