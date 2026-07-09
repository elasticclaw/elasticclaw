package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

// newTestStore opens a migrated in-memory database on a single
// connection so every goroutine sees the same DB.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	st.DB().SetMaxOpenConns(1)
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.DB().Exec(
		`INSERT INTO tenants(id,name,token,claw_token,created_at) VALUES('t1','Tenant','tok','claw-tok',?)`,
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return st
}

func mustCreateClaw(t *testing.T, st *Store, id string) {
	t.Helper()
	err := st.Claws().Create(context.Background(), CreateParams{
		ID: id, TenantID: "t1", Name: "claw-" + id, Provider: "docker",
		TemplateFiles: "{}", GitHubRepos: "[]", Tags: []string{"team=core"},
		Status: "provisioning", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("create claw: %v", err)
	}
}

func TestClawsRepoLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	claws := st.Claws()

	mustCreateClaw(t, st, "claw-a")

	got, err := claws.Get(ctx, "claw-a", "t1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "claw-claw-a" || got.Status != "provisioning" {
		t.Fatalf("unexpected claw: %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "team=core" {
		t.Fatalf("tags not round-tripped: %v", got.Tags)
	}

	if err := claws.SetName(ctx, "claw-a", "t1", "renamed"); err != nil {
		t.Fatalf("set name: %v", err)
	}
	if err := claws.SetColor(ctx, "claw-a", "t1", "#123456"); err != nil {
		t.Fatalf("set color: %v", err)
	}
	tags, err := claws.Tags(ctx, "claw-a", "t1")
	if err != nil || len(tags) != 1 {
		t.Fatalf("tags: %v %v", tags, err)
	}
	if _, err := claws.Tags(ctx, "missing", "t1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected ErrNoRows for missing claw, got %v", err)
	}

	if id := claws.ResolveID(ctx, "t1", "claw-a"[:6]); id != "claw-a" {
		t.Fatalf("resolve prefix: %q", id)
	}

	list, err := claws.List(ctx, "t1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}

	n, err := claws.SoftDelete(ctx, "claw-a", "t1")
	if err != nil || n != 1 {
		t.Fatalf("soft delete: n=%d err=%v", n, err)
	}
	n, err = claws.SoftDelete(ctx, "claw-a", "t1")
	if err != nil || n != 0 {
		t.Fatalf("second soft delete should affect 0 rows: n=%d err=%v", n, err)
	}
	if _, err := claws.Get(ctx, "claw-a", "t1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted claw should be invisible, got %v", err)
	}
}

func TestClawsRepoRegisterUpsertKeepsStickyStates(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	claws := st.Claws()
	at := time.Now().UTC()

	status, err := claws.RegisterUpsert(ctx, "claw-b", "t1", "n", "tmpl", "connected", at)
	if err != nil || status != "connected" {
		t.Fatalf("first upsert: %q %v", status, err)
	}

	// idle is sticky: a reconnect must not flip it back to connected.
	if _, err := st.DB().Exec(`UPDATE claws SET status='idle' WHERE id='claw-b'`); err != nil {
		t.Fatal(err)
	}
	status, err = claws.RegisterUpsert(ctx, "claw-b", "t1", "n", "tmpl", "connected", at)
	if err != nil || status != "idle" {
		t.Fatalf("sticky idle: %q %v", status, err)
	}
}

