package aichat

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/llm"
	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
	"github.com/google/uuid"
)

const artifactCSP = "default-src 'none'; script-src 'unsafe-inline' https://cdnjs.cloudflare.com https://cdn.jsdelivr.net; style-src 'unsafe-inline' https://fonts.googleapis.com; font-src data: https://fonts.gstatic.com; img-src data:; connect-src 'none'; frame-ancestors 'self'; sandbox allow-scripts"
const artifactURLLifetime = 15 * time.Minute

func artifactSignature(secret, tenant, slug string, version int, exp int64, download string) string {
	// Domain separation prevents artifact signatures from being web session tokens.
	key := hmac.New(sha256.New, []byte(secret))
	key.Write([]byte("ai-chat-artifact-urls-v1"))
	mac := hmac.New(sha256.New, key.Sum(nil))
	fmt.Fprintf(mac, "%s|%s|%d|%d|%s", tenant, slug, version, exp, download)
	return hex.EncodeToString(mac.Sum(nil))
}
func (a *api) signedArtifactURL(v Artifact, download string) string {
	exp := time.Now().Add(artifactURLLifetime).Unix()
	q := url.Values{"v": {strconv.Itoa(v.Version)}, "exp": {strconv.FormatInt(exp, 10)}, "tenant": {v.Tenant}, "download": {download}}
	q.Set("sig", artifactSignature(a.deps.ArtifactKey(), v.Tenant, v.Slug, v.Version, exp, download))
	return "/artifacts/" + v.Slug + "/raw?" + q.Encode()
}
func (a *api) getArtifact(w http.ResponseWriter, r *http.Request) {
	version := 0
	if raw := r.URL.Query().Get("v"); raw != "" {
		var err error
		version, err = strconv.Atoi(raw)
		if err != nil || version < 1 {
			http.NotFound(w, r)
			return
		}
	}
	tenant, _ := a.identity(r)
	v, err := a.store.Artifact(r.Context(), tenant, r.PathValue("slug"), version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "Unable to load artifact.", 500)
		}
		return
	}
	if a.deps.ArtifactKey == nil || a.deps.ArtifactKey() == "" {
		http.Error(w, "Artifact signing is unavailable.", 503)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonResponse(w, 200, struct {
		Artifact
		RawURL      string `json:"rawUrl"`
		DownloadURL string `json:"downloadUrl"`
		ShareURL    string `json:"shareUrl"`
	}{v, a.signedArtifactURL(v, "0"), a.signedArtifactURL(v, "1"), a.runner.shareURL(v.Slug)})
}

