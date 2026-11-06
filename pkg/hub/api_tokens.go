package hub

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

type apiTokenIdentity struct {
	TokenID    string
	OwnerLogin string
}

type apiTokenView struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	Status     string     `json:"status"`
}

var (
	errInvalidAPIToken     = errors.New("invalid API token")
	errAPITokenUnavailable = errors.New("API token store unavailable")
)

func apiTokenHash(raw string) string {
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}

// authenticateAPIToken validates a plaintext `ecf_` token and returns its owner.
// Every use checks revocation, expiration, and the current admin configuration.
// Every credential failure returns errInvalidAPIToken (respond with HTTP 401);
// database failures wrap errAPITokenUnavailable (respond with HTTP 503).
// Successful use updates last_used_at at most once per 60 seconds per token.
func (s *Server) authenticateAPIToken(ctx context.Context, raw string) (apiTokenIdentity, error) {
	if !strings.HasPrefix(raw, "ecf_") {
		return apiTokenIdentity{}, errInvalidAPIToken
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(raw, "ecf_"))
	if err != nil || len(secret) != 32 {
		return apiTokenIdentity{}, errInvalidAPIToken
	}
	var identity apiTokenIdentity
	var expiresAt, revokedAt, lastUsedAt sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT id, owner_login, expires_at, revoked_at, last_used_at
		FROM api_tokens WHERE token_hash = ?`, apiTokenHash(raw)).Scan(
		&identity.TokenID, &identity.OwnerLogin, &expiresAt, &revokedAt, &lastUsedAt)
	usedAt := now().Unix()
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return apiTokenIdentity{}, fmt.Errorf("%w: %v", errAPITokenUnavailable, err)
	}
	if err != nil || revokedAt.Valid || (expiresAt.Valid && expiresAt.Int64 <= usedAt) {
		return apiTokenIdentity{}, errInvalidAPIToken
	}
	s.mu.RLock()
	admin := s.hubCfg != nil && s.hubCfg.Auth != nil && isAccessAdmin(s.hubCfg.Auth.Access, identity.OwnerLogin)
	s.mu.RUnlock()
	if !admin {
		return apiTokenIdentity{}, errInvalidAPIToken
	}
	if !lastUsedAt.Valid || lastUsedAt.Int64 <= usedAt-60 {
		// The predicate also prevents concurrent requests from repeatedly writing.
		if _, err := s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ?
			WHERE id = ? AND (last_used_at IS NULL OR last_used_at <= ?)`, usedAt, identity.TokenID, usedAt-60); err != nil {
			return apiTokenIdentity{}, fmt.Errorf("%w: %v", errAPITokenUnavailable, err)
		}
	}
	return identity, nil
}

func apiTokenTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	t := time.Unix(value.Int64, 0).UTC()
	return &t
}

func scanAPIToken(row interface{ Scan(...any) error }, timestamp time.Time) (apiTokenView, error) {
	var item apiTokenView
	var created int64
	var expires, used, revoked sql.NullInt64
	if err := row.Scan(&item.ID, &item.Name, &item.Prefix, &created, &expires, &used, &revoked); err != nil {
		return item, err
	}
	item.CreatedAt = time.Unix(created, 0).UTC()
	item.ExpiresAt, item.LastUsedAt, item.RevokedAt = apiTokenTime(expires), apiTokenTime(used), apiTokenTime(revoked)
	item.Status = "active"
	if revoked.Valid {
		item.Status = "revoked"
	} else if expires.Valid && expires.Int64 <= timestamp.Unix() {
		item.Status = "expired"
	}
	return item, nil
}

func (s *Server) handleAPITokens(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	login := strings.ToLower(githubLoginFromContext(r.Context()))
	if login == "" {
		jsonError(w, http.StatusForbidden, "API tokens require GitHub sign-in")
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.db.QueryContext(r.Context(), `SELECT id, name, token_prefix, created_at, expires_at, last_used_at, revoked_at
			FROM api_tokens WHERE owner_login = ? COLLATE NOCASE ORDER BY created_at DESC, rowid DESC`, login)
		if err != nil {
			jsonError(w, http.StatusInternalServerError, "database error")
			return
		}
		defer rows.Close()
		items := make([]apiTokenView, 0)
		timestamp := now()
		for rows.Next() {
			item, err := scanAPIToken(rows, timestamp)
			if err != nil {
				jsonError(w, http.StatusInternalServerError, "database error")
				return
			}
			items = append(items, item)
		}
		if err := rows.Err(); err != nil {
			jsonError(w, http.StatusInternalServerError, "database error")
			return
		}
		jsonOK(w, map[string]any{"tokens": items})
	case http.MethodPost:
		s.createAPIToken(w, r, login)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) createAPIToken(w http.ResponseWriter, r *http.Request, login string) {
	var body struct {
		Name          string `json:"name"`
		ExpiresInDays *int   `json:"expires_in_days"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	if err := decoder.Decode(&body); err != nil {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		jsonError(w, http.StatusBadRequest, "invalid request")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || utf8.RuneCountInString(body.Name) > 100 {
		jsonError(w, http.StatusBadRequest, "name must be between 1 and 100 characters")
		return
	}
	created := now().Truncate(time.Second)
	var expires *time.Time
	var expiresUnix *int64
	if body.ExpiresInDays != nil {
		switch *body.ExpiresInDays {
		case 7, 30, 90, 180:
		default:
			jsonError(w, http.StatusBadRequest, "expires_in_days must be 7, 30, 90, 180, or null")
			return
		}
		t := created.Add(time.Duration(*body.ExpiresInDays) * 24 * time.Hour)
		u := t.Unix()
		expires, expiresUnix = &t, &u
	}
	var id [16]byte
	var secret [32]byte
	if _, err := rand.Read(id[:]); err != nil {
		jsonError(w, http.StatusInternalServerError, "could not generate token")
		return
	}
	if _, err := rand.Read(secret[:]); err != nil {
		jsonError(w, http.StatusInternalServerError, "could not generate token")
		return
	}
	raw := "ecf_" + base64.RawURLEncoding.EncodeToString(secret[:])
	item := apiTokenView{ID: hex.EncodeToString(id[:]), Name: body.Name, Prefix: raw[:12], CreatedAt: created, ExpiresAt: expires, Status: "active"}
	if _, err := s.db.ExecContext(r.Context(), `INSERT INTO api_tokens
		(id, owner_login, name, token_hash, token_prefix, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		item.ID, login, item.Name, apiTokenHash(raw), item.Prefix, created.Unix(), expiresUnix); err != nil {
		jsonError(w, http.StatusInternalServerError, "database error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, struct {
		apiTokenView
		Token string `json:"token"`
	}{item, raw})
}

func (s *Server) handleAPIToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	login := strings.ToLower(githubLoginFromContext(r.Context()))
	if login == "" {
		jsonError(w, http.StatusForbidden, "API tokens require GitHub sign-in")
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	result, err := s.db.ExecContext(r.Context(), `UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, ?)
		WHERE id = ? AND owner_login = ? COLLATE NOCASE`, now().Unix(), r.PathValue("id"), login)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "database error")
		return
	}
	count, err := result.RowsAffected()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, "database error")
		return
	}
	if count == 0 {
		jsonError(w, http.StatusNotFound, "token not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
