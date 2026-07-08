package claws

// HTTP response helpers moved to pkg/hub/httpserver during the phase-2
// split; the alias keeps the mechanically-moved handler bodies unchanged.

import "github.com/elasticclaw/elasticclaw/pkg/hub/httpserver"

var jsonOK = httpserver.JSONOK
