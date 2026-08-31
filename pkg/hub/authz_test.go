package hub

import (
	"errors"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestAccessSeed(t *testing.T) {
	_, db := NewTestServerWithConfig(t, nil, "", "", "")
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM permissions`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(AllPermissions) {
		t.Fatalf("permission count = %d, want %d", count, len(AllPermissions))
	}
	for _, role := range []string{"admin", "reader"} {
		if err := db.QueryRow(`SELECT COUNT(*) FROM roles WHERE id=?`, role).Scan(&count); err != nil || count != 1 {
			t.Fatalf("role %q count = %d, err = %v", role, count, err)
		}
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM role_permissions WHERE role_id='admin'`).Scan(&count); err != nil || count != len(AllPermissions) {
		t.Fatalf("admin permission count = %d, err = %v; want %d", count, err, len(AllPermissions))
	}
	rows, err := db.Query(`SELECT permission_key FROM role_permissions WHERE role_id='reader' ORDER BY permission_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []Permission
	for rows.Next() {
		var permission Permission
		if err := rows.Scan(&permission); err != nil {
			t.Fatal(err)
		}
		got = append(got, permission)
	}
	want := []Permission{PermAuthLogin, PermClawMessagesRead, PermClawViewAll}
	if len(got) != len(want) {
		t.Fatalf("reader permissions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reader permissions = %v, want %v", got, want)
		}
	}
}

func TestAccessPermissionEnumMatchesTable(t *testing.T) {
	_, db := NewTestServerWithConfig(t, nil, "", "", "")
	enum := make(map[Permission]bool, len(AllPermissions))
	for _, permission := range AllPermissions {
		enum[permission.Key] = true
	}
	rows, err := db.Query(`SELECT key FROM permissions`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	table := map[Permission]bool{}
	for rows.Next() {
		var permission Permission
		if err := rows.Scan(&permission); err != nil {
			t.Fatal(err)
		}
		table[permission] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for permission := range enum {
		if !table[permission] {
			t.Errorf("enum permission %q is missing from the table", permission)
		}
	}
	for permission := range table {
		if !enum[permission] {
			t.Errorf("table permission %q is missing from the enum", permission)
		}
	}
}

func TestAccessReconcileGrantsFuturePermissionToAdmin(t *testing.T) {
	_, db := NewTestServerWithConfig(t, nil, "", "", "")
	const future Permission = "future:operate"
	if _, err := db.Exec(`INSERT INTO permissions(key,resource,label,description) VALUES(?,?,?,?)`, future, "future", "Operate", ""); err != nil {
		t.Fatal(err)
	}
	if err := reconcileAccessControl(db); err != nil {
		t.Fatalf("reconcile access control: %v", err)
	}
	for _, role := range []struct {
		name string
		want int
	}{{"admin", 1}, {"reader", 0}} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM role_permissions WHERE role_id=? AND permission_key=?`, role.name, future).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != role.want {
			t.Errorf("%s future permission = %d, want %d", role.name, count, role.want)
		}
	}
}

