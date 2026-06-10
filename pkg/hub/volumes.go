package hub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/artifact"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
	"nhooyr.io/websocket/wsjson"
)

const (
	volumeModeRO       = "ro"
	volumeModeRW       = "rw"
	volumeDefaultTag   = "latest"
	volumeLeaseTTL     = 24 * time.Hour
	volumeAttachWait   = 2 * time.Minute
	volumeSyncWait     = 3 * time.Minute
	emptyVolumeArchive = "empty"
)

type workflowVolumeRuntime struct {
	types.WorkflowVolume
	LeaseID        string `json:"lease_id,omitempty"`
	Repo           string `json:"repo,omitempty"`
	Tag            string `json:"tag,omitempty"`
	ManifestDigest string `json:"manifest_digest,omitempty"`
}

type volumeManifest struct {
	SchemaVersion string              `json:"schemaVersion"`
	MediaType     string              `json:"mediaType"`
	CreatedAt     time.Time           `json:"createdAt"`
	Layer         volumeManifestLayer `json:"layer"`
	Annotations   map[string]string   `json:"annotations,omitempty"`
}

type volumeManifestLayer struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
}

func parseVolumeSource(source string) (repo, tag string, err error) {
	source = strings.TrimSpace(source)
	const prefix = "hub://volumes/"
	if !strings.HasPrefix(source, prefix) {
		return "", "", fmt.Errorf("volume source %q must use hub://volumes/<name>", source)
	}
	ref := strings.TrimPrefix(source, prefix)
	if ref == "" {
		return "", "", fmt.Errorf("volume source is missing a name")
	}
	tag = volumeDefaultTag
	if before, after, ok := strings.Cut(ref, ":"); ok {
		ref = before
		if strings.TrimSpace(after) != "" {
			tag = strings.TrimSpace(after)
		}
	}
	repo = "volumes/" + strings.Trim(ref, "/")
	if err := artifact.ValidateRef(repo, tag); err != nil {
		return "", "", err
	}
	return repo, tag, nil
}

func normalizeWorkflowVolumes(workflow *types.WorkflowConfig) ([]workflowVolumeRuntime, error) {
	if workflow == nil || len(workflow.Volumes) == 0 {
		return nil, nil
	}
	out := make([]workflowVolumeRuntime, 0, len(workflow.Volumes))
	for _, v := range workflow.Volumes {
		mode := strings.TrimSpace(v.Mode)
		if mode == "" {
			mode = volumeModeRO
		}
		repo, tag, err := parseVolumeSource(v.Source)
		if err != nil {
			return nil, fmt.Errorf("volume %q: %w", v.Name, err)
		}
		out = append(out, workflowVolumeRuntime{
			WorkflowVolume: types.WorkflowVolume{
				Name:   strings.TrimSpace(v.Name),
				Source: strings.TrimSpace(v.Source),
				Mount:  strings.TrimSpace(v.Mount),
				Mode:   mode,
			},
			Repo: repo,
			Tag:  tag,
		})
	}
	return out, nil
}

