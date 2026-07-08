package hub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestWorkflowVolumeLeaseModes(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	ro := []workflowVolumeRuntime{{
		WorkflowVolume: types.WorkflowVolume{Name: "fixtures", Source: "hub://volumes/fixtures", Mount: "/mnt/elasticclaw/fixtures", Mode: "ro"},
		Repo:           "volumes/fixtures",
		Tag:            "latest",
	}}
	firstRO, err := s.acquireWorkflowVolumeLeases(context.Background(), "claw-ro-1", ro)
	if err != nil {
		t.Fatalf("first ro lease: %v", err)
	}
	secondRO, err := s.acquireWorkflowVolumeLeases(context.Background(), "claw-ro-2", ro)
	if err != nil {
		t.Fatalf("second ro lease should share: %v", err)
	}
	rw := []workflowVolumeRuntime{{
		WorkflowVolume: types.WorkflowVolume{Name: "fixtures", Source: "hub://volumes/fixtures", Mount: "/mnt/elasticclaw/fixtures", Mode: "rw"},
		Repo:           "volumes/fixtures",
		Tag:            "latest",
	}}
	if _, err := s.acquireWorkflowVolumeLeases(context.Background(), "claw-rw", rw); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("rw lease with active ro leases error = %v, want locked", err)
	}

	s.releaseWorkflowVolumeLeases("claw-ro-1")
	s.releaseWorkflowVolumeLeases("claw-ro-2")
	if _, err := s.acquireWorkflowVolumeLeases(context.Background(), "claw-rw", rw); err != nil {
		t.Fatalf("rw lease after ro release: %v", err)
	}
	if _, err := s.acquireWorkflowVolumeLeases(context.Background(), "claw-ro-3", ro); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("ro lease with active rw lease error = %v, want locked", err)
	}
	if firstRO[0].ManifestDigest == "" || secondRO[0].ManifestDigest == "" {
		t.Fatal("expected leases to pin a manifest digest")
	}
}

