package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// FeatureStage controls who can use a feature flag.
type FeatureStage string

const (
	FeatureStageOff  FeatureStage = "off"
	FeatureStageBeta FeatureStage = "beta"
	FeatureStageOn   FeatureStage = "on"
)

// FeatureFlag describes a feature that can be staged from off to beta to on.
type FeatureFlag struct {
	Key          string
	Name         string
	Description  string
	DefaultStage FeatureStage
}

// featureFlagRegistry declares every supported feature flag. Add a FeatureFlag
// here when introducing a gated feature, then remove it after the feature ships.
var featureFlagRegistry = []FeatureFlag{}

type featureFlagView struct {
	Key          string       `json:"key"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	Stage        FeatureStage `json:"stage"`
	DefaultStage FeatureStage `json:"default_stage"`
}

type betaTesterView struct {
	Login   string `json:"login"`
	AddedAt int64  `json:"added_at"`
	AddedBy string `json:"added_by"`
}

var githubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*$`)

func validFeatureStage(stage FeatureStage) bool {
	return stage == FeatureStageOff || stage == FeatureStageBeta || stage == FeatureStageOn
}

func findFeatureFlag(key string) (FeatureFlag, bool) {
	for _, flag := range featureFlagRegistry {
		if flag.Key == key {
			return flag, true
		}
	}
	return FeatureFlag{}, false
}

func (s *Server) featureStage(ctx context.Context, flag FeatureFlag) (FeatureStage, error) {
	var stage FeatureStage
	err := s.db.QueryRowContext(ctx, `SELECT stage FROM hub_feature_flags WHERE key = ?`, flag.Key).Scan(&stage)
	if err == sql.ErrNoRows {
		return flag.DefaultStage, nil
	}
	if err != nil {
		return flag.DefaultStage, err
	}
	if !validFeatureStage(stage) {
		return flag.DefaultStage, nil
	}
	return stage, nil
}

func (s *Server) effectiveFeatureStage(ctx context.Context, flag FeatureFlag) FeatureStage {
	stage, err := s.featureStage(ctx, flag)
	if err != nil {
		return flag.DefaultStage
	}
	return stage
}

func normalizeGitHubLogin(login string) string {
	login = strings.TrimSpace(login)
	login = strings.TrimPrefix(login, "@")
	return strings.ToLower(login)
}

func validGitHubLogin(login string) bool {
	return len(login) >= 1 && len(login) <= 39 && githubLoginPattern.MatchString(login)
}

func (s *Server) isBetaTester(ctx context.Context, login string) bool {
	login = normalizeGitHubLogin(login)
	if login == "" {
		return false
	}
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hub_beta_testers WHERE login = ? COLLATE NOCASE)`, login).Scan(&exists); err != nil {
		return false
	}
	return exists
}

func (s *Server) enabledFeaturesForLogin(ctx context.Context, login string) []string {
	enabled := make([]string, 0)
	isTester := s.isBetaTester(ctx, login)
	for _, flag := range featureFlagRegistry {
		switch s.effectiveFeatureStage(ctx, flag) {
		case FeatureStageOn:
			enabled = append(enabled, flag.Key)
		case FeatureStageBeta:
			if isTester {
				enabled = append(enabled, flag.Key)
			}
		}
	}
	sort.Strings(enabled)
	return enabled
}

// featureEnabled reports whether the current web user can use key. Password
// sessions have no GitHub login, so beta flags remain disabled for them.
func (s *Server) featureEnabled(r *http.Request, key string) bool {
	flag, ok := findFeatureFlag(key)
	if !ok {
		return false
	}
	switch s.effectiveFeatureStage(r.Context(), flag) {
	case FeatureStageOn:
		return true
	case FeatureStageBeta:
		return s.isBetaTester(r.Context(), githubLoginFromContext(r.Context()))
	default:
		return false
	}
}

func featureFlagViewFor(flag FeatureFlag, stage FeatureStage) featureFlagView {
	return featureFlagView{
		Key:          flag.Key,
		Name:         flag.Name,
		Description:  flag.Description,
		Stage:        stage,
		DefaultStage: flag.DefaultStage,
	}
}

func (s *Server) handleFeatureFlags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	flags := make([]featureFlagView, 0, len(featureFlagRegistry))
	for _, flag := range featureFlagRegistry {
		stage, err := s.featureStage(r.Context(), flag)
		if err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		flags = append(flags, featureFlagViewFor(flag, stage))
	}

	testers := make([]betaTesterView, 0)
	rows, err := s.db.QueryContext(r.Context(), `SELECT login, added_at, added_by FROM hub_beta_testers ORDER BY login COLLATE NOCASE`)
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var tester betaTesterView
		if err := rows.Scan(&tester.Login, &tester.AddedAt, &tester.AddedBy); err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		testers = append(testers, tester)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]interface{}{
		"flags":        flags,
		"beta_testers": testers,
	})
}

func (s *Server) handleFeatureFlag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flag, ok := findFeatureFlag(r.PathValue("key"))
	if !ok {
		http.Error(w, "feature flag not found", http.StatusNotFound)
		return
	}
	var body struct {
		Stage FeatureStage `json:"stage"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if !validFeatureStage(body.Stage) {
		http.Error(w, "invalid feature stage", http.StatusBadRequest)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `
		INSERT INTO hub_feature_flags(key, stage, updated_at, updated_by)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			stage = excluded.stage,
			updated_at = excluded.updated_at,
			updated_by = excluded.updated_by`,
		flag.Key, body.Stage, now().UnixMilli(), githubLoginFromContext(r.Context())); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	jsonOK(w, featureFlagViewFor(flag, body.Stage))
}

func (s *Server) handleBetaTesters(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	login := normalizeGitHubLogin(body.Login)
	if !validGitHubLogin(login) {
		http.Error(w, "invalid GitHub login", http.StatusBadRequest)
		return
	}
	result, err := s.db.ExecContext(r.Context(), `
		INSERT OR IGNORE INTO hub_beta_testers(login, added_at, added_by)
		VALUES(?, ?, ?)`, login, now().UnixMilli(), githubLoginFromContext(r.Context()))
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	created, err := result.RowsAffected()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	var tester betaTesterView
	if err := s.db.QueryRowContext(r.Context(), `SELECT login, added_at, added_by FROM hub_beta_testers WHERE login = ?`, login).
		Scan(&tester.Login, &tester.AddedAt, &tester.AddedBy); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	if created == 1 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
	}
	jsonOK(w, tester)
}

func (s *Server) handleBetaTester(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	login := normalizeGitHubLogin(r.PathValue("login"))
	if !validGitHubLogin(login) {
		http.Error(w, "invalid GitHub login", http.StatusBadRequest)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `DELETE FROM hub_beta_testers WHERE login = ? COLLATE NOCASE`, login); err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
