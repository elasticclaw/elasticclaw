package aichat

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/config"
)

type api struct {
	deps   Deps
	store  *Store
	runner *Runner
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return false
	}
	return true
}
func (a *api) identity(r *http.Request) (string, string) {
	return a.deps.TenantID(r), OwnerLogin(a.deps.CallerLogin(r))
}
func (a *api) owned(w http.ResponseWriter, r *http.Request) (Thread, bool) {
	tenant, owner := a.identity(r)
	t, err := a.store.Thread(r.Context(), tenant, owner, r.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return t, false
	}
	if err != nil {
		http.Error(w, "unable to load thread", 500)
		return t, false
	}
	return t, true
}
func (a *api) loadConfig(workspace string) (*config.Config, error) {
	names, err := a.deps.Workspaces()
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if name == workspace {
			return config.Load(a.deps.ManagedDir(name))
		}
	}
	return nil, errors.New("unknown workspace")
}
func (a *api) publish(t Thread) {
	if a.deps.Publish != nil {
		a.deps.Publish(t)
	}
}
func (a *api) listThreads(w http.ResponseWriter, r *http.Request) {
	tenant, owner := a.identity(r)
	threads, err := a.store.Threads(r.Context(), tenant, owner)
	if err != nil {
		http.Error(w, "unable to list threads", 500)
		return
	}
	jsonResponse(w, 200, threads)
}
func (a *api) createThread(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Workspace string `json:"workspace"`
		Mode      string `json:"mode"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Mode == "" {
		body.Mode = "explore_idea"
	}
	switch body.Mode {
	case "explore_idea", "validate_hypothesis", "ask_the_data", "write_brief":
	default:
		http.Error(w, "invalid mode", 400)
		return
	}
	if _, err := a.loadConfig(body.Workspace); err != nil {
		http.Error(w, "Not configured for this workspace", 400)
		return
	}
	tenant, owner := a.identity(r)
	t, err := a.store.CreateThread(r.Context(), tenant, owner, body.Workspace, body.Mode)
	if err != nil {
		http.Error(w, "unable to create thread", 500)
		return
	}
	a.publish(t)
	jsonResponse(w, 201, t)
}
func (a *api) getThread(w http.ResponseWriter, r *http.Request) {
	t, ok := a.owned(w, r)
	if !ok {
		return
	}
	if err := a.runner.RecoverInterrupted(r.Context(), t); err != nil {
		http.Error(w, "unable to recover interrupted turn", 500)
		return
	}
	messages, err := a.store.Messages(r.Context(), t.ID, 0)
	if err != nil {
		http.Error(w, "unable to load messages", 500)
		return
	}
	jsonResponse(w, 200, map[string]any{"thread": t, "messages": messages})
}
func (a *api) patchThread(w http.ResponseWriter, r *http.Request) {
	t, ok := a.owned(w, r)
	if !ok {
		return
	}
	var body struct {
		Title    *string `json:"title"`
		Archived *bool   `json:"archived"`
	}
	if !decode(w, r, &body) {
		return
	}
	if (body.Title == nil && body.Archived == nil) || (body.Title != nil && (strings.TrimSpace(*body.Title) == "" || utf8.RuneCountInString(*body.Title) > 200)) {
		http.Error(w, "invalid thread update", 400)
		return
	}
	if err := a.store.UpdateThread(r.Context(), t, body.Title, body.Archived); err != nil {
		http.Error(w, "unable to update thread", 500)
		return
	}
	a.publish(t)
	w.WriteHeader(http.StatusNoContent)
}
func (a *api) cancelTurn(w http.ResponseWriter, r *http.Request) {
	t, ok := a.owned(w, r)
	if !ok {
		return
	}
	a.runner.Cancel(t)
	w.WriteHeader(http.StatusNoContent)
}
func (a *api) sendMessage(w http.ResponseWriter, r *http.Request) {
	t, ok := a.owned(w, r)
	if !ok {
		return
	}
	if t.ArchivedAt != nil {
		http.Error(w, "thread is archived", 409)
		return
	}
	var body struct {
		Text  string `json:"text"`
		Retry bool   `json:"retry"`
	}
	if !decode(w, r, &body) {
		return
	}
	body.Text = strings.TrimSpace(body.Text)
	if (!body.Retry && body.Text == "") || len(body.Text) > 32*1024 {
		http.Error(w, "message must contain 1 to 32768 bytes", 400)
		return
	}
	ctx, release, err := a.runner.Acquire(r.Context(), t)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, ErrBusy.Error(), 429)
		return
	}
	defer release()
	cfg, err := a.loadConfig(t.Workspace)
	if err != nil {
		http.Error(w, "Not configured for this workspace", 400)
		return
	}
	provider, err := a.deps.LLM(cfg.LLM)
	if err != nil {
		http.Error(w, "No usable LLM key configured for this workspace", 503)
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming unavailable", 500)
		return
	}
	m, err := a.store.BeginTurn(ctx, t, body.Text, provider.Model(), body.Retry)
	if errors.Is(err, ErrRetry) {
		http.Error(w, err.Error(), 409)
		return
	}
	if err != nil {
		http.Error(w, "unable to save message", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	_ = a.runner.Run(ctx, t, m, cfg, provider, streamWriter(w))
	release()
	a.publish(t)
}