// RawArtifactRoutes authenticates only signed capabilities minted by the gated
// JSON endpoint. A beta capability remains usable for 15 minutes, but turning
// the feature off revokes all links immediately.
func RawArtifactRoutes(deps Deps) http.Handler {
	a := &api{deps: deps, store: &Store{DB: deps.DB}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /artifacts/{slug}/raw", a.rawArtifact)
	return mux
}
func (a *api) rawArtifact(w http.ResponseWriter, r *http.Request) {
	// The signed URL is the capability; no other origin needs to read it.
	for _, header := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Headers", "Access-Control-Allow-Methods"} {
		w.Header().Del(header)
	}
	q := r.URL.Query()
	slug := r.PathValue("slug")
	tenant := q.Get("tenant")
	version, e1 := strconv.Atoi(q.Get("v"))
	exp, e2 := strconv.ParseInt(q.Get("exp"), 10, 64)
	download := q.Get("download")
	signature, e3 := hex.DecodeString(q.Get("sig"))
	if e1 != nil || e2 != nil || e3 != nil || version < 1 || exp <= time.Now().Unix() || exp > time.Now().Add(artifactURLLifetime).Unix() || !artifactSlug.MatchString(slug) || !artifactComponent.MatchString(tenant) || (download != "0" && download != "1") || a.deps.ArtifactKey == nil || a.deps.ArtifactKey() == "" {
		http.NotFound(w, r)
		return
	}
	want, _ := hex.DecodeString(artifactSignature(a.deps.ArtifactKey(), tenant, slug, version, exp, download))
	if !hmac.Equal(signature, want) || a.deps.FeatureStage == nil {
		http.NotFound(w, r)
		return
	}
	stage := a.deps.FeatureStage(r.Context(), "ai-chat")
	if stage != "on" && stage != "beta" {
		http.NotFound(w, r)
		return
	}
	v, err := a.store.Artifact(r.Context(), tenant, slug, version)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body, err := readArtifact(a.deps.ArtifactsDir, v)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	disposition := "inline"
	if download == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", artifactCSP)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s-v%d.html"`, disposition, slug, version))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

// ArtifactStaticPath maps only public factory page shapes to the static export.
func ArtifactStaticPath(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 2 && len(parts) != 4 || parts[0] != "artifacts" || !artifactSlug.MatchString(parts[1]) {
		return "", false
	}
	if len(parts) == 4 {
		n, err := strconv.Atoi(parts[3])
		if parts[2] != "v" || err != nil || n < 1 {
			return "", false
		}
	}
	return "artifacts/index.html", true
}

func (a *api) runAction(w http.ResponseWriter, r *http.Request) {
	t, ok := a.owned(w, r)
	if !ok {
		return
	}
	if t.ArchivedAt != nil {
		http.Error(w, "thread is archived", 409)
		return
	}
	var body struct {
		Kind string `json:"kind"`
		Body string `json:"body,omitempty"`
		tools.ArtifactRequest
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Kind != "artifact" && body.Kind != "ticket" {
		http.Error(w, "not available", 400)
		return
	}
	for _, v := range []struct {
		s   string
		max int
	}{{body.Title, 80}, {body.Brief, 4000}, {body.Body, 8000}, {body.ArtifactID, 80}} {
		if utf8.RuneCountInString(v.s) > v.max || v.s != "" && strings.TrimSpace(v.s) == "" {
			http.Error(w, "invalid action", 400)
			return
		}
	}
	if body.Kind == "ticket" && (utf8.RuneCountInString(body.Title) > 70 || (body.Title == "") != (body.Body == "") || body.Brief != "") {
		http.Error(w, "invalid action", 400)
		return
	}
	ctx, release, err := a.runner.Acquire(r.Context(), t)
	if err != nil {
		w.Header().Set("Retry-After", "1")
		http.Error(w, ErrBusy.Error(), 429)
		return
	}
	defer release()
	if body.ArtifactID != "" {
		if _, err = a.store.ownedArtifact(ctx, t, body.ArtifactID); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	cfg, err := a.loadConfig(t.Workspace)
	if err != nil {
		http.Error(w, "Not configured for this workspace", 400)
		return
	}
	if a.runner.AvailableActions == nil || !slices.Contains(a.runner.AvailableActions(t.Workspace, cfg), body.Kind) {
		http.Error(w, "not available", 400)
		return
	}
	var p llm.Provider
	model := ""
	if body.Kind == "artifact" || body.Title == "" {
		p, err = a.deps.LLM(cfg.LLM)
		if err != nil {
			http.Error(w, "No usable LLM key configured for this workspace", 503)
			return
		}
		model = p.Model()
	}
	if _, ok := w.(http.Flusher); !ok {
		http.Error(w, "streaming unavailable", 500)
		return
	}
	m, err := a.store.beginAction(ctx, t, model, body.ArtifactID, body.Kind)
	if err != nil {
		http.Error(w, "Unable to save action.", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	emit, stop := streamWithHeartbeat(ctx, w, 15*time.Second, func() { a.runner.Cancel(t) })
	defer stop()
	if body.Kind == "ticket" {
		_ = a.runner.RunTicket(ctx, t, m, cfg, p, tools.TicketRequest{Title: body.Title, Body: body.Body, ArtifactID: body.ArtifactID}, emit)
	} else {
		_ = a.runner.RunArtifact(ctx, t, m, cfg, p, body.ArtifactRequest, emit)
	}
	stop()
	release()
	a.publish(t)
}

func (s *Store) beginAction(ctx context.Context, t Thread, model, artifactID string, actionKind ...string) (Message, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, interruptMessagesSQL, t.ID); err != nil {
		return Message{}, err
	}
	m := Message{ID: uuid.NewString(), ThreadID: t.ID, Role: "assistant", Model: model, Status: "streaming", CreatedAt: time.Now().UnixMilli()}
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM ai_chat_messages WHERE thread_id=?`, t.ID).Scan(&m.Seq); err != nil {
		return Message{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ai_chat_messages(id,thread_id,seq,role,model,status,created_at) VALUES(?,?,?,'assistant',?,'streaming',?)`, m.ID, t.ID, m.Seq, model, m.CreatedAt)
	if err != nil {
		return Message{}, err
	}
	// Only the already ownership-checked ID is stored before secrets are resolved.
	kind := "artifact"
	if len(actionKind) > 0 {
		kind = actionKind[0]
	}
	pending := string(redactedJSON(tools.ArtifactRequest{ArtifactID: artifactID}, nil))
	if kind == "ticket" {
		pending = string(redactedJSON(TicketCard{TicketRequest: tools.TicketRequest{ArtifactID: artifactID}}, nil))
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ai_chat_tool_runs(id,message_id,seq,tool,provider,args_json,created_at) VALUES(?,?,1,?,'AI Chat',?,?)`, uuid.NewString(), m.ID, kind, pending, m.CreatedAt); err != nil {
		return Message{}, err
	}
	return m, tx.Commit()
}
