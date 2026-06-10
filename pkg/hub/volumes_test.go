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
	putReq.Header.Set("X-Claw-Token", "claw-token")
	putRec := httptest.NewRecorder()
	s.handleVolumeArchive(putRec, putReq)
	if putRec.Code != http.StatusOK {
		t.Fatalf("put status = %d: %s", putRec.Code, putRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/volumes/leases/"+acquired[0].LeaseID+"/archive", nil)
	getReq.SetPathValue("lease", acquired[0].LeaseID)
	getReq.Header.Set("X-Claw-Token", "claw-token")
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
