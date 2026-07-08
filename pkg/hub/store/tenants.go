package store

import "context"

// TenantsRepo is the repository for the tenant aggregate. SQL moved
// verbatim from pkg/hub/auth.go during the phase-2 store extraction.
type TenantsRepo struct {
	st *Store
}

// FirstID returns the oldest tenant's ID (the tenant backing GitHub
// OAuth sessions on single-tenant hubs).
func (r *TenantsRepo) FirstID(ctx context.Context) (string, error) {
	var id string
	err := r.st.queryRowScan(ctx, `SELECT id FROM tenants ORDER BY created_at ASC LIMIT 1`, nil, &id)
	return id, err
}

// IDByToken resolves a user login token to its tenant ID.
func (r *TenantsRepo) IDByToken(ctx context.Context, token string) (string, error) {
	var id string
	err := r.st.queryRowScan(ctx, `SELECT id FROM tenants WHERE token = ?`, []any{token}, &id)
	return id, err
}

// IDByClawToken resolves a claw connect token to its tenant ID.
func (r *TenantsRepo) IDByClawToken(ctx context.Context, token string) (string, error) {
	var id string
	err := r.st.queryRowScan(ctx, `SELECT id FROM tenants WHERE claw_token = ?`, []any{token}, &id)
	return id, err
}
