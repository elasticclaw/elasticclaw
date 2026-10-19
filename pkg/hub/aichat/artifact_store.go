package aichat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
	"github.com/oklog/ulid/v2"
)

type Artifact struct {
	ID             string `json:"artifactId"`
	Slug           string `json:"slug"`
	Tenant         string `json:"-"`
	OwnerLogin     string `json:"ownerLogin"`
	ThreadID       string `json:"-"`
	Title          string `json:"title"`
	Version        int    `json:"version"`
	CurrentVersion int    `json:"currentVersion"`
	Bytes          int64  `json:"bytes"`
	CreatedAt      int64  `json:"createdAt"`
	Path           string `json:"-"`
	SHA256         string `json:"-"`
}

// One serving process owns a database. Serialize publication across Store values,
// taking SQLite's write lock before choosing the next immutable version number.
var artifactWriteMu sync.Mutex
var artifactComponent = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// Slugs are lowercase ULIDs; title-based slugs from before stay valid.
var artifactSlug = regexp.MustCompile(`^(?:[0-9a-hjkmnp-tv-z]{26}|[a-z0-9][a-z0-9-]{0,39}-[a-z2-7]{6})$`)

// newArtifactID returns a lowercase ULID used as both the artifact ID and its
// URL slug. Nothing in it comes from the title or the model, so neither can
// guess or imitate one.
func newArtifactID() (string, error) {
	id, err := ulid.New(ulid.Now(), rand.Reader)
	if err != nil {
		return "", err
	}
	return strings.ToLower(id.String()), nil
}

func (s *Store) Artifact(ctx context.Context, tenant, slug string, version int) (Artifact, error) {
	return s.artifact(ctx, tenant, "", slug, version)
}

// Empty owner allows shared reads within the tenant; mutations require an owner.
func (s *Store) artifact(ctx context.Context, tenant, owner, slug string, version int) (Artifact, error) {
	var a Artifact
	query := `SELECT a.id,a.slug,a.tenant_id,a.owner_login,a.thread_id,a.title,a.current_version,v.version,v.bytes,v.created_at,v.path,v.sha256 FROM ai_chat_artifacts a JOIN ai_chat_artifact_versions v ON v.artifact_id=a.id AND v.version=CASE WHEN ?=0 THEN a.current_version ELSE ? END WHERE a.tenant_id=? AND a.slug=? AND a.deleted_at IS NULL`
	args := []any{version, version, tenant, slug}
	if owner != "" {
		query += ` AND a.owner_login=?`
		args = append(args, OwnerLogin(owner))
	}
	err := s.DB.QueryRowContext(ctx, query, args...).Scan(&a.ID, &a.Slug, &a.Tenant, &a.OwnerLogin, &a.ThreadID, &a.Title, &a.CurrentVersion, &a.Version, &a.Bytes, &a.CreatedAt, &a.Path, &a.SHA256)
	return a, err
}
func (s *Store) ownedArtifact(ctx context.Context, t Thread, id string) (Artifact, error) {
	var slug string
	if err := s.DB.QueryRowContext(ctx, `SELECT slug FROM ai_chat_artifacts WHERE id=? AND tenant_id=? AND owner_login=? AND thread_id=? AND deleted_at IS NULL`, id, t.TenantID, t.OwnerLogin, t.ID).Scan(&slug); err != nil {
		return Artifact{}, err
	}
	return s.artifact(ctx, t.TenantID, t.OwnerLogin, slug, 0)
}

