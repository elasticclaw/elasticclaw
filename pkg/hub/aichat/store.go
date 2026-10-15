package aichat

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Thread struct {
	ID         string `json:"id"`
	TenantID   string `json:"-"`
	Workspace  string `json:"workspace"`
	OwnerLogin string `json:"-"`
	Title      string `json:"title"`
	Mode       string `json:"mode"`
	CreatedAt  int64  `json:"createdAt"`
	UpdatedAt  int64  `json:"updatedAt"`
	ArchivedAt *int64 `json:"archivedAt"`
}
type Message struct {
	Blocks       json.RawMessage `json:"blocks,omitempty"`
	Interview    json.RawMessage `json:"interview,omitempty"`
	ID           string          `json:"id"`
	ThreadID     string          `json:"threadId"`
	Seq          int             `json:"seq"`
	Role         string          `json:"role"`
	Content      string          `json:"content"`
	Model        string          `json:"model"`
	InputTokens  int             `json:"inputTokens"`
	OutputTokens int             `json:"outputTokens"`
	Status       string          `json:"status"`
	CreatedAt    int64           `json:"createdAt"`
}
type ToolRun struct {
	ID         string
	MessageID  string
	Seq        int
	Tool       string
	Provider   string
	Args       string
	Summary    string
	Result     string
	RowCount   int
	DurationMS int64
	Error      string
}
type Store struct{ DB *sql.DB }

const threadColumns = `id,tenant_id,workspace,owner_login,title,mode,created_at,updated_at,archived_at`

type scanner interface{ Scan(...any) error }

func scanThread(row scanner) (Thread, error) {
	var t Thread
	err := row.Scan(&t.ID, &t.TenantID, &t.Workspace, &t.OwnerLogin, &t.Title, &t.Mode, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt)
	return t, err
}
func (s *Store) CreateThread(ctx context.Context, tenant, owner, workspace, mode string) (Thread, error) {
	now := time.Now().UnixMilli()
	t := Thread{ID: uuid.NewString(), TenantID: tenant, OwnerLogin: OwnerLogin(owner), Workspace: workspace, Mode: mode, CreatedAt: now, UpdatedAt: now}
	_, err := s.DB.ExecContext(ctx, `INSERT INTO ai_chat_threads(id,tenant_id,workspace,owner_login,mode,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, t.ID, tenant, t.Workspace, t.OwnerLogin, mode, now, now)
	return t, err
}
func (s *Store) Thread(ctx context.Context, tenant, owner, id string) (Thread, error) {
	return scanThread(s.DB.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM ai_chat_threads WHERE id=? AND tenant_id=? AND owner_login=?`, id, tenant, OwnerLogin(owner)))
}
func (s *Store) Threads(ctx context.Context, tenant, owner string) ([]Thread, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+threadColumns+` FROM ai_chat_threads WHERE tenant_id=? AND owner_login=? AND archived_at IS NULL ORDER BY updated_at DESC,id DESC`, tenant, OwnerLogin(owner))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	threads := []Thread{}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		threads = append(threads, t)
	}
	return threads, rows.Err()
}
func (s *Store) UpdateThread(ctx context.Context, t Thread, title *string, archived *bool) error {
	fields := []string{"updated_at=?"}
	args := []any{time.Now().UnixMilli()}
	if title != nil {
		fields = append(fields, "title=?")
		args = append(args, strings.TrimSpace(*title))
	}
	if archived != nil {
		var archivedAt *int64
		if *archived {
			now := time.Now().UnixMilli()
			archivedAt = &now
		}
		fields = append(fields, "archived_at=?")
		args = append(args, archivedAt)
	}
	args = append(args, t.ID, t.TenantID, t.OwnerLogin)
	_, err := s.DB.ExecContext(ctx, `UPDATE ai_chat_threads SET `+strings.Join(fields, ",")+` WHERE id=? AND tenant_id=? AND owner_login=?`, args...)
	return err
}
func (s *Store) Messages(ctx context.Context, threadID string, limit int) ([]Message, error) {
	return s.messages(ctx, threadID, limit, false)
}
func (s *Store) History(ctx context.Context, threadID string) ([]Message, error) {
	return s.messages(ctx, threadID, 40, true)
}
func (s *Store) messages(ctx context.Context, threadID string, limit int, completedOnly bool) ([]Message, error) {
	query := `SELECT id,thread_id,seq,role,content,COALESCE(model,''),input_tokens,output_tokens,status,created_at,blocks_json,interview_json FROM ai_chat_messages WHERE thread_id=?`
	if completedOnly {
		query += ` AND (status='completed' OR (status IN ('error','cancelled') AND EXISTS(SELECT 1 FROM json_each(COALESCE(blocks_json,'[]')) WHERE json_extract(value,'$.type')='action')))`
	}
	query += ` ORDER BY seq DESC`
	args := []any{threadID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []Message{}
	for rows.Next() {
		var m Message
		var blocks, interview sql.NullString
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.Seq, &m.Role, &m.Content, &m.Model, &m.InputTokens, &m.OutputTokens, &m.Status, &m.CreatedAt, &blocks, &interview); err != nil {
			return nil, err
		}
		m.Blocks = json.RawMessage(blocks.String)
		m.Interview = json.RawMessage(interview.String)
		messages = append(messages, m)
	}
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return messages, rows.Err()
}

