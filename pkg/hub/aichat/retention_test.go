package aichat

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
)

func execRetention(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.DB.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionArchivesByWorkspaceAndExcludesActiveTurns(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	now := time.Now()
	old := now.AddDate(0, 0, -40).UnixMilli()
	expired := testThread(t, s)
	kept, err := s.CreateThread(ctx, "tenant", "octocat", "long", "explore_idea")
	if err != nil {
		t.Fatal(err)
	}
	preparing := testThread(t, s)
	active := testThread(t, s)
	crashed := testThread(t, s)
	_, err = s.BeginTurn(ctx, active, "In flight", "fake", false)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.BeginTurn(ctx, crashed, "Interrupted", "fake", false)
	if err != nil {
		t.Fatal(err)
	}
	execRetention(t, s, `UPDATE ai_chat_messages SET created_at=? WHERE id=?`, old, m.ID)
	execRetention(t, s, `UPDATE ai_chat_threads SET updated_at=?`, old)
	runner := &Runner{Store: s}
	_, release, err := runner.Acquire(ctx, preparing)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	stats, err := s.Sweep(ctx, now, func(ws string) (config.Retention, bool) {
		if ws == "long" {
			return config.Retention{ChatsDays: 90}, true
		}
		return config.Retention{ChatsDays: 30}, true
	}, t.TempDir())
	if err != nil || stats.ArchivedThreads != 2 {
		t.Fatal(stats, err)
	}
	for _, v := range []struct {
		thread   Thread
		archived bool
	}{{expired, true}, {kept, false}, {preparing, false}, {active, false}, {crashed, true}} {
		saved, err := s.Thread(ctx, v.thread.TenantID, v.thread.OwnerLogin, v.thread.ID)
		if err != nil || (saved.ArchivedAt != nil) != v.archived {
			t.Fatal(v, saved, err)
		}
	}
	if _, _, err := runner.Acquire(ctx, expired); !errors.Is(err, ErrArchived) {
		t.Fatal("stale handler started archived thread", err)
	}
	stats, err = s.Sweep(ctx, now, func(string) (config.Retention, bool) { return config.Retention{ChatsDays: 30}, true }, t.TempDir())
	if err != nil || stats.ArchivedThreads != 1 {
		t.Fatal(stats, err)
	} // The long workspace now has the shorter window.
}

func TestRetentionFilesAndToolResults(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	thread := testThread(t, s)
	dir := t.TempDir()
	now := time.Now()
	old := now.AddDate(0, 0, -40).UnixMilli()
	publish := func(id string) Artifact {
		m, err := s.beginAction(ctx, thread, "fake", id, "artifact")
		if err != nil {
			t.Fatal(err)
		}
		a, err := s.publishArtifact(ctx, dir, thread, &m, tools.ArtifactRequest{ArtifactID: id, Title: "Prototype"}, testHTML)
		if err != nil {
			t.Fatal(err)
		}
		m.Status = "completed"
		if err = s.FinishMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		return a
	}
	first := publish("")
	second := publish(first.ID)
	current := publish(first.ID)
	deleted := publish("")
	deletedCurrent := publish(deleted.ID)
	if _, err := s.DeleteArtifact(ctx, "tenant", "octocat", deleted.Slug); err != nil {
		t.Fatal(err)
	}
	execRetention(t, s, `UPDATE ai_chat_artifact_versions SET created_at=? WHERE artifact_id=? AND version IN (1,3)`, old, first.ID)
	escaped := publish("")
	publish(escaped.ID)
	outside := filepath.Join(t.TempDir(), "outside.html")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	execRetention(t, s, `UPDATE ai_chat_artifact_versions SET path=?,created_at=? WHERE artifact_id=? AND version=1`, outside, old, escaped.ID)
	m, err := s.BeginTurn(ctx, thread, "Read", "fake", false)
	if err != nil {
		t.Fatal(err)
	}
	m.Status = "completed"
	if err = s.FinishMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old-read", "new-read"} {
		if err = s.RecordToolRun(ctx, ToolRun{ID: id, MessageID: m.ID, Seq: 1, Tool: "kb_read", Provider: "GitHub", Args: `{}`, Result: `{"data":"old"}`, Summary: "Keep summary", Error: "Keep error"}); err != nil {
			t.Fatal(err)
		}
	}
	execRetention(t, s, `UPDATE ai_chat_tool_runs SET created_at=? WHERE id='old-read'`, old)
	stats, err := s.Sweep(ctx, now, func(string) (config.Retention, bool) { return config.Retention{}, true }, dir)
	if err != nil || stats.DeletedVersions != 1 || stats.RemovedFiles != 3 || stats.ClearedToolResults != 1 || stats.SkippedFiles != 1 {
		t.Fatal(stats, err)
	}
	for _, path := range []string{first.Path, deleted.Path, deletedCurrent.Path} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("file remains", path, err)
		}
	}
	for _, path := range []string{second.Path, current.Path, outside} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("file removed", path, err)
		}
	}
	var count int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM ai_chat_artifact_versions WHERE artifact_id=?`, deleted.ID).Scan(&count); err != nil || count != 2 {
		t.Fatal("deleted artifact rows lost", count, err)
	}
	var result sql.NullString
	var summary, errorText string
	if err := s.DB.QueryRow(`SELECT result_json,summary,error FROM ai_chat_tool_runs WHERE id='old-read'`).Scan(&result, &summary, &errorText); err != nil || result.Valid || summary != "Keep summary" || errorText != "Keep error" {
		t.Fatal(result, summary, errorText, err)
	}
	if err := s.DB.QueryRow(`SELECT result_json FROM ai_chat_tool_runs WHERE id='new-read'`).Scan(&result); err != nil || !result.Valid {
		t.Fatal(result, err)
	}
	stats, err = s.Sweep(ctx, now, func(string) (config.Retention, bool) { return config.Retention{}, true }, dir)
	if err != nil || stats.ArchivedThreads != 0 || stats.RemovedFiles != 0 || stats.DeletedVersions != 0 || stats.ClearedToolResults != 0 {
		t.Fatal(stats, err)
	}
}

func TestRetentionPreservesUnknownAndDismissedTicketAttempts(t *testing.T) {
	for _, dismiss := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "dismissed"}[dismiss], func(t *testing.T) {
			h, s := ticketFixture(t, nil, ticketHTTP(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) { w.WriteHeader(502) }))
			thread := testThread(t, s)
			path := "/api/ai-chat/threads/" + thread.ID + "/actions"
			body := `{"kind":"ticket","title":"Draft","body":"Evidence","actionKey":"old-attempt"}`
			if rec := request(h, "POST", path, body, "octocat", "on"); rec.Code != 200 {
				t.Fatal(rec.Code, rec.Body)
			}
			m, _, card := latestTicket(t, s, thread)
			if !card.OutcomeUnknown {
				t.Fatal(card)
			}
			if dismiss {
				if rec := request(h, "POST", "/api/ai-chat/threads/"+thread.ID+"/ticket-attempts/"+m.ID+"/dismiss", "", "octocat", "on"); rec.Code != 204 {
					t.Fatal(rec.Code, rec.Body)
				}
			}
			ctx := context.Background()
			before, err := s.ticketActions(ctx, thread.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			execRetention(t, s, `UPDATE ai_chat_tool_runs SET created_at=?`, time.Now().AddDate(0, 0, -100).UnixMilli())
			if _, err = s.Sweep(ctx, time.Now(), func(string) (config.Retention, bool) { return config.Retention{}, true }, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			after, err := s.ticketActions(ctx, thread.ID, "")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal(before, after, err)
			}
			if (unknownTicketAction(after) == nil) != dismiss {
				t.Fatal(after)
			}
			if rec := request(h, "POST", path, body, "octocat", "on"); rec.Code != 409 {
				t.Fatal("lost duplicate guard", rec.Code, rec.Body)
			}
		})
	}
}

func TestRetentionConfigurationAndLifecycle(t *testing.T) {
	s := testStore(t)
	thread := testThread(t, s)
	dir := t.TempDir()
	ctx := context.Background()
	now := time.Now()
	execRetention(t, s, `UPDATE ai_chat_threads SET updated_at=?`, now.AddDate(0, 0, -100).UnixMilli())
	published := 0
	stage := "on"
	deps := Deps{DB: s.DB, ArtifactsDir: t.TempDir(), FeatureStage: func(context.Context, string) string { return stage }, Workspaces: func() ([]string, error) { return []string{"workspace"}, nil }, ManagedDir: func(string) string { return dir }, Publish: func(v Thread) {
		if v.ID != thread.ID {
			t.Fatal(v)
		}
		published++
	}}
	if stats, err := deps.sweep(ctx, now); err != nil || stats.ArchivedThreads != 0 {
		t.Fatal(stats, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ai_chat.yaml"), []byte("invalid: ["), 0600); err != nil {
		t.Fatal(err)
	}
	stage = "off"
	if stats, err := deps.sweep(ctx, now); err != nil || stats.ArchivedThreads != 0 {
		t.Fatal(stats, err)
	}
	stage = "on"
	if stats, err := deps.sweep(ctx, now); err != nil || stats.ArchivedThreads != 0 || published != 0 {
		t.Fatal(stats, published, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ai_chat.yaml"), []byte("about: Test"), 0600); err != nil {
		t.Fatal(err)
	}
	if stats, err := deps.sweep(ctx, now); err != nil || stats.ArchivedThreads != 1 || published != 1 {
		t.Fatal(stats, published, err)
	}
	// Cancellation before the startup delay must join promptly, without a sweep.
	cancelled, cancel := context.WithCancel(ctx)
	stop := StartRetention(cancelled, deps)
	cancel()
	stop()
	stop()
}

func TestRetentionPathConfinement(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "keep.html")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "escape")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, filepath.Join(link, "keep.html"), dir, filepath.Join(dir, "..", "sibling", "keep.html"), "relative.html"} {
		removed, unsafe, err := removeArtifactFile(dir, path)
		if err != nil || removed || !unsafe {
			t.Fatal(path, removed, unsafe, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	}
	if removed, unsafe, err := removeArtifactFile(dir, filepath.Join(dir, "missing.html")); err != nil || removed || unsafe {
		t.Fatal(removed, unsafe, err)
	}
}

func TestRetentionLoadsWorkspaceConfigAndDefaultsForMissingFile(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	dir := t.TempDir()
	now := time.Now()
	short, err := s.CreateThread(ctx, "tenant", "octocat", "short", "explore_idea")
	if err != nil {
		t.Fatal(err)
	}
	missing, err := s.CreateThread(ctx, "tenant", "octocat", "missing", "explore_idea")
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range []Thread{short, missing} {
		m, err := s.BeginTurn(ctx, thread, "Read", "fake", false)
		if err != nil {
			t.Fatal(err)
		}
		m.Status = "completed"
		if err = s.FinishMessage(ctx, m); err != nil {
			t.Fatal(err)
		}
		if err = s.RecordToolRun(ctx, ToolRun{ID: thread.ID, MessageID: m.ID, Tool: "read", Args: `{}`, Result: `{}`}); err != nil {
			t.Fatal(err)
		}
	}
	execRetention(t, s, `UPDATE ai_chat_threads SET updated_at=?`, now.AddDate(0, 0, -40).UnixMilli())
	execRetention(t, s, `UPDATE ai_chat_tool_runs SET created_at=?`, now.AddDate(0, 0, -20).UnixMilli())
	managed := filepath.Join(dir, "short")
	if err = os.MkdirAll(managed, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(managed, "ai_chat.yaml"), []byte("about: Test\nretention:\n  chats_days: 30\n  tool_results_days: 10\n  artifact_versions_days: 5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	d := Deps{DB: s.DB, ArtifactsDir: t.TempDir(), FeatureStage: func(context.Context, string) string { return "beta" }, Workspaces: func() ([]string, error) { return []string{"short", "missing"}, nil }, ManagedDir: func(ws string) string { return filepath.Join(dir, ws) }}
	stats, err := d.sweep(ctx, now)
	if err != nil || stats.ArchivedThreads != 1 || stats.ClearedToolResults != 1 {
		t.Fatal(stats, err)
	}
	kept, err := s.Thread(ctx, missing.TenantID, missing.OwnerLogin, missing.ID)
	if err != nil || kept.ArchivedAt != nil {
		t.Fatal(kept, err)
	}
	var result sql.NullString
	if err = s.DB.QueryRow(`SELECT result_json FROM ai_chat_tool_runs WHERE id=?`, missing.ID).Scan(&result); err != nil || !result.Valid {
		t.Fatal(result, err)
	}
}

func retentionArtifact(t *testing.T, s *Store, thread Thread, dir, id string) Artifact {
	t.Helper()
	ctx := context.Background()
	m, err := s.beginAction(ctx, thread, "fake", id, "artifact")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.publishArtifact(ctx, dir, thread, &m, tools.ArtifactRequest{ArtifactID: id, Title: "Prototype"}, testHTML)
	if err != nil {
		t.Fatal(err)
	}
	m.Status = "completed"
	if err := s.FinishMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestRetentionContinuesAfterVersionFailure(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	dir := t.TempDir()
	now := time.Now()
	var blocked, expired, current []Artifact
	for _, workspace := range []string{"first", "second"} {
		thread, err := s.CreateThread(ctx, "tenant", "octocat", workspace, "explore_idea")
		if err != nil {
			t.Fatal(err)
		}
		a := retentionArtifact(t, s, thread, dir, "")
		blocked = append(blocked, a)
		expired = append(expired, retentionArtifact(t, s, thread, dir, a.ID))
		current = append(current, retentionArtifact(t, s, thread, dir, a.ID))
		if err := os.Remove(a.Path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(a.Path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(a.Path, "keep"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	execRetention(t, s, `UPDATE ai_chat_artifact_versions SET created_at=?`, now.AddDate(0, 0, -40).UnixMilli())
	stats, err := s.Sweep(ctx, now, func(string) (config.Retention, bool) { return config.Retention{}, true }, dir)
	if err != nil || stats.Errors != 2 || stats.DeletedVersions != 2 || stats.RemovedFiles != 2 {
		t.Fatal(stats, err)
	}
	for _, a := range expired {
		if _, err := os.Stat(a.Path); !os.IsNotExist(err) {
			t.Fatal("later version was not swept", err)
		}
		if _, err := s.Artifact(ctx, a.Tenant, a.Slug, a.Version); !errors.Is(err, sql.ErrNoRows) {
			t.Fatal("expired version row remains", err)
		}
	}
	for _, a := range append(blocked, current...) {
		if _, err := os.Stat(a.Path); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Artifact(ctx, a.Tenant, a.Slug, a.Version); err != nil {
			t.Fatal("failed or current version row lost", err)
		}
	}
}

func TestRetentionSkipsUnavailableWorkspaceConfig(t *testing.T) {
	for _, invalid := range []string{"malformed", "unreadable"} {
		t.Run(invalid, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t)
			dir, files := t.TempDir(), t.TempDir()
			now := time.Now()
			threads := map[string]Thread{}
			versions := map[string]Artifact{}
			for _, workspace := range []string{"invalid", "valid", "missing"} {
				thread, err := s.CreateThread(ctx, "tenant", "octocat", workspace, "explore_idea")
				if err != nil {
					t.Fatal(err)
				}
				threads[workspace] = thread
				a := retentionArtifact(t, s, thread, files, "")
				versions[workspace] = a
				retentionArtifact(t, s, thread, files, a.ID)
				messages, err := s.Messages(ctx, thread.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.RecordToolRun(ctx, ToolRun{ID: workspace, MessageID: messages[0].ID, Tool: "read", Args: `{}`, Result: `{}`}); err != nil {
					t.Fatal(err)
				}
				managed := filepath.Join(dir, workspace)
				if err := os.Mkdir(managed, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(managed, "ai_chat.yaml")
				switch {
				case workspace == "valid":
					err = os.WriteFile(path, []byte("about: Test"), 0600)
				case workspace == "invalid" && invalid == "malformed":
					err = os.WriteFile(path, []byte("retention: ["), 0600)
				case workspace == "invalid":
					// A directory is unreadable as YAML, even when tests run as root.
					err = os.Mkdir(path, 0700)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			old := now.AddDate(0, 0, -100).UnixMilli()
			execRetention(t, s, `UPDATE ai_chat_threads SET updated_at=?`, old)
			execRetention(t, s, `UPDATE ai_chat_tool_runs SET created_at=? WHERE tool='read'`, old)
			execRetention(t, s, `UPDATE ai_chat_artifact_versions SET created_at=?`, old)
			d := Deps{DB: s.DB, ArtifactsDir: files,
				FeatureStage: func(context.Context, string) string { return "on" },
				Workspaces:   func() ([]string, error) { return []string{"invalid", "valid", "missing"}, nil },
				ManagedDir:   func(ws string) string { return filepath.Join(dir, ws) },
			}
			stats, err := d.sweep(ctx, now)
			if err != nil || stats.ArchivedThreads != 2 || stats.ClearedToolResults != 2 || stats.DeletedVersions != 2 || stats.RemovedFiles != 2 {
				t.Fatal(stats, err)
			}
			for workspace, thread := range threads {
				kept := workspace == "invalid"
				saved, err := s.Thread(ctx, thread.TenantID, thread.OwnerLogin, thread.ID)
				if err != nil || (saved.ArchivedAt == nil) != kept {
					t.Fatal(workspace, saved, err)
				}
				var result sql.NullString
				if err := s.DB.QueryRow(`SELECT result_json FROM ai_chat_tool_runs WHERE id=?`, workspace).Scan(&result); err != nil || result.Valid != kept {
					t.Fatal(workspace, result, err)
				}
				a := versions[workspace]
				if _, err := os.Stat(a.Path); (err == nil) != kept || (err != nil && !os.IsNotExist(err)) {
					t.Fatal(workspace, err)
				}
				_, err = s.Artifact(ctx, a.Tenant, a.Slug, 1)
				if (err == nil) != kept || (err != nil && !errors.Is(err, sql.ErrNoRows)) {
					t.Fatal(workspace, err)
				}
			}
		})
	}
}

func TestRetentionSweepPanicDoesNotEscape(t *testing.T) {
	d := Deps{FeatureStage: func(context.Context, string) string { panic("boom") }}
	d.sweepSafely(context.Background(), time.Now())
}