func TestAccessReaderPermissionsSurviveMigration(t *testing.T) {
	s, db := NewTestServerWithConfig(t, nil, "", "", "")
	want := []Permission{PermAuthLogin, PermWorkflowView}
	if err := s.updateRolePermissions("reader", want); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rows, err := db.Query(`SELECT permission_key FROM role_permissions WHERE role_id='reader' ORDER BY permission_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[Permission]bool{}
	for rows.Next() {
		var permission Permission
		if err := rows.Scan(&permission); err != nil {
			t.Fatal(err)
		}
		got[permission] = true
	}
	if len(got) != len(want) || !got[PermAuthLogin] || !got[PermWorkflowView] {
		t.Fatalf("reader permissions after migration = %v, want %v", got, want)
	}
}

func TestRenameImmutableRoleReturnsErrRoleImmutable(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	if !errors.Is(s.renameRole("admin", "administrators"), ErrRoleImmutable) {
		t.Fatal("rename admin did not return ErrRoleImmutable")
	}
}

func TestDeleteSystemRoleReturnsErrRoleSystem(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	if !errors.Is(s.deleteRole("reader"), ErrRoleSystem) {
		t.Fatal("delete reader did not return ErrRoleSystem")
	}
}

func TestDeleteRoleInUseReturnsErrRoleInUse(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	role, err := s.createRole("operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.upsertUserOnLogin("test-tenant-id", "alice", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.setUserRole("test-tenant-id", "alice", role.ID); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.deleteRole(role.ID), ErrRoleInUse) {
		t.Fatal("delete used role did not return ErrRoleInUse")
	}
}

func TestRemoveLastAdminReturnsErrLastAdmin(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	if _, err := s.upsertUserOnLogin("test-tenant-id", "alice", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.setUserRole("test-tenant-id", "alice", "admin"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(s.setUserActive("test-tenant-id", "alice", false), ErrLastAdmin) {
		t.Fatal("deactivating last admin did not return ErrLastAdmin")
	}
}

func TestUpdateRoleWithUnknownPermissionReturnsErrUnknownPermission(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	if !errors.Is(s.updateRolePermissions("reader", []Permission{"not:known"}), ErrUnknownPermission) {
		t.Fatal("unknown permission did not return ErrUnknownPermission")
	}
}

func TestPrincipalAllows(t *testing.T) {
	ownTags := []string{"owner={user}"}
	t.Run("all grants", func(t *testing.T) {
		p := &Principal{Login: "alice", perms: map[Permission]struct{}{PermClawInteractAll: {}}, ownTags: ownTags}
		if !p.Allows(PermClawInteractAll, []string{"owner=bob"}) {
			t.Fatal("all permission was denied")
		}
	})
	t.Run("own grants matching owner", func(t *testing.T) {
		p := &Principal{Login: "alice", perms: map[Permission]struct{}{PermClawInteractOwn: {}}, ownTags: ownTags}
		if !p.Allows(PermClawInteractAll, []string{"owner=alice"}) {
			t.Fatal("own permission was denied for matching owner")
		}
	})
	t.Run("own denies another owner", func(t *testing.T) {
		p := &Principal{Login: "alice", perms: map[Permission]struct{}{PermClawInteractOwn: {}}, ownTags: ownTags}
		if p.Allows(PermClawInteractAll, []string{"owner=bob"}) {
			t.Fatal("own permission was granted for another owner")
		}
	})
	t.Run("own denies untagged claw", func(t *testing.T) {
		p := &Principal{Login: "alice", perms: map[Permission]struct{}{PermClawViewOwn: {}}, ownTags: ownTags}
		for _, tags := range [][]string{nil, {}} {
			if p.Allows(PermClawViewAll, tags) {
				t.Fatalf("own permission was granted for a claw with tags %v", tags)
			}
		}
	})
	t.Run("own key passed directly still checks ownership", func(t *testing.T) {
		p := &Principal{Login: "alice", perms: map[Permission]struct{}{PermClawModifyOwn: {}}, ownTags: ownTags}
		if p.Allows(PermClawModifyOwn, []string{"owner=bob"}) {
			t.Fatal("own permission was granted for another owner when queried directly")
		}
		if !p.Allows(PermClawModifyOwn, []string{"owner=alice"}) {
			t.Fatal("own permission was denied for own claw when queried directly")
		}
	})
	t.Run("own denies when no ownership pattern is configured", func(t *testing.T) {
		p := &Principal{Login: "alice", perms: map[Permission]struct{}{PermClawViewOwn: {}}}
		if p.Allows(PermClawViewAll, []string{"owner=alice"}) {
			t.Fatal("ownership was inferred without a configured pattern")
		}
	})
	t.Run("empty login grants all", func(t *testing.T) {
		if !(&Principal{}).Allows(PermSecretsManage, nil) {
			t.Fatal("empty login was denied")
		}
	})
	t.Run("tag scoped claw permissions follow the view scope", func(t *testing.T) {
		p := &Principal{Login: "alice", perms: map[Permission]struct{}{PermClawViewOwn: {}, PermClawMessagesRead: {}, PermClawTerminal: {}, PermClawFiles: {}, PermClawCheckpoint: {}}, ownTags: ownTags}
		for _, perm := range []Permission{PermClawMessagesRead, PermClawTerminal, PermClawFiles, PermClawCheckpoint} {
			if p.Allows(perm, []string{"owner=bob"}) {
				t.Fatalf("%s was granted on another user's claw", perm)
			}
			if !p.Allows(perm, []string{"owner=alice"}) {
				t.Fatalf("%s was denied on own claw", perm)
			}
		}
	})
	t.Run("tag scoped claw permissions still need the permission itself", func(t *testing.T) {
		p := &Principal{Login: "alice", perms: map[Permission]struct{}{PermClawViewAll: {}}, ownTags: ownTags}
		if p.Allows(PermClawTerminal, []string{"owner=alice"}) {
			t.Fatal("terminal was granted without claw:terminal")
		}
	})
}

func TestOwnershipTagPatternsKeepsOnlyUserPatterns(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, nil, "", "", "")
	s.hubCfg = &types.HubConfig{Auth: &types.AuthConfig{Access: &types.AccessConfig{
		ViewRequiresTags:     []string{"assignee={user}", "team=core"},
		InteractRequiresTags: []string{"assignee={user}", "owner={user}"},
	}}}
	got := s.ownershipTagPatterns()
	want := []string{"assignee={user}", "owner={user}"}
	if len(got) != len(want) {
		t.Fatalf("patterns = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("patterns = %v, want %v", got, want)
		}
	}
}
