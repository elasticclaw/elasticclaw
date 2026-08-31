package hub

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Permission string

const (
	PermAuthLogin        Permission = "auth:login"
	PermClawViewAll      Permission = "claw:view:all"
	PermClawViewOwn      Permission = "claw:view:own"
	PermClawMessagesRead Permission = "claw:messages:read"
	PermClawCreate       Permission = "claw:create"
	PermClawInteractAll  Permission = "claw:interact:all"
	PermClawInteractOwn  Permission = "claw:interact:own"
	PermClawModifyAll    Permission = "claw:modify:all"
	PermClawModifyOwn    Permission = "claw:modify:own"
	PermClawDeleteAll    Permission = "claw:delete:all"
	PermClawDeleteOwn    Permission = "claw:delete:own"
	PermClawCheckpoint   Permission = "claw:checkpoint"
	PermClawTerminal     Permission = "claw:terminal"
	PermClawFiles        Permission = "claw:files"
	PermWorkflowView     Permission = "workflow:view"
	PermWorkflowTrigger  Permission = "workflow:trigger"
	PermWorkflowEdit     Permission = "workflow:edit"
	PermWorkspaceView    Permission = "workspace:view"
	PermWorkspaceEdit    Permission = "workspace:edit"
	PermWorkspaceSecrets Permission = "workspace:secrets"
	PermFactoryView      Permission = "factory:view"
	PermFactoryTrigger   Permission = "factory:trigger"
	PermFactoryEdit      Permission = "factory:edit"
	PermAnalyticsView    Permission = "analytics:view"
	PermAnalyticsCosts   Permission = "analytics:costs"
	PermSettingsView     Permission = "settings:view"
	PermSettingsEdit     Permission = "settings:edit"
	PermSecretsManage    Permission = "secrets:manage"
	PermAccessManage     Permission = "access:manage"
	PermDoctorRun        Permission = "doctor:run"
	PermTemplateView     Permission = "template:view"
	PermTemplateEdit     Permission = "template:edit"
	PermMCPManage        Permission = "mcp:manage"
)

type PermissionMeta struct {
	Key                          Permission
	Resource, Label, Description string
}

var AllPermissions = []PermissionMeta{
	{PermAuthLogin, "auth", "Enter the UI", ""}, {PermClawViewAll, "claw", "View all agents", ""}, {PermClawViewOwn, "claw", "View own agents", ""}, {PermClawMessagesRead, "claw", "Read agent conversations and transcripts", ""}, {PermClawCreate, "claw", "Create agent", "Provisions a VM and consumes LLM usage"}, {PermClawInteractAll, "claw", "Message any agent", ""}, {PermClawInteractOwn, "claw", "Message own agents", ""}, {PermClawModifyAll, "claw", "Modify any agent", ""}, {PermClawModifyOwn, "claw", "Modify own agents", ""}, {PermClawDeleteAll, "claw", "Delete any agent", ""}, {PermClawDeleteOwn, "claw", "Delete own agents", ""}, {PermClawCheckpoint, "claw", "Create and restore checkpoints", ""}, {PermClawTerminal, "claw", "Open sandbox terminal", ""}, {PermClawFiles, "claw", "Transfer agent files", ""},
	{PermWorkflowView, "workflow", "View workflows, runs, and cron history", ""}, {PermWorkflowTrigger, "workflow", "Trigger workflows", ""}, {PermWorkflowEdit, "workflow", "Edit workflows", ""}, {PermWorkspaceView, "workspace", "View workspaces", ""}, {PermWorkspaceEdit, "workspace", "Edit workspaces", ""}, {PermWorkspaceSecrets, "workspace", "Manage workspace secrets and integrations", ""}, {PermFactoryView, "factory", "View factories and events", ""}, {PermFactoryTrigger, "factory", "Trigger factories", ""}, {PermFactoryEdit, "factory", "Edit factories", ""}, {PermAnalyticsView, "analytics", "View cost-free dashboard", ""}, {PermAnalyticsCosts, "analytics", "View costs and factory analytics", ""}, {PermSettingsView, "admin", "View settings", ""}, {PermSettingsEdit, "admin", "Edit hub configuration", ""}, {PermSecretsManage, "admin", "Manage global secrets and model credentials", ""}, {PermAccessManage, "admin", "Manage users, roles, and permissions", ""}, {PermDoctorRun, "admin", "Run doctor and troubleshoot", ""},
	{PermTemplateView, "template", "View agent templates", ""}, {PermTemplateEdit, "template", "Push and remove agent templates", "Templates decide the repo, tags, and env of new agents"}, {PermMCPManage, "admin", "Manage MCP servers", ""},
}