var ErrRetry = errors.New("no failed turn to retry")

const interruptMessagesSQL = `UPDATE ai_chat_messages SET status='error', blocks_json=CASE
 WHEN EXISTS(SELECT 1 FROM ai_chat_tool_runs WHERE message_id=ai_chat_messages.id AND tool='ticket')
 AND NOT EXISTS(SELECT 1 FROM json_each(COALESCE(blocks_json,'[]')) WHERE json_extract(value,'$.type')='action' AND json_extract(value,'$.kind')='ticket')
 THEN json_insert(COALESCE(blocks_json,'[]'),'$[#]',json_object('id',id||'-ticket','type','action','kind','ticket','state','failed','at',length(content),'card',json_patch(COALESCE((SELECT args_json FROM ai_chat_tool_runs WHERE message_id=ai_chat_messages.id AND tool='ticket' LIMIT 1),'{}'),json_object('error',CASE
 WHEN EXISTS(SELECT 1 FROM ai_chat_tool_runs WHERE message_id=ai_chat_messages.id AND tool='ticket' AND json_extract(NULLIF(result_json,''),'$.requestSent')=1)
 THEN COALESCE((SELECT json_extract(args_json,'$.tracker') FROM ai_chat_tool_runs WHERE message_id=ai_chat_messages.id AND tool='ticket' LIMIT 1),'The tracker') || ' did not confirm the ticket. Check ' || COALESCE((SELECT json_extract(args_json,'$.tracker') FROM ai_chat_tool_runs WHERE message_id=ai_chat_messages.id AND tool='ticket' LIMIT 1),'the tracker') || ' before trying again.'
 ELSE 'Stopped. Nothing was created.' END,'outcomeUnknown',json(CASE WHEN EXISTS(SELECT 1 FROM ai_chat_tool_runs WHERE message_id=ai_chat_messages.id AND tool='ticket' AND json_extract(NULLIF(result_json,''),'$.requestSent')=1) THEN 'true' ELSE 'false' END)))))
 WHEN EXISTS(SELECT 1 FROM ai_chat_tool_runs WHERE message_id=ai_chat_messages.id AND tool='artifact')
 AND NOT EXISTS(SELECT 1 FROM json_each(COALESCE(blocks_json,'[]')) WHERE json_extract(value,'$.type')='action' AND json_extract(value,'$.kind')='artifact')
 THEN json_insert(COALESCE(blocks_json,'[]'),'$[#]',json_object('id',id||'-artifact','type','action','kind','artifact','state','failed','at',length(content),'card',json_patch(json_object('title','HTML artifact'),json_patch(COALESCE((SELECT args_json FROM ai_chat_tool_runs WHERE message_id=ai_chat_messages.id AND tool='artifact' LIMIT 1),'{}'),json_object('error','The generation was interrupted.')))))
 ELSE blocks_json END WHERE thread_id=? AND role='assistant' AND status='streaming'`