func (s *Server) acquireWorkflowVolumeLeases(ctx context.Context, clawID string, volumes []workflowVolumeRuntime) ([]workflowVolumeRuntime, error) {
	if len(volumes) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	expires := now.Add(volumeLeaseTTL)
	acquired := make([]workflowVolumeRuntime, 0, len(volumes))
	for _, volume := range volumes {
		manifestDigest, err := s.resolveVolumeManifest(ctx, volume.Repo, volume.Tag)
		if err != nil {
			return nil, err
		}
		var conflicting int
		if volume.Mode == volumeModeRO {
			err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM volume_leases WHERE volume_id=? AND mode='rw' AND released_at IS NULL AND expires_at > ?`, volume.Repo, now).Scan(&conflicting)
		} else {
			err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM volume_leases WHERE volume_id=? AND released_at IS NULL AND expires_at > ?`, volume.Repo, now).Scan(&conflicting)
		}
		if err != nil {
			return nil, err
		}
		if conflicting > 0 {
			return nil, fmt.Errorf("volume %q is locked by an active lease", volume.Name)
		}
		volume.LeaseID = uuid.New().String()
		volume.ManifestDigest = manifestDigest
		if _, err := tx.ExecContext(ctx, `INSERT INTO volume_leases(id, volume_id, repo, tag, claw_id, mode, mount, manifest_digest, acquired_at, expires_at, heartbeat_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			volume.LeaseID, volume.Repo, volume.Repo, volume.Tag, clawID, volume.Mode, volume.Mount, manifestDigest, now, expires, now); err != nil {
			return nil, err
		}
		acquired = append(acquired, volume)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return acquired, nil
}

func (s *Server) resolveVolumeManifest(ctx context.Context, repo, tag string) (string, error) {
	digest, err := s.artifacts.ResolveRef(ctx, repo, tag)
	if err == nil {
		return digest, nil
	}
	if !strings.Contains(err.Error(), "no such file") && !strings.Contains(err.Error(), "not found") {
		return "", err
	}
	return s.createEmptyVolumeManifest(ctx, repo, tag)
}

func (s *Server) createEmptyVolumeManifest(ctx context.Context, repo, tag string) (string, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	layerDigest, size, err := s.artifacts.PutBlob(ctx, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return "", err
	}
	manifest := volumeManifest{
		SchemaVersion: "v1",
		MediaType:     artifact.MediaTypeVolumeV1,
		CreatedAt:     time.Now().UTC(),
		Layer: volumeManifestLayer{
			MediaType: artifact.MediaTypeVolumeLayerTarGz,
			Digest:    layerDigest,
			Size:      size,
		},
		Annotations: map[string]string{"elasticclaw.volume.empty": emptyVolumeArchive},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	manifestDigest, err := s.artifacts.PutManifest(ctx, data)
	if err != nil {
		return "", err
	}
	if err := s.artifacts.Tag(ctx, repo, tag, manifestDigest); err != nil {
		return "", err
	}
	return manifestDigest, nil
}

func (s *Server) releaseWorkflowVolumeLeases(clawID string) {
	_, _ = s.db.Exec(`UPDATE volume_leases SET released_at=? WHERE claw_id=? AND released_at IS NULL`, time.Now().UTC(), clawID)
}

func (s *Server) heartbeatWorkflowVolumeLeases(clawID string) {
	now := time.Now().UTC()
	_, _ = s.db.Exec(`UPDATE volume_leases SET heartbeat_at=?, expires_at=? WHERE claw_id=? AND released_at IS NULL`, now, now.Add(volumeLeaseTTL), clawID)
}

func (s *Server) workflowVolumesForClaw(clawID string) ([]workflowVolumeRuntime, error) {
	var raw string
	if err := s.db.QueryRow(`SELECT COALESCE(workflow_volumes,'[]') FROM claws WHERE id=?`, clawID).Scan(&raw); err != nil {
		return nil, err
	}
	var volumes []workflowVolumeRuntime
	if err := json.Unmarshal([]byte(raw), &volumes); err != nil {
		return nil, err
	}
	return volumes, nil
}

func (s *Server) storeWorkflowVolumes(clawID string, volumes []workflowVolumeRuntime) error {
	data, err := json.Marshal(volumes)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE claws SET workflow_volumes=? WHERE id=?`, string(data), clawID)
	return err
}

