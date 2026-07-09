package httpserver

import (
	"errors"
	"net/http"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// domainStatus is one row of the sentinel-error → HTTP status table.
type domainStatus struct {
	err    error
	status int
	code   string
}

// domainStatuses is the single errors.Is → HTTP status map for the sentinel
// domain errors in pkg/types (phase-2 item 2.5). Handlers call WriteDomainErr
// instead of mapping errors to statuses ad hoc.
var domainStatuses = []domainStatus{
	{types.ErrUnauthorized, http.StatusUnauthorized, "unauthorized"},
	{types.ErrTenantMismatch, http.StatusForbidden, "forbidden"},
	{types.ErrClawNotFound, http.StatusNotFound, "not_found"},
	{types.ErrWorkflowNotFound, http.StatusNotFound, "not_found"},
	{types.ErrWorkspaceNotFound, http.StatusNotFound, "not_found"},
	{types.ErrCheckpointNotFound, http.StatusNotFound, "not_found"},
	{types.ErrCheckpointNotReady, http.StatusConflict, "conflict"},
}

// StatusForError resolves err against the domain-error table with errors.Is
// and returns the HTTP status plus the stable machine-readable code for the
// JSON error envelope. Unrecognized errors map to 500/"internal".
func StatusForError(err error) (status int, code string) {
	for _, m := range domainStatuses {
		if errors.Is(err, m.err) {
			return m.status, m.code
		}
	}
	return http.StatusInternalServerError, "internal"
}

// WriteDomainErr maps err to an HTTP status via StatusForError and writes the
// unified JSON error envelope. Domain errors surface their message to the
// client; unrecognized errors are written as a generic 500 so internal detail
// (SQL text, file paths) never leaks into responses.
func WriteDomainErr(w http.ResponseWriter, err error) {
	status, code := StatusForError(err)
	msg := "internal server error"
	if status != http.StatusInternalServerError {
		msg = err.Error()
	}
	WriteErr(w, status, code, msg)
}