func TestClawsRepoPurgeConversationIsTransactional(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	mustCreateClaw(t, st, "claw-c")

	msg := types.HubMessage{ID: "m1", ClawID: "claw-c", TenantID: "t1", Role: "user", Content: "hi", CreatedAt: time.Now().UTC()}
	if err := st.Messages().Insert(ctx, msg); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO claw_prs(id,claw_id,repo,pr_number,pr_url,created_at) VALUES('pr1','claw-c','o/r',1,'u',?)`,
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("insert claw_pr: %v", err)
	}

	if err := st.Claws().PurgeConversation(ctx, "claw-c"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	var msgs, prs int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id='claw-c'`).Scan(&msgs)
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM claw_prs WHERE claw_id='claw-c'`).Scan(&prs)
	if msgs != 0 || prs != 0 {
		t.Fatalf("purge left rows: messages=%d prs=%d", msgs, prs)
	}
}

func TestMessagesRepoVisibleWindowAndActivity(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	msgs := st.Messages()
	mustCreateClaw(t, st, "claw-d")

	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	insert := func(id, role, content string, at time.Time) {
		t.Helper()
		if err := msgs.Insert(ctx, types.HubMessage{
			ID: id, ClawID: "claw-d", TenantID: "t1", Role: role, Content: content, CreatedAt: at,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	insert("m1", "user", "hello", base)
	insert("m2", "claw", "world", base.Add(1*time.Minute))
	insert("m3", "system", "[WAKE]", base.Add(2*time.Minute)) // hidden marker
	insert("m4", "activity", "ran tool", base.Add(3*time.Minute))
	insert("m5", "user", "again", base.Add(4*time.Minute))

	hidden := []string{"[WAKE]"}
	page, err := msgs.ListVisible(ctx, VisibleWindow{
		ClawID: "claw-d", TenantID: "t1", Limit: 10, HiddenSystemContents: hidden,
	})
	if err != nil {
		t.Fatalf("list visible: %v", err)
	}
	if len(page) != 4 { // m3 excluded, DESC order
		t.Fatalf("expected 4 visible messages, got %d", len(page))
	}
	if page[0].ID != "m5" {
		t.Fatalf("expected newest-first, got %s", page[0].ID)
	}

	conv, err := msgs.ListConversation(ctx, "claw-d", "t1", nil, 10, hidden)
	if err != nil || len(conv) != 3 { // activity + marker excluded
		t.Fatalf("conversation: %d %v", len(conv), err)
	}

	has, err := msgs.HasConversationBefore(ctx, "claw-d", "t1", base.Add(30*time.Second), hidden)
	if err != nil || !has {
		t.Fatalf("has-before: %v %v", has, err)
	}

	acts, err := msgs.ListActivity(ctx, ActivityWindow{ClawID: "claw-d", TenantID: "t1", Limit: 10})
	if err != nil || len(acts) != 1 || acts[0].ID != "m4" {
		t.Fatalf("activity: %+v %v", acts, err)
	}

	count, minC, maxC, err := msgs.ActivityStats(ctx, "claw-d", "t1", nil, nil)
	if err != nil || count != 1 || minC == "" || maxC == "" {
		t.Fatalf("activity stats: %d %q %q %v", count, minC, maxC, err)
	}
}

func TestMessagesRepoUpsertReplacesContent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	mustCreateClaw(t, st, "claw-e")

	m := types.HubMessage{ID: "s1", ClawID: "claw-e", TenantID: "t1", Role: "claw", Content: "partial", CreatedAt: time.Now().UTC()}
	if err := st.Messages().Upsert(ctx, m); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	m.Content = "partial + more"
	if err := st.Messages().Upsert(ctx, m); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	var content string
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM messages WHERE claw_id='claw-e'`).Scan(&n)
	_ = st.DB().QueryRow(`SELECT content FROM messages WHERE id='s1'`).Scan(&content)
	if n != 1 || content != "partial + more" {
		t.Fatalf("upsert: n=%d content=%q", n, content)
	}
}

func TestSettingsRepoTemplates(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	settings := st.Settings()
	at := time.Now().UTC()

	if err := settings.UpsertTemplate(ctx, "tmpl-a", `{"a":"1"}`, at); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := settings.UpsertTemplate(ctx, "tmpl-a", `{"a":"2"}`, at.Add(time.Minute)); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	files, err := settings.TemplateFiles(ctx, "tmpl-a")
	if err != nil || files != `{"a":"2"}` {
		t.Fatalf("files: %q %v", files, err)
	}
	names, err := settings.TemplateNames(ctx)
	if err != nil || len(names) != 1 || names[0] != "tmpl-a" {
		t.Fatalf("names: %v %v", names, err)
	}
	list, err := settings.ListTemplates(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "tmpl-a" {
		t.Fatalf("list: %v %v", list, err)
	}
	if err := settings.DeleteTemplate(ctx, "tmpl-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := settings.TemplateFiles(ctx, "tmpl-a"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected ErrNoRows after delete, got %v", err)
	}
}

