package hub

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func rebriefTestFactory() *types.FactoryConfig {
	return &types.FactoryConfig{
		Name: "rebrief-factory",
		PipelineYAML: `
stages:
  - id: plan
    entry: true
    on_enter:
      inject: Plan the work
  - id: implement
    triggers:
      - message_contains: "[GO]"
    on_enter:
      inject: Implement {{.Issue.Identifier}}
`,
	}
}

func newRebriefTestServer(t *testing.T, clawID, status, stage string) (*Server, *sql.DB) {
	t.Helper()
	s, db := NewTestServerWithConfig(t, &types.HubConfig{
		Token:     "test-token",
		Factories: []*types.FactoryConfig{rebriefTestFactory()},
	}, "", "", "")
	if _, err := db.Exec(
		`INSERT INTO claws(id, tenant_id, name, template, status, tags, linear_issue_id, pipeline_stage, created_at) VALUES(?,?,?,?,?,?,?,?,datetime('now'))`,
		clawID, "test-tenant-id", "rebrief claw", "elasticclaw", status, `["factory:rebrief-factory"]`, "AMA-200", stage,
	); err != nil {
		t.Fatalf("insert claw: %v", err)
	}
	return s, db
}

func clawRebriefPending(t *testing.T, db *sql.DB, clawID string) int {
	t.Helper()
	var pending int
	if err := db.QueryRow(`SELECT rebrief_pending FROM claws WHERE id=?`, clawID).Scan(&pending); err != nil {
		t.Fatalf("read rebrief_pending: %v", err)
	}
	return pending
}

func TestRebriefPendingArmedByRetryAndConsumedOnce(t *testing.T) {
	const clawID = "claw-rebrief-consume"
	s, db := newRebriefTestServer(t, clawID, "error", "implement")
	if _, err := db.Exec(
		`INSERT INTO claw_prs(id, claw_id, repo, pr_number, pr_url, created_at) VALUES('pr-1',?,?,?,?,datetime('now'))`,
		clawID, "acme/widgets", 42, "https://github.com/acme/widgets/pull/42",
	); err != nil {
		t.Fatalf("insert claw_prs: %v", err)
	}

	reset, err := s.resetClawForRetry("test-tenant-id", clawID, "", "retrying (attempt 2/3)")
	if err != nil {
		t.Fatalf("resetClawForRetry: %v", err)
	}
	if !reset {
		t.Fatal("resetClawForRetry did not update the claw")
	}
	if pending := clawRebriefPending(t, db, clawID); pending != 1 {
		t.Fatalf("rebrief_pending=%d after retry reset, want 1", pending)
	}

	if !s.rebriefAfterRestoreIfNeeded(nil, clawID) {
		t.Fatal("first rebriefAfterRestoreIfNeeded returned false, want true")
	}
	// The flag is consumed atomically before delivery, so a racing second
	// reconnect must not re-brief again.
	if s.rebriefAfterRestoreIfNeeded(nil, clawID) {
		t.Fatal("second rebriefAfterRestoreIfNeeded returned true, want false")
	}
	if pending := clawRebriefPending(t, db, clawID); pending != 0 {
		t.Fatalf("rebrief_pending=%d after consume, want 0", pending)
	}

	rows, err := db.Query(`SELECT content FROM messages WHERE claw_id=? AND role='hub'`, clawID)
	if err != nil {
		t.Fatalf("select hub messages: %v", err)
	}
	defer rows.Close()
	var briefs []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(content, "Your sandbox was replaced") {
			briefs = append(briefs, content)
		}
	}
	if len(briefs) != 1 {
		t.Fatalf("re-brief messages=%d, want exactly 1", len(briefs))
	}
	brief := briefs[0]
	for _, want := range []string{
		"Current workflow stage: implement",
		"Implement AMA-200",
		"Open PR: https://github.com/acme/widgets/pull/42 (acme/widgets, #42)",
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("re-brief missing %q:\n%s", want, brief)
		}
	}
}

