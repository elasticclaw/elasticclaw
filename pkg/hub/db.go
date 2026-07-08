package hub

import (
	"database/sql"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/store"
)

// openDB opens (and migrates) the hub database. The connection string
// and migrations moved to pkg/hub/store (phase-2 item 2.4); this thin
// delegate keeps the existing call sites and tests unchanged.
func openDB(path string) (*sql.DB, error) {
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	return st.DB(), nil
}

// migrate delegates to the store's schema migrations (kept for tests
// that re-run migrations against an existing database).
func migrate(db *sql.DB) error { return store.Migrate(db) }

// st returns the store bound to the server's database. The store is a
// cheap stateless wrapper, so building it per call keeps hand-built test
// servers (&Server{db: ...}) working without extra wiring — the same
// pattern the claws/settings service bridges use.
func (s *Server) st() *store.Store { return store.New(s.db) }

func now() time.Time { return time.Now().UTC() }
