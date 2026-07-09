package types

import "errors"

// Sentinel domain errors (phase-2 item 2.5). Services and stores return
// these — or wrap them with fmt.Errorf("...: %w", ...) — instead of ad hoc
// error strings, and pkg/hub/httpserver maps them to HTTP statuses with
// errors.Is in a single place (StatusForError). This ends the per-handler
// response inconsistency where the same condition produced different
// statuses depending on the endpoint.
var (
	// ErrUnauthorized: the request carries no valid credential. Maps to 401.
	ErrUnauthorized = errors.New("unauthorized")

	// ErrTenantMismatch: the credential is valid but belongs to a different
	// tenant than the resource. Maps to 403.
	ErrTenantMismatch = errors.New("tenant mismatch")

	// ErrClawNotFound: the claw does not exist (or is deleted) for the
	// requesting tenant. Maps to 404.
	ErrClawNotFound = errors.New("claw not found")

	// ErrWorkflowNotFound: no workflow with that name in the workspace.
	// Maps to 404.
	ErrWorkflowNotFound = errors.New("workflow not found")

	// ErrWorkspaceNotFound: no workspace with that name. Maps to 404.
	ErrWorkspaceNotFound = errors.New("workspace not found")

	// ErrCheckpointNotFound: the checkpoint does not exist for the
	// requesting tenant/claw. Maps to 404.
	ErrCheckpointNotFound = errors.New("checkpoint not found")

	// ErrCheckpointNotReady: the checkpoint exists but is not in a
	// restorable state. Maps to 409.
	ErrCheckpointNotReady = errors.New("checkpoint is not ready")
)