func (s *Server) attachWorkflowVolumes(ctx context.Context, cc *clawConn, clawID string) error {
	volumes, err := s.workflowVolumesForClaw(clawID)
	if err != nil || len(volumes) == 0 {
		return err
	}
	if volumes[0].LeaseID == "" {
		volumes, err = s.acquireWorkflowVolumeLeases(ctx, clawID, volumes)
		if err != nil {
			return err
		}
		if err := s.storeWorkflowVolumes(clawID, volumes); err != nil {
			s.releaseWorkflowVolumeLeases(clawID)
			return err
		}
	}
	for _, volume := range volumes {
		if err := s.dispatchVolumeAttach(ctx, cc, volume); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) dispatchVolumeAttach(ctx context.Context, cc *clawConn, volume workflowVolumeRuntime) error {
	reqID := uuid.New().String()
	ch := make(chan types.VolumeAttachAck, 1)
	s.fileAckMu.Lock()
	if s.volumeAttachWaiters == nil {
		s.volumeAttachWaiters = make(map[string]chan types.VolumeAttachAck)
	}
	s.volumeAttachWaiters[reqID] = ch
	s.fileAckMu.Unlock()
	defer func() {
		s.fileAckMu.Lock()
		delete(s.volumeAttachWaiters, reqID)
		s.fileAckMu.Unlock()
	}()
	payload := types.VolumeAttachPayload{
		RequestID: reqID,
		LeaseID:   volume.LeaseID,
		Name:      volume.Name,
		Mode:      volume.Mode,
		Mount:     volume.Mount,
		HubURL:    s.clawHubURL(),
		ClawToken: s.hubCfg.ClawToken,
	}
	if err := wsjson.Write(ctx, cc.conn, types.WSMessage{Type: "volume_attach", Payload: payload}); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, volumeAttachWait)
	defer cancel()
	select {
	case ack := <-ch:
		if !ack.OK {
			return fmt.Errorf("attach volume %q: %s", volume.Name, ack.Error)
		}
		return nil
	case <-waitCtx.Done():
		return fmt.Errorf("attach volume %q: %w", volume.Name, waitCtx.Err())
	}
}

func (s *Server) syncWorkflowVolumes(clawID string) {
	volumes, err := s.workflowVolumesForClaw(clawID)
	if err != nil || len(volumes) == 0 {
		return
	}
	s.mu.RLock()
	cc := s.claws[clawID]
	s.mu.RUnlock()
	if cc == nil {
		s.releaseWorkflowVolumeLeases(clawID)
		return
	}
	for _, volume := range volumes {
		if volume.Mode != volumeModeRW || volume.LeaseID == "" {
			continue
		}
		if err := s.dispatchVolumeSync(context.Background(), cc, volume); err != nil {
			log.Printf("[volume] sync %s/%s failed: %v", clawID, volume.Name, err)
		}
	}
	s.releaseWorkflowVolumeLeases(clawID)
}

func (s *Server) dispatchVolumeSync(ctx context.Context, cc *clawConn, volume workflowVolumeRuntime) error {
	reqID := uuid.New().String()
	ch := make(chan types.VolumeSyncAck, 1)
	s.fileAckMu.Lock()
	if s.volumeSyncWaiters == nil {
		s.volumeSyncWaiters = make(map[string]chan types.VolumeSyncAck)
	}
	s.volumeSyncWaiters[reqID] = ch
	s.fileAckMu.Unlock()
	defer func() {
		s.fileAckMu.Lock()
		delete(s.volumeSyncWaiters, reqID)
		s.fileAckMu.Unlock()
	}()
	payload := types.VolumeSyncPayload{
		RequestID: reqID,
		LeaseID:   volume.LeaseID,
		Name:      volume.Name,
		Mode:      volume.Mode,
		Mount:     volume.Mount,
		HubURL:    s.clawHubURL(),
		ClawToken: s.hubCfg.ClawToken,
	}
	if err := wsjson.Write(ctx, cc.conn, types.WSMessage{Type: "volume_sync", Payload: payload}); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, volumeSyncWait)
	defer cancel()
	select {
	case ack := <-ch:
		if !ack.OK {
			return fmt.Errorf("sync volume %q: %s", volume.Name, ack.Error)
		}
		return nil
	case <-waitCtx.Done():
		return fmt.Errorf("sync volume %q: %w", volume.Name, waitCtx.Err())
	}
}

