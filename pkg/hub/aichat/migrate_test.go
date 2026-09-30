package aichat

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMigrate(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "chat.db")+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < 2; i++ {
		if err := Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	tables := map[string]string{
		"ai_chat_threads":           "id,tenant_id,workspace,owner_login,title,mode,topic,visibility,panel_json,created_at,updated_at,archived_at",
		"ai_chat_messages":          "id,thread_id,seq,role,content,blocks_json,interview_json,panel_json,model,input_tokens,output_tokens,status,created_at",
		"ai_chat_tool_runs":         "id,message_id,seq,tool,provider,args_json,summary,result_json,row_count,duration_ms,error,created_at",
		"ai_chat_artifacts":         "id,slug,tenant_id,workspace,owner_login,thread_id,title,current_version,created_at,updated_at,deleted_at",
		"ai_chat_artifact_versions": "artifact_id,version,message_id,path,bytes,sha256,created_at",
		"ai_chat_handoffs":          "id,thread_id,message_id,kind,provider,external_ref,url,payload_json,created_at",
	}
	for table, want := range tables {
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			names = append(names, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if strings.Join(names, ",") != want {
			t.Errorf("%s columns = %v", table, names)
		}
	}
	for index, want := range map[string][]string{
		"idx_ai_chat_threads_owner":       {"tenant_id", "owner_login", "updated_at"},
		"idx_ai_chat_messages_thread_seq": {"thread_id", "seq"},
		"idx_ai_chat_artifacts_slug":      {"slug"},
		"idx_ai_chat_artifacts_owner":     {"owner_login", "updated_at"},
	} {
		rows, err := db.Query(`SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			got = append(got, name)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s columns = %v, want %v", index, got, want)
		}
	}
	var unique bool
	if err := db.QueryRow(`SELECT "unique" FROM pragma_index_list('ai_chat_artifacts') WHERE name='idx_ai_chat_artifacts_slug'`).Scan(&unique); err != nil || !unique {
		t.Fatalf("slug unique = %v, %v", unique, err)
	}
	if _, err := db.Exec(`INSERT INTO ai_chat_threads(id, tenant_id, workspace, owner_login, mode, created_at, updated_at) VALUES('thread', 'tenant', 'workspace', 'tester', 'explore_idea', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	var visibility string
	var topic, panel sql.NullString
	if err := db.QueryRow(`SELECT visibility, topic, panel_json FROM ai_chat_threads`).Scan(&visibility, &topic, &panel); err != nil || visibility != "private" || topic.Valid || panel.Valid {
		t.Fatalf("thread defaults = %q, %v, %v, %v", visibility, topic, panel, err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM ai_chat_threads`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration changed data: %d, %v", count, err)
	}
}

func TestMigrateConcurrentOpeners(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "chat.db") + "?_txlock=immediate&_pragma=busy_timeout(5000)"
	start := make(chan struct{})
	results := make(chan error, 4)
	for i := 0; i < 4; i++ {
		go func() {
			db, err := sql.Open("sqlite", dsn)
			if err != nil {
				results <- err
				return
			}
			defer db.Close()
			<-start
			results <- Migrate(db)
		}()
	}
	close(start)
	for i := 0; i < 4; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}