// RecoverInterrupted requires the runner to exclude an active turn for this thread.
func (s *Store) RecoverInterrupted(ctx context.Context, threadID string) error {
	_, err := s.DB.ExecContext(ctx, interruptMessagesSQL, threadID)
	return err
}

// BeginTurn saves the user and pending assistant atomically. The runner owns the
// thread's in-flight slot before calling it, so sequence numbers cannot race.
func (s *Store) BeginTurn(ctx context.Context, t Thread, text, model string, retry bool) (Message, error) {
	return s.beginTurn(ctx, t, text, model, retry, nil)
}
func (s *Store) beginTurn(ctx context.Context, t Thread, text, model string, retry bool, answers *InterviewAnswers) (Message, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()
	var summary json.RawMessage
	if answers != nil {
		var answerErr error
		text, summary, answerErr = acceptInterview(ctx, tx, t.ID, *answers)
		if answerErr != nil {
			return Message{}, answerErr
		}
	}
	if _, err := tx.ExecContext(ctx, interruptMessagesSQL, t.ID); err != nil {
		return Message{}, err
	}
	var seq int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM ai_chat_messages WHERE thread_id=?`, t.ID).Scan(&seq); err != nil {
		return Message{}, err
	}
	now := time.Now().UnixMilli()
	if retry {
		var role, status string
		if err := tx.QueryRowContext(ctx, `SELECT role,status FROM ai_chat_messages WHERE thread_id=? ORDER BY seq DESC LIMIT 1`, t.ID).Scan(&role, &status); err != nil {
			return Message{}, ErrRetry
		}
		if role != "assistant" || (status != "error" && status != "cancelled" && status != "limit") {
			return Message{}, ErrRetry
		}
	} else {
		seq++
		if _, err := tx.ExecContext(ctx, `INSERT INTO ai_chat_messages(id,thread_id,seq,role,content,status,created_at,interview_json) VALUES(?,?,?,'user',?,'completed',?,?)`, uuid.NewString(), t.ID, seq, text, now, nullableJSON(summary)); err != nil {
			return Message{}, err
		}
	}
	m := Message{ID: uuid.NewString(), ThreadID: t.ID, Seq: seq + 1, Role: "assistant", Model: model, Status: "streaming", CreatedAt: now}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ai_chat_messages(id,thread_id,seq,role,model,status,created_at) VALUES(?,?,?,'assistant',?,'streaming',?)`, m.ID, t.ID, m.Seq, model, now); err != nil {
		return Message{}, err
	}
	title := []rune(strings.Join(strings.Fields(text), " "))
	if len(title) > 80 {
		title = title[:80]
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ai_chat_threads SET title=CASE WHEN title='' AND ?=1 THEN ? ELSE title END,updated_at=? WHERE id=?`, seq, string(title), now, t.ID); err != nil {
		return Message{}, err
	}
	return m, tx.Commit()
}
func (s *Store) FinishMessage(ctx context.Context, m Message) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE ai_chat_messages SET content=?,model=?,input_tokens=?,output_tokens=?,status=?,blocks_json=?,interview_json=? WHERE id=?`, m.Content, m.Model, m.InputTokens, m.OutputTokens, m.Status, nullableJSON(m.Blocks), nullableJSON(m.Interview), m.ID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ai_chat_threads SET updated_at=? WHERE id=?`, time.Now().UnixMilli(), m.ThreadID); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) RecordToolRun(ctx context.Context, run ToolRun) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO ai_chat_tool_runs(id,message_id,seq,tool,provider,args_json,summary,result_json,row_count,duration_ms,error,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, run.MessageID, run.Seq, run.Tool, run.Provider, run.Args, run.Summary, run.Result, run.RowCount, run.DurationMS, run.Error, time.Now().UnixMilli())
	return err
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
