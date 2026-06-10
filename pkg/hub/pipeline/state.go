package pipeline

// This file intentionally contains only the Stage type helpers used by the hub
// for pipeline state management. The actual DB read/write methods live on
// *hub.Server in pkg/hub/pipeline_state.go to avoid circular imports.