var (
	ErrRoleImmutable     = errors.New("role is immutable")
	ErrRoleSystem        = errors.New("role is a system role")
	ErrRoleInUse         = errors.New("role is in use")
	ErrLastAdmin         = errors.New("last active admin")
	ErrUnknownPermission = errors.New("unknown permission")
)

type Principal struct {
	TenantID, Login, RoleName string
	perms                     map[Permission]struct{}
	// ownTags are the tag patterns that prove a claw belongs to this
	// principal. They come from the hub AccessConfig (the same
	// view_requires_tags / interact_requires_tags patterns canViewClaw
	// already honours) so that the :own scopes match whatever ownership
	// convention the deployment actually writes onto its claws. Only
	// patterns containing {user} qualify: a static pattern such as
	// "team=core" grants access to a group, it does not prove ownership.
	// When the list is empty ownership cannot be proven and every :own
	// scope denies.
	ownTags []string
}

// clawTagScopedPerms are claw permissions that have no :own/:all axis of their
// own. Each is evaluated together with the view scope of the same claw, so a
// principal holding only claw:view:own cannot read the transcript, open the
// terminal, pull files, or checkpoint somebody else's claw.
var clawTagScopedPerms = map[Permission]struct{}{
	PermClawMessagesRead: {}, PermClawTerminal: {}, PermClawFiles: {}, PermClawCheckpoint: {},
}

// Allows reports whether the principal may act on a claw carrying tags.
//
// tags must be the claw's complete tag list. An empty or nil slice means "this
// claw carries no tag that proves ownership", so it denies every :own scope —
// callers that do not know the tags must not pretend they do.
func (p *Principal) Allows(perm Permission, tags []string) bool {
	if p == nil || p.Login == "" {
		return true
	}
	if _, ok := clawTagScopedPerms[perm]; ok {
		return p.holds(perm) && p.scopedAllows(PermClawViewAll, tags)
	}
	return p.scopedAllows(perm, tags)
}

func (p *Principal) holds(perm Permission) bool {
	_, ok := p.perms[perm]
	return ok
}

func (p *Principal) scopedAllows(perm Permission, tags []string) bool {
	key := string(perm)
	// A :own key handed in directly is evaluated as the scoped check the
	// caller naturally expects: holding the permission is not enough, the
	// claw has to be owned.
	if base := strings.TrimSuffix(key, ":own"); base != key {
		return p.holds(perm) && p.ownsTags(tags)
	}
	if p.holds(perm) {
		return true
	}
	if base := strings.TrimSuffix(key, ":all"); base != key {
		return p.holds(Permission(base+":own")) && p.ownsTags(tags)
	}
	return false
}

func (p *Principal) ownsTags(tags []string) bool {
	if len(p.ownTags) == 0 || len(tags) == 0 {
		return false
	}
	return matchesTags(p.ownTags, p.Login, tags)
}

// ownershipTagPatterns keeps the RBAC :own scopes in sync with the tag rules
// the hub is already configured with, instead of inventing a convention no
// producer emits.
func (s *Server) ownershipTagPatterns() []string {
	if s.hubCfg == nil || s.hubCfg.Auth == nil || s.hubCfg.Auth.Access == nil {
		return nil
	}
	cfg := s.hubCfg.Auth.Access
	var out []string
	seen := map[string]bool{}
	for _, pattern := range append(append([]string{}, cfg.ViewRequiresTags...), cfg.InteractRequiresTags...) {
		if !strings.Contains(pattern, "{user}") || seen[pattern] {
			continue
		}
		seen[pattern] = true
		out = append(out, pattern)
	}
	return out
}