func TestRebriefNotArmedOnNormalReconnect(t *testing.T) {
	const clawID = "claw-rebrief-normal"
	s, db := newRebriefTestServer(t, clawID, "connected", "implement")

	if s.rebriefAfterRestoreIfNeeded(nil, clawID) {
		t.Fatal("rebriefAfterRestoreIfNeeded returned true without a retry reset")
	}
	if pending := clawRebriefPending(t, db, clawID); pending != 0 {
		t.Fatalf("rebrief_pending=%d on normal reconnect, want 0", pending)
	}
	var messages int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id=?`, clawID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 0 {
		t.Fatalf("messages=%d after no-op rebrief, want 0", messages)
	}
}

func TestRenderStageInjectRendersIssueIdentifier(t *testing.T) {
	const clawID = "claw-rebrief-render"
	s, _ := newRebriefTestServer(t, clawID, "connected", "implement")

	factory := rebriefTestFactory()
	ctx := pipelineContext{Factory: factory, IssueID: "AMA-200"}
	pl := parsePipelineForContext(ctx)
	if pl == nil {
		t.Fatal("parse pipeline")
	}
	stage := pl.StageByID("implement")
	if stage == nil {
		t.Fatal("implement stage not found")
	}
	// No Linear token is configured, so rendering falls back to the identifier
	// from the claw's issue context — exactly what the re-brief needs.
	got := s.renderStageInject(clawID, *stage, ctx)
	if got != "Implement AMA-200" {
		t.Fatalf("renderStageInject=%q, want %q", got, "Implement AMA-200")
	}
}

// A claw that dies before its first pipeline initialization takes the
// entry-init branch on reconnect: the entry inject briefs the fresh session,
// so the armed flag must be discarded there or a later benign bridge flap
// would replay the "sandbox was replaced" brief into a live session.
func TestClearRebriefPendingDiscardsArmedFlag(t *testing.T) {
	const clawID = "claw-rebrief-clear"
	s, db := newRebriefTestServer(t, clawID, "error", "")

	if _, err := s.resetClawForRetry("test-tenant-id", clawID, "", "retrying (attempt 2/3)"); err != nil {
		t.Fatalf("resetClawForRetry: %v", err)
	}
	if pending := clawRebriefPending(t, db, clawID); pending != 1 {
		t.Fatalf("rebrief_pending=%d after retry reset, want 1", pending)
	}

	s.clearRebriefPending(clawID)
	if pending := clawRebriefPending(t, db, clawID); pending != 0 {
		t.Fatalf("rebrief_pending=%d after clear, want 0", pending)
	}
	if s.rebriefAfterRestoreIfNeeded(nil, clawID) {
		t.Fatal("rebriefAfterRestoreIfNeeded returned true after clear, want false")
	}
}

// An existing hub database predates rebrief_pending. resetClawForRetry's
// UPDATE references the column on the retry hot path, so the additive
// migration must add it (backfilled to 0 so no mid-flight claw gets a
// spurious re-brief) and absorb the re-run on every later hub restart.
func TestMigrateAddsRebriefPendingToExistingClaws(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Minimal pre-migration shape of claws; migrate()'s ALTERs add the rest.
	if _, err := db.Exec(`CREATE TABLE claws (
		id         TEXT PRIMARY KEY,
		tenant_id  TEXT NOT NULL,
		name       TEXT NOT NULL,
		template   TEXT NOT NULL DEFAULT '',
		status     TEXT NOT NULL DEFAULT 'offline',
		created_at DATETIME NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO claws(id,tenant_id,name,created_at) VALUES('c','t','claw',datetime('now'))`); err != nil {
		t.Fatal(err)
	}

	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var pending int
	if err := db.QueryRow(`SELECT rebrief_pending FROM claws WHERE id='c'`).Scan(&pending); err != nil {
		t.Fatalf("select after migration: %v", err)
	}
	if pending != 0 {
		t.Fatalf("rebrief_pending=%d on pre-existing row, want 0", pending)
	}

	// Every hub restart re-runs migrate() against a database that already has
	// the column. addColumn must absorb that, not abort startup.
	if err := migrate(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}
