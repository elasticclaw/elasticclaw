package claws

// HTTP response helpers live in pkg/hub/httpserver; the claws service must
// not import httpserver (phase-2 dependency rule: httpserver knows the
// services, never the reverse), so the composition root injects the JSON-OK
// and domain-error writers through Deps and the methods below keep the
// mechanically-moved handler bodies unchanged.

import "net/http"

// jsonOK writes v as a 200 JSON response via the injected writer. The
// plain-text fallback only triggers in hand-built test services that skip
// the hub wiring.
func (s *Service) jsonOK(w http.ResponseWriter, v interface{}) {
	if s.deps.JSONOK != nil {
		s.deps.JSONOK(w, v)
		return
	}
	http.Error(w, "response writer not wired", http.StatusInternalServerError)
}

// writeDomainErr maps err to the unified JSON error envelope via the
// injected writer (httpserver.WriteDomainErr in production wiring).
func (s *Service) writeDomainErr(w http.ResponseWriter, err error) {
	if s.deps.WriteDomainErr != nil {
		s.deps.WriteDomainErr(w, err)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