func artifactPath(dir, tenant, slug string, version int) (string, error) {
	if dir == "" || !artifactComponent.MatchString(tenant) || !artifactSlug.MatchString(slug) || version < 1 {
		return "", errors.New("invalid artifact path")
	}
	return filepath.Join(dir, tenant, slug, fmt.Sprintf("v%d.html", version)), nil
}
func readArtifact(dir string, a Artifact) ([]byte, error) {
	path, err := artifactPath(dir, a.Tenant, a.Slug, a.Version)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

func (s *Store) publishArtifact(ctx context.Context, dir string, t Thread, m *Message, input tools.ArtifactRequest, html string) (Artifact, error) {
	if len(html) > maxArtifactBytes {
		return Artifact{}, errArtifactSize
	}
	artifactWriteMu.Lock()
	defer artifactWriteMu.Unlock()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Artifact{}, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	// Acquire the write lock before reading current_version.
	if _, err = tx.ExecContext(ctx, `UPDATE ai_chat_threads SET updated_at=? WHERE id=?`, now, t.ID); err != nil {
		return Artifact{}, err
	}
	a := Artifact{ID: input.ArtifactID, Tenant: t.TenantID, OwnerLogin: t.OwnerLogin, ThreadID: t.ID, Title: input.Title, Version: 1, CreatedAt: now, Bytes: int64(len(html))}
	if a.ID != "" {
		err = tx.QueryRowContext(ctx, `SELECT slug,current_version FROM ai_chat_artifacts WHERE id=? AND tenant_id=? AND owner_login=? AND thread_id=? AND deleted_at IS NULL`, a.ID, t.TenantID, t.OwnerLogin, t.ID).Scan(&a.Slug, &a.Version)
		if err != nil {
			return Artifact{}, err
		}
		a.Version++
	} else {
		for attempt := 0; attempt < 8; attempt++ {
			a.ID, err = newArtifactID()
			if err != nil {
				return Artifact{}, err
			}
			a.Slug = a.ID
			var result sql.Result
			result, err = tx.ExecContext(ctx, `INSERT INTO ai_chat_artifacts(id,slug,tenant_id,workspace,owner_login,thread_id,title,current_version,created_at,updated_at) VALUES(?,?,?,?,?,?,?,1,?,?) ON CONFLICT(slug) DO NOTHING`, a.ID, a.Slug, t.TenantID, t.Workspace, t.OwnerLogin, t.ID, a.Title, now, now)
			if err != nil {
				return Artifact{}, err
			}
			n, _ := result.RowsAffected()
			if n == 1 {
				break
			}
			a.Slug = ""
		}
		if a.Slug == "" {
			return Artifact{}, errors.New("unable to allocate artifact slug")
		}
	}
	a.CurrentVersion = a.Version
	a.Path, err = artifactPath(dir, t.TenantID, a.Slug, a.Version)
	if err != nil {
		return Artifact{}, err
	}
	// The configured root may be an administrator-managed symlink. Only enforce
	// private permissions and reject symlinks on our tenant and slug directories.
	for _, p := range []string{filepath.Join(dir, t.TenantID), filepath.Dir(a.Path)} {
		if err = os.MkdirAll(p, 0700); err != nil {
			return Artifact{}, err
		}
		info, e := os.Lstat(p)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return Artifact{}, errors.New("invalid artifact directory")
		}
		if err = os.Chmod(p, 0700); err != nil {
			return Artifact{}, err
		}
	}
	file, err := os.CreateTemp(filepath.Dir(a.Path), ".pending-")
	if err != nil {
		return Artifact{}, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = file.Chmod(0600); err == nil {
		_, err = file.WriteString(html)
	}
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = file.Close()
	}
	if err != nil {
		return Artifact{}, err
	}
	if err = ctx.Err(); err != nil {
		return Artifact{}, err
	}
	// A rolled-back/crashed publication may leave this unreferenced filename;
	// committed versions are never selected again because the transaction holds the lock.
	if err = os.Rename(file.Name(), a.Path); err != nil {
		return Artifact{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(a.Path)
		}
	}()
	directory, err := os.Open(filepath.Dir(a.Path))
	if err != nil {
		return Artifact{}, err
	}
	err = directory.Sync()
	directory.Close()
	if err != nil {
		return Artifact{}, err
	}
	sum := sha256.Sum256([]byte(html))
	a.SHA256 = hex.EncodeToString(sum[:])
	if _, err = tx.ExecContext(ctx, `INSERT INTO ai_chat_artifact_versions(artifact_id,version,message_id,path,bytes,sha256,created_at) VALUES(?,?,?,?,?,?,?)`, a.ID, a.Version, m.ID, a.Path, a.Bytes, a.SHA256, now); err != nil {
		return Artifact{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ai_chat_artifacts SET current_version=?,title=?,updated_at=? WHERE id=?`, a.Version, a.Title, now, a.ID); err != nil {
		return Artifact{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE ai_chat_tool_runs SET summary=?,result_json='null',error='' WHERE message_id=? AND tool='artifact'`, fmt.Sprintf("Published %s v%d", a.Title, a.Version), m.ID); err != nil {
		return Artifact{}, err
	}
	saved := *m
	setArtifactBlock(&saved, "created", ArtifactCard{ArtifactRequest: tools.ArtifactRequest{ArtifactID: a.ID, Title: a.Title, Brief: input.Brief}, Slug: a.Slug, Version: a.Version, Bytes: a.Bytes})
	if _, err = tx.ExecContext(ctx, `UPDATE ai_chat_messages SET blocks_json=? WHERE id=?`, string(saved.Blocks), m.ID); err != nil {
		return Artifact{}, err
	}
	if err = ctx.Err(); err != nil {
		return Artifact{}, err
	}
	if err = tx.Commit(); err != nil {
		return Artifact{}, err
	}
	committed = true
	m.Blocks = saved.Blocks
	return a, nil
}
