package hub

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// The boot-time allocation gate.
//
// A table copy (the CHECK-widening rebuilds) and an index build over the two
// largest tables are the migration steps that can need gigabytes, and on the
// disk-full hub they are doomed before they start: each scans its table under
// the write lock, allocates the whole result, and fails at the last page --
// on the first boot after a deploy, three times over for task_run_attempts,
// task_run_summaries and task_run_events, before the hub serves a request.
// truncateWALAfterFailure gives the space back afterwards; this gate spares
// the attempt when the filesystem visibly cannot hold the result.
//
// The free-space figure is a real statfs of the filesystem holding the
// database, never a guess. The need is measured too, from the pages the
// object occupies (dbstat), and only when it matters: a filesystem with room
// for a second copy of the whole database has room for any one step, and that
// check is a single pragma. An unknown answer -- an in-memory database, a
// platform without statfs, a failed query -- lets the step run: the gate is an
// optimisation of the failure, not a second line of defence, and the truncate
// after a failure covers what it misses.

// diskFreeBytes reports the bytes a non-root process may still write to the
// filesystem holding path. A variable so a test can model a nearly full disk
// without needing one.
var diskFreeBytes = diskFreeBytesOS

// tableCopyFitsOnDisk returns an error when the filesystem holding the
// database visibly cannot hold a rebuilt copy of table. A copy needs the
// table and its indexes twice over -- once as WAL frames, once checkpointed
// into the file -- before the old pages are freed.
func tableCopyFitsOnDisk(db *sql.DB, table string) error {
	return stepFitsOnDisk(db, "a copy of "+table, func() (int64, error) {
		names, err := tableObjectNames(db, table)
		if err != nil {
			return 0, err
		}
		size, err := btreeBytes(db, names...)
		return 2 * size, err
	})
}

// indexBuildFitsOnDisk returns an error when the filesystem holding the
// database visibly cannot hold a new index on table. A single-column index is
// a fraction of its table; doubled for the WAL copy, the table's own size
// bounds it.
func indexBuildFitsOnDisk(db *sql.DB, table string) error {
	return stepFitsOnDisk(db, "an index on "+table, func() (int64, error) {
		return btreeBytes(db, table)
	})
}

// stepFitsOnDisk compares the free space on the database's filesystem with
// what the step needs. It measures the need only when the cheap bound -- room
// for a second copy of the whole database -- does not already answer.
func stepFitsOnDisk(db *sql.DB, what string, need func() (int64, error)) error {
	path, err := databaseFilePath(db)
	if err != nil || path == "" {
		return nil
	}
	free, err := diskFreeBytes(filepath.Dir(path))
	if err != nil {
		return nil
	}
	var pageCount, pageSize int64
	if err := db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		return nil
	}
	if err := db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return nil
	}
	if free >= 2*pageCount*pageSize {
		return nil
	}
	required, err := need()
	if err != nil || free >= required {
		return nil
	}
	return fmt.Errorf("skipped: %s needs about %d bytes of free disk and the filesystem holding the database has %d", what, required, free)
}

// databaseFilePath is the file behind the main database, or "" for an
// in-memory one.
func databaseFilePath(db *sql.DB) (string, error) {
	rows, err := db.Query(`PRAGMA database_list`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var seq int
		var name, file string
		if err := rows.Scan(&seq, &name, &file); err != nil {
			return "", err
		}
		if name == "main" {
			return file, nil
		}
	}
	return "", rows.Err()
}

// tableObjectNames is the table together with every index on it, by name.
func tableObjectNames(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE tbl_name=? AND type IN ('table','index')`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// btreeBytes is the disk the named tables and indexes occupy, from dbstat.
// It walks every page of each object, which is why the callers ask only when
// the cheap bound has not already answered.
func btreeBytes(db *sql.DB, names ...string) (int64, error) {
	if len(names) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(names))
	for _, name := range names {
		args = append(args, name)
	}
	var size int64
	err := db.QueryRow(`SELECT COALESCE(SUM(pgsize), 0) FROM dbstat WHERE name IN (?`+strings.Repeat(",?", len(names)-1)+`)`, args...).Scan(&size)
	return size, err
}
