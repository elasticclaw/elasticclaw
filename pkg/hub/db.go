package hub

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite, no CGO required

	"github.com/elasticclaw/elasticclaw/pkg/hub/store"
)

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path+"?_time_format=sqlite&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := store.Migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// pruneFactoryAnalytics deletes factory_analytics rows older than 1 year.
// Should be called periodically (e.g. daily) from a background goroutine.
func pruneFactoryAnalytics(db *sql.DB) {
	_, err := db.Exec(`DELETE FROM factory_analytics WHERE created_at < datetime('now', '-1 year')`)
	if err != nil {
		log.Printf("[db] factory analytics prune error: %v", err)
	}
}

func now() time.Time { return time.Now().UTC() }