type Role struct {
	ID, Name          string
	System, Immutable bool
}
type AccessUser struct {
	ID, TenantID, Login, Name, AvatarURL, RoleID, RoleName string
	Active                                                 bool
}
type accessCache struct {
	principal *Principal
	until     time.Time
	version   uint64
}
type accessState struct {
	mu      sync.Mutex
	version uint64
	cache   map[string]accessCache
}

func (s *Server) access() *accessState { // lazily attach avoids changing existing Server construction paths.
	accessStates.Lock()
	defer accessStates.Unlock()
	if v := accessStates.m[s]; v != nil {
		return v
	}
	v := &accessState{cache: map[string]accessCache{}}
	accessStates.m[s] = v
	return v
}

var accessStates = struct {
	sync.Mutex
	m map[*Server]*accessState
}{m: map[*Server]*accessState{}}

func (s *Server) invalidateAccess() {
	a := s.access()
	a.mu.Lock()
	a.version++
	a.cache = map[string]accessCache{}
	a.mu.Unlock()
}

func migrateAccessControl(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS permissions (key TEXT PRIMARY KEY, resource TEXT NOT NULL, label TEXT NOT NULL, description TEXT NOT NULL DEFAULT ''); CREATE TABLE IF NOT EXISTS roles (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, system INTEGER NOT NULL DEFAULT 0, immutable INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL); CREATE TABLE IF NOT EXISTS role_permissions (role_id TEXT NOT NULL REFERENCES roles(id) ON DELETE CASCADE, permission_key TEXT NOT NULL REFERENCES permissions(key), PRIMARY KEY(role_id, permission_key)); CREATE TABLE IF NOT EXISTS users (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL REFERENCES tenants(id), login TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', avatar_url TEXT NOT NULL DEFAULT '', role_id TEXT NOT NULL REFERENCES roles(id), active INTEGER NOT NULL DEFAULT 1, created_at INTEGER NOT NULL, last_login_at INTEGER NOT NULL DEFAULT 0, UNIQUE(tenant_id, login)); CREATE INDEX IF NOT EXISTS idx_users_tenant_login ON users(tenant_id, login)`)
	if err != nil {
		return err
	}
	return reconcileAccessControl(db)
}
func reconcileAccessControl(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, p := range AllPermissions {
		if _, err = tx.Exec(`INSERT INTO permissions(key,resource,label,description) VALUES(?,?,?,?) ON CONFLICT(key) DO UPDATE SET resource=excluded.resource,label=excluded.label,description=excluded.description`, p.Key, p.Resource, p.Label, p.Description); err != nil {
			return err
		}
	}
	for _, r := range []struct {
		name      string
		immutable int
	}{{"admin", 1}, {"reader", 0}} {
		result, e := tx.Exec(`INSERT OR IGNORE INTO roles(id,name,system,immutable,created_at) VALUES(?,?,?,?,?)`, r.name, r.name, 1, r.immutable, now().UnixMilli())
		if e != nil {
			return e
		}
		if r.name == "reader" {
			n, _ := result.RowsAffected()
			if n == 1 {
				for _, p := range []Permission{PermAuthLogin, PermClawViewAll, PermClawMessagesRead} {
					if _, e = tx.Exec(`INSERT OR IGNORE INTO role_permissions(role_id,permission_key) VALUES('reader',?)`, p); e != nil {
						return e
					}
				}
			}
		}
	}
	if _, err = tx.Exec(`DELETE FROM role_permissions WHERE role_id='admin'`); err != nil {
		return err
	}
	permissions, err := tx.Query(`SELECT key FROM permissions ORDER BY key`)
	if err != nil {
		return err
	}
	defer permissions.Close()
	for permissions.Next() {
		var key string
		if err = permissions.Scan(&key); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO role_permissions(role_id,permission_key) VALUES('admin',?)`, key); err != nil {
			return err
		}
	}
	if err = permissions.Err(); err != nil {
		return err
	}
	return tx.Commit()
}
func normalizeLogin(login string) string { return strings.ToLower(strings.TrimSpace(login)) }
func (s *Server) principalFor(tenantID, login string) (*Principal, error) {
	login = normalizeLogin(login)
	if login == "" {
		return &Principal{TenantID: tenantID}, nil
	}
	a := s.access()
	k := tenantID + "\x00" + login
	a.mu.Lock()
	c, ok := a.cache[k]
	if ok && c.until.After(now()) && c.version == a.version {
		a.mu.Unlock()
		return c.principal, nil
	}
	a.mu.Unlock()
	p := &Principal{TenantID: tenantID, Login: login, perms: map[Permission]struct{}{}, ownTags: s.ownershipTagPatterns()}
	rows, e := s.db.Query(`SELECT r.name,rp.permission_key FROM users u JOIN roles r ON r.id=u.role_id LEFT JOIN role_permissions rp ON rp.role_id=r.id WHERE u.tenant_id=? AND u.login=? AND u.active=1`, tenantID, login)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var key sql.NullString
		if e = rows.Scan(&p.RoleName, &key); e != nil {
			return nil, e
		}
		if key.Valid {
			p.perms[Permission(key.String)] = struct{}{}
		}
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	a.mu.Lock()
	a.cache[k] = accessCache{p, now().Add(5 * time.Second), a.version}
	a.mu.Unlock()
	return p, nil
}
func (s *Server) upsertUserOnLogin(tenantID, login, name, avatar string) (*Principal, error) {
	login = normalizeLogin(login)
	_, e := s.db.Exec(`INSERT INTO users(id,tenant_id,login,name,avatar_url,role_id,active,created_at,last_login_at) VALUES(?,?,?,?,?,'reader',1,?,?) ON CONFLICT(tenant_id,login) DO UPDATE SET name=excluded.name,avatar_url=excluded.avatar_url,last_login_at=excluded.last_login_at`, uuid.NewString(), tenantID, login, name, avatar, now().UnixMilli(), now().UnixMilli())
	if e != nil {
		return nil, e
	}
	s.invalidateAccess()
	return s.principalFor(tenantID, login)
}
func (s *Server) listRoles() ([]Role, error) {
	rows, e := s.db.Query(`SELECT id,name,system,immutable FROM roles ORDER BY name`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []Role
	for rows.Next() {
		var r Role
		var x, y int
		if e = rows.Scan(&r.ID, &r.Name, &x, &y); e != nil {
			return nil, e
		}
		r.System = x != 0
		r.Immutable = y != 0
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Server) listUsers(tenant string) ([]AccessUser, error) {
	rows, e := s.db.Query(`SELECT u.id,u.tenant_id,u.login,u.name,u.avatar_url,u.role_id,r.name,u.active FROM users u JOIN roles r ON r.id=u.role_id WHERE u.tenant_id=? ORDER BY u.login`, tenant)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []AccessUser
	for rows.Next() {
		var u AccessUser
		var a int
		if e = rows.Scan(&u.ID, &u.TenantID, &u.Login, &u.Name, &u.AvatarURL, &u.RoleID, &u.RoleName, &a); e != nil {
			return nil, e
		}
		u.Active = a != 0
		out = append(out, u)
	}
	return out, rows.Err()
}
func (s *Server) createRole(name string) (Role, error) {
	r := Role{ID: uuid.NewString(), Name: strings.TrimSpace(name)}
	_, e := s.db.Exec(`INSERT INTO roles(id,name,created_at) VALUES(?,?,?)`, r.ID, r.Name, now().UnixMilli())
	if e == nil {
		s.invalidateAccess()
	}
	return r, e
}

func (s *Server) renameRole(id, name string) error {
	var immutable int
	if err := s.db.QueryRow(`SELECT immutable FROM roles WHERE id=?`, id).Scan(&immutable); err != nil {
		return err
	}
	if immutable != 0 {
		return ErrRoleImmutable
	}
	_, err := s.db.Exec(`UPDATE roles SET name=? WHERE id=?`, strings.TrimSpace(name), id)
	if err == nil {
		s.invalidateAccess()
	}
	return err
}
func knownPermission(p Permission) bool {
	for _, x := range AllPermissions {
		if x.Key == p {
			return true
		}
	}
	return false
}
func (s *Server) updateRolePermissions(id string, perms []Permission) error {
	var imm int
	if e := s.db.QueryRow(`SELECT immutable FROM roles WHERE id=?`, id).Scan(&imm); e != nil {
		return e
	}
	if imm != 0 {
		return ErrRoleImmutable
	}
	for _, p := range perms {
		if !knownPermission(p) {
			return fmt.Errorf("%w: %s", ErrUnknownPermission, p)
		}
	}
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`DELETE FROM role_permissions WHERE role_id=?`, id); e != nil {
		return e
	}
	for _, p := range perms {
		if _, e = tx.Exec(`INSERT INTO role_permissions(role_id,permission_key) VALUES(?,?)`, id, p); e != nil {
			return e
		}
	}
	if e = tx.Commit(); e == nil {
		s.invalidateAccess()
	}
	return e
}
func (s *Server) deleteRole(id string) error {
	var sys int
	if e := s.db.QueryRow(`SELECT system FROM roles WHERE id=?`, id).Scan(&sys); e != nil {
		return e
	}
	if sys != 0 {
		return ErrRoleSystem
	}
	var n int
	if e := s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role_id=?`, id).Scan(&n); e != nil {
		return e
	}
	if n > 0 {
		return fmt.Errorf("%w: %d users", ErrRoleInUse, n)
	}
	_, e := s.db.Exec(`DELETE FROM roles WHERE id=?`, id)
	if e == nil {
		s.invalidateAccess()
	}
	return e
}
func (s *Server) setUserRole(tenant, login, role string) error {
	return s.changeUser(tenant, login, `role_id=?`, role)
}
func (s *Server) setUserActive(tenant, login string, active bool) error {
	v := 0
	if active {
		v = 1
	}
	return s.changeUser(tenant, login, `active=?`, v)
}

// changeUser reads, checks the last-active-admin invariant, and writes inside a
// single transaction. openDB uses _txlock=immediate, so concurrent demotions
// serialize on the write lock instead of both observing the same admin count
// and leaving the tenant with nobody holding access:manage.
func (s *Server) changeUser(tenant, login, field string, value any) error {
	login = normalizeLogin(login)
	tx, e := s.db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var oldRole string
	var active int
	if e = tx.QueryRow(`SELECT r.name,u.active FROM users u JOIN roles r ON r.id=u.role_id WHERE u.tenant_id=? AND u.login=?`, tenant, login).Scan(&oldRole, &active); e != nil {
		return e
	}
	nextAdmin := oldRole == "admin"
	if field == "role_id=?" {
		var name string
		if e = tx.QueryRow(`SELECT name FROM roles WHERE id=?`, value).Scan(&name); e != nil {
			return e
		}
		nextAdmin = name == "admin"
	}
	nextActive := active != 0
	if field == "active=?" {
		nextActive = value.(int) != 0
	}
	if oldRole == "admin" && active != 0 && (!nextAdmin || !nextActive) {
		var n int
		if e = tx.QueryRow(`SELECT COUNT(*) FROM users u JOIN roles r ON r.id=u.role_id WHERE u.tenant_id=? AND r.name='admin' AND u.active=1`, tenant).Scan(&n); e != nil {
			return e
		}
		if n <= 1 {
			return ErrLastAdmin
		}
	}
	if _, e = tx.Exec(`UPDATE users SET `+field+` WHERE tenant_id=? AND login=?`, value, tenant, login); e != nil {
		return e
	}
	if e = tx.Commit(); e == nil {
		s.invalidateAccess()
	}
	return e
}

type AccessBackfillRow struct{ Tenant, Login, Role, Action string }

// BackfillAccess seeds the users table from the configured administrators and
// from historical message authors. It runs once per tenant: the admin list is
// hub-wide, and every tenant needs at least one active admin of its own, so a
// tenant that only shows up in the messages table must not be left without one.
func BackfillAccess(dbPath string, admins []string, dryRun bool) ([]AccessBackfillRow, error) {
	if len(admins) == 0 {
		return nil, errors.New("auth.access.admins is empty; refusing backfill without an administrator")
	}
	var db *sql.DB
	var err error
	if dryRun {
		db, err = sql.Open("sqlite", dbPath+"?_time_format=sqlite&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)")
	} else {
		db, err = openDB(dbPath)
	}
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	adminSet := map[string]bool{}
	for _, login := range admins {
		if login = normalizeLogin(login); login != "" {
			adminSet[login] = true
		}
	}
	if len(adminSet) == 0 {
		return nil, errors.New("auth.access.admins is empty; refusing backfill without an administrator")
	}
	adminLogins := make([]string, 0, len(adminSet))
	for login := range adminSet {
		adminLogins = append(adminLogins, login)
	}
	sort.Strings(adminLogins)
	tenants, err := backfillTenants(tx)
	if err != nil {
		return nil, err
	}
	var out []AccessBackfillRow
	for _, tenant := range tenants {
		logins, e := backfillTenantLogins(tx, tenant, adminSet, adminLogins)
		if e != nil {
			return nil, e
		}
		for _, login := range logins {
			role := "reader"
			if adminSet[login] {
				role = "admin"
			}
			action, e := backfillUser(tx, tenant, login, role)
			if e != nil {
				return nil, e
			}
			out = append(out, AccessBackfillRow{tenant, login, role, action})
		}
	}
	if dryRun {
		return out, nil
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func backfillTenants(tx *sql.Tx) ([]string, error) {
	rows, err := tx.Query(`SELECT id FROM tenants ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("find tenant: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("find tenant: %w", err)
		}
		out = append(out, id)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("find tenant: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("find tenant: %w", sql.ErrNoRows)
	}
	return out, nil
}

// backfillTenantLogins returns the administrators plus the message authors that
// belong to this tenant. Message authors are filtered by tenant_id so that a
// login only seen in another tenant does not get an account here.
func backfillTenantLogins(tx *sql.Tx, tenant string, adminSet map[string]bool, adminLogins []string) ([]string, error) {
	logins := append([]string{}, adminLogins...)
	rows, err := tx.Query(`SELECT DISTINCT lower(trim(user_login)) FROM messages WHERE tenant_id=? AND user_login <> '' ORDER BY 1`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l string
		if err = rows.Scan(&l); err != nil {
			return nil, err
		}
		if l != "" && !adminSet[l] {
			logins = append(logins, l)
		}
	}
	return logins, rows.Err()
}

func backfillUser(tx *sql.Tx, tenant, login, role string) (string, error) {
	var existingRole string
	var existingActive int
	e := tx.QueryRow(`SELECT r.name,u.active FROM users u JOIN roles r ON r.id=u.role_id WHERE u.tenant_id=? AND u.login=?`, tenant, login).Scan(&existingRole, &existingActive)
	switch {
	case e == sql.ErrNoRows:
		_, e = tx.Exec(`INSERT INTO users(id,tenant_id,login,role_id,created_at,last_login_at) VALUES(?,?,?,?,?,0)`, uuid.NewString(), tenant, login, role, now().UnixMilli())
		return "created", e
	case e != nil:
		return "", e
	case role == "admin" && (existingRole != "admin" || existingActive == 0):
		// Promoting without reactivating would satisfy "the tenant has an
		// admin" with an admin that cannot log in, so do both at once.
		_, e = tx.Exec(`UPDATE users SET role_id='admin',active=1 WHERE tenant_id=? AND login=?`, tenant, login)
		return "updated", e
	}
	return "unchanged", nil
}