func (s *Server) handleVolumeArchive(w http.ResponseWriter, r *http.Request) {
	leaseID := strings.TrimSpace(r.PathValue("lease"))
	if leaseID == "" {
		http.Error(w, "lease required", http.StatusBadRequest)
		return
	}
	if !s.authenticateClawToken(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleVolumeArchiveGet(w, r, leaseID)
	case http.MethodPut:
		s.handleVolumeArchivePut(w, r, leaseID)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleVolumeArchiveGet(w http.ResponseWriter, r *http.Request, leaseID string) {
	manifestDigest, err := s.volumeLeaseManifest(r.Context(), leaseID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	data, err := s.artifacts.GetManifest(r.Context(), manifestDigest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var manifest volumeManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, err := s.artifacts.GetBlob(r.Context(), manifest.Layer.Digest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", manifest.Layer.MediaType)
	_, _ = io.Copy(w, body)
}

func (s *Server) handleVolumeArchivePut(w http.ResponseWriter, r *http.Request, leaseID string) {
	var repo, tag, mode, attachedDigest string
	if err := s.db.QueryRow(`SELECT repo, tag, mode, manifest_digest FROM volume_leases WHERE id=? AND released_at IS NULL AND expires_at > ?`, leaseID, time.Now().UTC()).Scan(&repo, &tag, &mode, &attachedDigest); err != nil {
		http.Error(w, "active lease not found", http.StatusNotFound)
		return
	}
	if mode != volumeModeRW {
		http.Error(w, "volume is read-only", http.StatusForbidden)
		return
	}
	currentDigest, err := s.artifacts.ResolveRef(r.Context(), repo, tag)
	if err != nil {
		http.Error(w, "resolve volume ref: "+err.Error(), http.StatusConflict)
		return
	}
	if currentDigest != attachedDigest {
		http.Error(w, "volume changed since lease was acquired", http.StatusConflict)
		return
	}
	layerDigest, size, err := s.artifacts.PutBlob(r.Context(), r.Body)
	if err != nil {
		http.Error(w, "store volume layer: "+err.Error(), http.StatusInternalServerError)
		return
	}
	manifest := volumeManifest{
		SchemaVersion: "v1",
		MediaType:     artifact.MediaTypeVolumeV1,
		CreatedAt:     time.Now().UTC(),
		Layer: volumeManifestLayer{
			MediaType: artifact.MediaTypeVolumeLayerTarGz,
			Digest:    layerDigest,
			Size:      size,
		},
	}
	data, _ := json.Marshal(manifest)
	manifestDigest, err := s.artifacts.PutManifest(r.Context(), data)
	if err != nil {
		http.Error(w, "store volume manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.artifacts.Tag(r.Context(), repo, tag, manifestDigest); err != nil {
		http.Error(w, "tag volume: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = s.db.Exec(`UPDATE volume_leases SET manifest_digest=?, heartbeat_at=? WHERE id=?`, manifestDigest, time.Now().UTC(), leaseID)
	jsonOK(w, map[string]string{"manifest_digest": manifestDigest})
}

func (s *Server) volumeLeaseManifest(ctx context.Context, leaseID string) (string, error) {
	var digest string
	err := s.db.QueryRowContext(ctx, `SELECT manifest_digest FROM volume_leases WHERE id=? AND released_at IS NULL AND expires_at > ?`, leaseID, time.Now().UTC()).Scan(&digest)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("active lease not found")
	}
	return digest, err
}

func (s *Server) authenticateClawToken(w http.ResponseWriter, r *http.Request) bool {
	token := r.Header.Get("X-Claw-Token")
	if token == "" {
		token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	s.mu.RLock()
	want := s.hubCfg.ClawToken
	s.mu.RUnlock()
	if token == "" || want == "" || token != want {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}
