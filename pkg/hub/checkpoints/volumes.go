package checkpoints

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/hub/artifact"
	"github.com/elasticclaw/elasticclaw/pkg/hub/httpserver"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
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

type WorkflowVolumeRuntime struct {
	types.WorkflowVolume
	LeaseID        string `json:"lease_id,omitempty"`
	AccessToken    string `json:"access_token,omitempty"`
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

func parseVolumeSource(tenantID, source string) (repo, tag string, err error) {
	tenantID = strings.TrimSpace(tenantID)
	if tenantID == "" {
		return "", "", fmt.Errorf("tenant id is required")
	}
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
	repo = "volumes/" + tenantID + "/" + strings.Trim(ref, "/")
	if err := artifact.ValidateRef(repo, tag); err != nil {
		return "", "", err
	}
	return repo, tag, nil
}

func NormalizeWorkflowVolumes(tenantID string, workflow *types.WorkflowConfig) ([]WorkflowVolumeRuntime, error) {
	if workflow == nil || len(workflow.Volumes) == 0 {
		return nil, nil
	}
	out := make([]WorkflowVolumeRuntime, 0, len(workflow.Volumes))
	for _, v := range workflow.Volumes {
		mode := strings.TrimSpace(v.Mode)
		if mode == "" {
			mode = volumeModeRO
		}
		repo, tag, err := parseVolumeSource(tenantID, v.Source)
		if err != nil {
			return nil, fmt.Errorf("volume %q: %w", v.Name, err)
		}
		out = append(out, WorkflowVolumeRuntime{
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

func (s *Service) AcquireWorkflowVolumeLeases(ctx context.Context, clawID string, volumes []WorkflowVolumeRuntime) ([]WorkflowVolumeRuntime, error) {
	if len(volumes) == 0 {
		return nil, nil
	}
	tx, err := s.deps.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	expires := now.Add(volumeLeaseTTL)
	acquired := make([]WorkflowVolumeRuntime, 0, len(volumes))
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
		volume.AccessToken = uuid.New().String()
		volume.ManifestDigest = manifestDigest
		if _, err := tx.ExecContext(ctx, `INSERT INTO volume_leases(id, volume_id, repo, tag, claw_id, access_token, mode, mount, manifest_digest, acquired_at, expires_at, heartbeat_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			volume.LeaseID, volume.Repo, volume.Repo, volume.Tag, clawID, volume.AccessToken, volume.Mode, volume.Mount, manifestDigest, now, expires, now); err != nil {
			return nil, err
		}
		acquired = append(acquired, volume)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return acquired, nil
}

func (s *Service) resolveVolumeManifest(ctx context.Context, repo, tag string) (string, error) {
	digest, err := s.artifacts().ResolveRef(ctx, repo, tag)
	if err == nil {
		return digest, nil
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "no such file") && !strings.Contains(msg, "not found") {
		return "", err
	}
	return s.createEmptyVolumeManifest(ctx, repo, tag)
}

func (s *Service) createEmptyVolumeManifest(ctx context.Context, repo, tag string) (string, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	layerDigest, size, err := s.artifacts().PutBlob(ctx, bytes.NewReader(buf.Bytes()))
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
	manifestDigest, err := s.artifacts().PutManifest(ctx, data)
	if err != nil {
		return "", err
	}
	if err := s.artifacts().Tag(ctx, repo, tag, manifestDigest); err != nil {
		return "", err
	}
	return manifestDigest, nil
}

func (s *Service) ReleaseWorkflowVolumeLeases(clawID string) {
	_, _ = s.deps.DB.Exec(`UPDATE volume_leases SET released_at=? WHERE claw_id=? AND released_at IS NULL`, time.Now().UTC(), clawID)
}

func (s *Service) HeartbeatWorkflowVolumeLeases(clawID string) {
	now := time.Now().UTC()
	_, _ = s.deps.DB.Exec(`UPDATE volume_leases SET heartbeat_at=?, expires_at=? WHERE claw_id=? AND released_at IS NULL`, now, now.Add(volumeLeaseTTL), clawID)
}

func (s *Service) workflowVolumeLeaseActive(leaseID string) bool {
	var active int
	err := s.deps.DB.QueryRow(`SELECT COUNT(*) FROM volume_leases WHERE id=? AND released_at IS NULL AND expires_at > ?`, leaseID, time.Now().UTC()).Scan(&active)
	return err == nil && active > 0
}

func (s *Service) workflowVolumesForClaw(clawID string) ([]WorkflowVolumeRuntime, error) {
	var raw string
	if err := s.deps.DB.QueryRow(`SELECT COALESCE(workflow_volumes,'[]') FROM claws WHERE id=?`, clawID).Scan(&raw); err != nil {
		return nil, err
	}
	var volumes []WorkflowVolumeRuntime
	if err := json.Unmarshal([]byte(raw), &volumes); err != nil {
		return nil, err
	}
	return volumes, nil
}

func (s *Service) StoreWorkflowVolumes(clawID string, volumes []WorkflowVolumeRuntime) error {
	data, err := json.Marshal(volumes)
	if err != nil {
		return err
	}
	_, err = s.deps.DB.Exec(`UPDATE claws SET workflow_volumes=? WHERE id=?`, string(data), clawID)
	return err
}

func (s *Service) AttachWorkflowVolumes(ctx context.Context, cc Conn, clawID string) error {
	volumes, err := s.workflowVolumesForClaw(clawID)
	if err != nil || len(volumes) == 0 {
		return err
	}
	if volumes[0].LeaseID == "" {
		volumes, err = s.AcquireWorkflowVolumeLeases(ctx, clawID, volumes)
		if err != nil {
			return err
		}
		if err := s.StoreWorkflowVolumes(clawID, volumes); err != nil {
			s.ReleaseWorkflowVolumeLeases(clawID)
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

func (s *Service) dispatchVolumeAttach(ctx context.Context, cc Conn, volume WorkflowVolumeRuntime) error {
	reqID := uuid.New().String()
	ch := make(chan types.VolumeAttachAck, 1)
	s.deps.FileAckMu.Lock()
	s.deps.VolumeAttachWaiters()[reqID] = ch
	s.deps.FileAckMu.Unlock()
	defer func() {
		s.deps.FileAckMu.Lock()
		delete(s.deps.VolumeAttachWaiters(), reqID)
		s.deps.FileAckMu.Unlock()
	}()
	payload := types.VolumeAttachPayload{
		RequestID:  reqID,
		LeaseID:    volume.LeaseID,
		ClawID:     cc.ID(),
		LeaseToken: volume.AccessToken,
		Name:       volume.Name,
		Mode:       volume.Mode,
		Mount:      volume.Mount,
		HubURL:     s.clawHubURL(),
		ClawToken:  s.hubCfg().ClawToken,
	}
	if err := cc.WriteWS(ctx, types.WSMessage{Type: "volume_attach", Payload: payload}); err != nil {
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

func (s *Service) SyncWorkflowVolumes(clawID string) {
	volumes, err := s.workflowVolumesForClaw(clawID)
	if err != nil || len(volumes) == 0 {
		return
	}
	hasActiveWritableLease := false
	for _, volume := range volumes {
		if volume.Mode == volumeModeRW && volume.LeaseID != "" && s.workflowVolumeLeaseActive(volume.LeaseID) {
			hasActiveWritableLease = true
			break
		}
	}
	cc := s.deps.ClawConn(clawID)
	if cc == nil {
		if hasActiveWritableLease {
			logf("[volume] sync %s skipped: claw disconnected with writable volume lease; keeping lease until expiry", clawID)
			return
		}
		s.ReleaseWorkflowVolumeLeases(clawID)
		return
	}
	syncFailed := false
	for _, volume := range volumes {
		if volume.Mode != volumeModeRW || volume.LeaseID == "" {
			continue
		}
		if !s.workflowVolumeLeaseActive(volume.LeaseID) {
			continue
		}
		if err := s.dispatchVolumeSync(context.Background(), cc, volume); err != nil {
			logf("[volume] sync %s/%s failed: %v", clawID, volume.Name, err)
			syncFailed = true
		}
	}
	if syncFailed {
		logf("[volume] sync %s incomplete; keeping writable volume leases until expiry", clawID)
		return
	}
	s.ReleaseWorkflowVolumeLeases(clawID)
}

func (s *Service) dispatchVolumeSync(ctx context.Context, cc Conn, volume WorkflowVolumeRuntime) error {
	reqID := uuid.New().String()
	ch := make(chan types.VolumeSyncAck, 1)
	s.deps.FileAckMu.Lock()
	s.deps.VolumeSyncWaiters()[reqID] = ch
	s.deps.FileAckMu.Unlock()
	defer func() {
		s.deps.FileAckMu.Lock()
		delete(s.deps.VolumeSyncWaiters(), reqID)
		s.deps.FileAckMu.Unlock()
	}()
	payload := types.VolumeSyncPayload{
		RequestID:  reqID,
		LeaseID:    volume.LeaseID,
		ClawID:     cc.ID(),
		LeaseToken: volume.AccessToken,
		Name:       volume.Name,
		Mode:       volume.Mode,
		Mount:      volume.Mount,
		HubURL:     s.clawHubURL(),
		ClawToken:  s.hubCfg().ClawToken,
	}
	if err := cc.WriteWS(ctx, types.WSMessage{Type: "volume_sync", Payload: payload}); err != nil {
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

func (s *Service) HandleVolumeArchive(w http.ResponseWriter, r *http.Request) {
	leaseID := strings.TrimSpace(r.PathValue("lease"))
	if leaseID == "" {
		http.Error(w, "lease required", http.StatusBadRequest)
		return
	}
	if !s.authenticateClawToken(w, r) {
		return
	}
	clawID := strings.TrimSpace(r.Header.Get("X-Claw-ID"))
	accessToken := strings.TrimSpace(r.Header.Get("X-Volume-Token"))
	if clawID == "" || accessToken == "" {
		http.Error(w, "volume lease credentials required", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleVolumeArchiveGet(w, r, leaseID, clawID, accessToken)
	case http.MethodPut:
		s.handleVolumeArchivePut(w, r, leaseID, clawID, accessToken)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Service) handleVolumeArchiveGet(w http.ResponseWriter, r *http.Request, leaseID, clawID, accessToken string) {
	manifestDigest, err := s.volumeLeaseManifest(r.Context(), leaseID, clawID, accessToken)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	data, err := s.artifacts().GetManifest(r.Context(), manifestDigest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var manifest volumeManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body, err := s.artifacts().GetBlob(r.Context(), manifest.Layer.Digest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", manifest.Layer.MediaType)
	_, _ = io.Copy(w, body)
}

func (s *Service) handleVolumeArchivePut(w http.ResponseWriter, r *http.Request, leaseID, clawID, accessToken string) {
	var repo, tag, mode, attachedDigest string
	if err := s.deps.DB.QueryRow(`SELECT repo, tag, mode, manifest_digest FROM volume_leases WHERE id=? AND claw_id=? AND access_token=? AND released_at IS NULL AND expires_at > ?`, leaseID, clawID, accessToken, time.Now().UTC()).Scan(&repo, &tag, &mode, &attachedDigest); err != nil {
		http.Error(w, "active lease not found", http.StatusNotFound)
		return
	}
	if mode != volumeModeRW {
		http.Error(w, "volume is read-only", http.StatusForbidden)
		return
	}
	currentDigest, err := s.artifacts().ResolveRef(r.Context(), repo, tag)
	if err != nil {
		http.Error(w, "resolve volume ref: "+err.Error(), http.StatusConflict)
		return
	}
	if currentDigest != attachedDigest {
		http.Error(w, "volume changed since lease was acquired", http.StatusConflict)
		return
	}
	layerDigest, size, err := s.artifacts().PutBlob(r.Context(), r.Body)
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
	manifestDigest, err := s.artifacts().PutManifest(r.Context(), data)
	if err != nil {
		http.Error(w, "store volume manifest: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.artifacts().Tag(r.Context(), repo, tag, manifestDigest); err != nil {
		http.Error(w, "tag volume: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := s.deps.DB.Exec(`UPDATE volume_leases SET manifest_digest=?, heartbeat_at=? WHERE id=? AND claw_id=? AND access_token=?`, manifestDigest, time.Now().UTC(), leaseID, clawID, accessToken); err != nil {
		logfCtx(r.Context(), "[volume] lease %s: failed to update manifest_digest after successful tag: %v", leaseID, err)
	}
	httpserver.JSONOK(w, map[string]string{"manifest_digest": manifestDigest})
}

func (s *Service) volumeLeaseManifest(ctx context.Context, leaseID, clawID, accessToken string) (string, error) {
	var digest string
	err := s.deps.DB.QueryRowContext(ctx, `SELECT manifest_digest FROM volume_leases WHERE id=? AND claw_id=? AND access_token=? AND released_at IS NULL AND expires_at > ?`, leaseID, clawID, accessToken, time.Now().UTC()).Scan(&digest)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("active lease not found")
	}
	return digest, err
}

func (s *Service) authenticateClawToken(w http.ResponseWriter, r *http.Request) bool {
	token := r.Header.Get("X-Claw-Token")
	if token == "" {
		token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	s.deps.CfgMu.RLock()
	want := s.hubCfg().ClawToken
	s.deps.CfgMu.RUnlock()
	if token == "" || want == "" || token != want {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}
