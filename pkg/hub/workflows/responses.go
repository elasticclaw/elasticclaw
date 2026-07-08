package workflows

// HTTP response helpers moved to pkg/hub/httpserver during the phase-2
// split; the aliases keep the mechanically-moved handler bodies unchanged.

import "github.com/elasticclaw/elasticclaw/pkg/hub/httpserver"

var (
	writeErr  = httpserver.WriteErr
	jsonOK    = httpserver.JSONOK
	jsonError = httpserver.JSONError
)