func TestTenantsRepo(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	tenants := st.Tenants()

	id, err := tenants.FirstID(ctx)
	if err != nil || id != "t1" {
		t.Fatalf("first: %q %v", id, err)
	}
	id, err = tenants.IDByToken(ctx, "tok")
	if err != nil || id != "t1" {
		t.Fatalf("by token: %q %v", id, err)
	}
	id, err = tenants.IDByClawToken(ctx, "claw-tok")
	if err != nil || id != "t1" {
		t.Fatalf("by claw token: %q %v", id, err)
	}
	if _, err := tenants.IDByToken(ctx, "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected ErrNoRows, got %v", err)
	}
}

func TestWithTxRollsBackOnError(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	mustCreateClaw(t, st, "claw-f")

	sentinel := errors.New("boom")
	err := st.WithTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE claws SET name='changed' WHERE id='claw-f'`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
	var name string
	_ = st.DB().QueryRow(`SELECT name FROM claws WHERE id='claw-f'`).Scan(&name)
	if name != "claw-claw-f" {
		t.Fatalf("rollback failed: name=%q", name)
	}
}

func TestWithTxDoesNotReplayFnOnBusyCommit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	mustCreateClaw(t, st, "claw-g")

	busyErr := errors.New("database is locked (5) (SQLITE_BUSY)")
	origCommit := commitTx
	commitTx = func(tx *sql.Tx) error {
		_ = tx.Rollback() // leave the connection clean; the fake commit never ran
		return busyErr
	}
	defer func() { commitTx = origCommit }()

	fnRuns := 0
	err := st.WithTx(ctx, func(tx *sql.Tx) error {
		fnRuns++
		_, err := tx.ExecContext(ctx, `UPDATE claws SET name='replayed' WHERE id='claw-g'`)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "SQLITE_BUSY") {
		t.Fatalf("expected the busy commit error to reach the caller, got %v", err)
	}
	if fnRuns != 1 {
		t.Fatalf("fn replayed after failed commit: runs=%d", fnRuns)
	}
}

func TestQueryScanRetriesBusyDuringScan(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	mustCreateClaw(t, st, "claw-h")

	scans := 0
	var ids []string
	err := st.queryScan(ctx, `SELECT id FROM claws`, nil, func(rows *sql.Rows) error {
		scans++
		if scans == 1 {
			// Simulate SQLITE_BUSY surfacing mid-iteration (rows.Err()).
			return errors.New("database is locked (5) (SQLITE_BUSY)")
		}
		ids = ids[:0]
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("queryScan: %v", err)
	}
	if scans != 2 {
		t.Fatalf("expected a retry after busy scan, got %d attempts", scans)
	}
	if len(ids) != 1 || ids[0] != "claw-h" {
		t.Fatalf("unexpected rows after retry: %v", ids)
	}
}

func TestRetryBusyRetriesThenSucceeds(t *testing.T) {
	attempts := 0
	err := retryBusy(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return errors.New("database is locked (5) (SQLITE_BUSY)")
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("retry: attempts=%d err=%v", attempts, err)
	}
}

func TestRetryBusyDoesNotRetryOtherErrors(t *testing.T) {
	attempts := 0
	sentinel := errors.New("constraint failed")
	err := retryBusy(context.Background(), func() error {
		attempts++
		return sentinel
	})
	if !errors.Is(err, sentinel) || attempts != 1 {
		t.Fatalf("non-busy error retried: attempts=%d err=%v", attempts, err)
	}
}