func TestVolumeArchivePutAndGetUsesArtifactStore(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	volumes := []workflowVolumeRuntime{{
		WorkflowVolume: types.WorkflowVolume{Name: "scratch", Source: "hub://volumes/scratch", Mount: "/mnt/elasticclaw/scratch", Mode: "rw"},
		Repo:           "volumes/scratch",
		Tag:            "latest",
	}}
	acquired, err := s.acquireWorkflowVolumeLeases(context.Background(), "claw-rw", volumes)
	if err != nil {
		t.Fatalf("acquire rw lease: %v", err)
	}
	archive := testVolumeArchive(t, map[string]string{"state.txt": "hello volume"})

	putReq := httptest.NewRequest(http.MethodPut, "/api/volumes/leases/"+acquired[0].LeaseID+"/archive", bytes.NewReader(archive))
	putReq.SetPathValue("lease", acquired[0].LeaseID)
	setVolumeArchiveAuth(putReq, "claw-token", "claw-rw", acquired[0].AccessToken)
	putRec := httptest.NewRecorder()
	s.handleVolumeArchive(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("put status = %d: %s", putRec.Code, putRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/volumes/leases/"+acquired[0].LeaseID+"/archive", nil)
	getReq.SetPathValue("lease", acquired[0].LeaseID)
	setVolumeArchiveAuth(getReq, "claw-token", "claw-rw", acquired[0].AccessToken)
	getRec := httptest.NewRecorder()
	s.handleVolumeArchive(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", getRec.Code, getRec.Body.String())
	}
	files := readTestVolumeArchive(t, getRec.Body.Bytes())
	if files["state.txt"] != "hello volume" {
		t.Fatalf("archive files = %#v, want state.txt", files)
	}
}

func TestNormalizeWorkflowVolumesNamespacesTenant(t *testing.T) {
	workflow := &types.WorkflowConfig{Volumes: []types.WorkflowVolume{{
		Name:   "cache",
		Source: "hub://volumes/cache",
		Mount:  "/mnt/elasticclaw/cache",
		Mode:   "rw",
	}}}
	tenantA, err := normalizeWorkflowVolumes("tenant-a", workflow)
	if err != nil {
		t.Fatalf("normalize tenant a: %v", err)
	}
	tenantB, err := normalizeWorkflowVolumes("tenant-b", workflow)
	if err != nil {
		t.Fatalf("normalize tenant b: %v", err)
	}
	if tenantA[0].Repo != "volumes/tenant-a/cache" {
		t.Fatalf("tenant a repo = %q", tenantA[0].Repo)
	}
	if tenantB[0].Repo != "volumes/tenant-b/cache" {
		t.Fatalf("tenant b repo = %q", tenantB[0].Repo)
	}
	if tenantA[0].Repo == tenantB[0].Repo {
		t.Fatalf("tenant repos should be distinct: %q", tenantA[0].Repo)
	}
}

func TestVolumeArchiveRejectsWrongLeaseCredentials(t *testing.T) {
	s, _ := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	volumes := []workflowVolumeRuntime{{
		WorkflowVolume: types.WorkflowVolume{Name: "scratch", Source: "hub://volumes/scratch", Mount: "/mnt/elasticclaw/scratch", Mode: "rw"},
		Repo:           "volumes/test-tenant/scratch",
		Tag:            "latest",
	}}
	acquired, err := s.acquireWorkflowVolumeLeases(context.Background(), "claw-rw", volumes)
	if err != nil {
		t.Fatalf("acquire rw lease: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/volumes/leases/"+acquired[0].LeaseID+"/archive", nil)
	req.SetPathValue("lease", acquired[0].LeaseID)
	setVolumeArchiveAuth(req, "claw-token", "other-claw", acquired[0].AccessToken)
	rec := httptest.NewRecorder()
	s.handleVolumeArchive(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get with wrong claw status = %d, want %d", rec.Code, http.StatusNotFound)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/volumes/leases/"+acquired[0].LeaseID+"/archive", nil)
	req.SetPathValue("lease", acquired[0].LeaseID)
	setVolumeArchiveAuth(req, "claw-token", "claw-rw", "wrong-token")
	rec = httptest.NewRecorder()
	s.handleVolumeArchive(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get with wrong lease token status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestSyncWorkflowVolumesKeepsWritableLeaseWhenDisconnected(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	volumes := []workflowVolumeRuntime{{
		WorkflowVolume: types.WorkflowVolume{Name: "scratch", Source: "hub://volumes/scratch", Mount: "/mnt/elasticclaw/scratch", Mode: "rw"},
		Repo:           "volumes/scratch",
		Tag:            "latest",
	}}
	acquired, err := s.acquireWorkflowVolumeLeases(context.Background(), "claw-rw", volumes)
	if err != nil {
		t.Fatalf("acquire rw lease: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,datetime('now'))`,
		"claw-rw", "test-tenant-id", "claw rw", "test", "connected"); err != nil {
		t.Fatal(err)
	}
	if err := s.storeWorkflowVolumes("claw-rw", acquired); err != nil {
		t.Fatalf("store workflow volumes: %v", err)
	}

	s.syncWorkflowVolumes("claw-rw")

	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM volume_leases WHERE claw_id=? AND released_at IS NULL`, "claw-rw").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active writable leases after disconnected sync = %d, want 1", active)
	}
}

func TestSyncWorkflowVolumesSkipsReleasedWritableLease(t *testing.T) {
	s, db := NewTestServerWithConfig(t, &types.HubConfig{ClawToken: "claw-token"}, "", "", "")
	volumes := []workflowVolumeRuntime{{
		WorkflowVolume: types.WorkflowVolume{Name: "scratch", Source: "hub://volumes/scratch", Mount: "/mnt/elasticclaw/scratch", Mode: "rw"},
		Repo:           "volumes/scratch",
		Tag:            "latest",
	}}
	acquired, err := s.acquireWorkflowVolumeLeases(context.Background(), "claw-rw", volumes)
	if err != nil {
		t.Fatalf("acquire rw lease: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO claws(id, tenant_id, name, template, status, created_at) VALUES(?,?,?,?,?,datetime('now'))`,
		"claw-rw", "test-tenant-id", "claw rw", "test", "connected"); err != nil {
		t.Fatal(err)
	}
	if err := s.storeWorkflowVolumes("claw-rw", acquired); err != nil {
		t.Fatalf("store workflow volumes: %v", err)
	}
	s.releaseWorkflowVolumeLeases("claw-rw")
	s.mu.Lock()
	s.claws["claw-rw"] = &clawConn{ClawID: "claw-rw", TenantID: "test-tenant-id"}
	s.mu.Unlock()

	s.syncWorkflowVolumes("claw-rw")

	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM volume_leases WHERE claw_id=? AND released_at IS NULL`, "claw-rw").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("active writable leases after released sync = %d, want 0", active)
	}
}

func setVolumeArchiveAuth(req *http.Request, clawToken, clawID, leaseToken string) {
	req.Header.Set("X-Claw-Token", clawToken)
	req.Header.Set("X-Claw-ID", clawID)
	req.Header.Set("X-Volume-Token", leaseToken)
}

func testVolumeArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		data := []byte(content)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o640, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readTestVolumeArchive(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = string(body)
	}
}
