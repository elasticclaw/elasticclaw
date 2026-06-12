package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/types"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func handleVolumeAttach(ctx context.Context, conn *websocket.Conn, payload json.RawMessage) {
	var req types.VolumeAttachPayload
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Printf("[volume] bad attach payload: %v", err)
		return
	}
	err := attachVolume(ctx, req)
	ack := types.VolumeAttachAck{RequestID: req.RequestID, LeaseID: req.LeaseID, OK: err == nil}
	if err != nil {
		ack.Error = err.Error()
	}
	_ = wsjson.Write(ctx, conn, hubMsg{Type: "volume_attach_ack", Payload: mustJSON(ack)})
}

func handleVolumeSync(ctx context.Context, conn *websocket.Conn, payload json.RawMessage) {
	var req types.VolumeSyncPayload
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Printf("[volume] bad sync payload: %v", err)
		return
	}
	err := syncVolume(ctx, req)
	ack := types.VolumeSyncAck{RequestID: req.RequestID, LeaseID: req.LeaseID, OK: err == nil}
	if err != nil {
		ack.Error = err.Error()
	}
	_ = wsjson.Write(ctx, conn, hubMsg{Type: "volume_sync_ack", Payload: mustJSON(ack)})
}

func attachVolume(ctx context.Context, req types.VolumeAttachPayload) error {
	if err := validateVolumeMount(req.Mount); err != nil {
		return err
	}
	if err := os.MkdirAll(req.Mount, 0o750); err != nil {
		return fmt.Errorf("mkdir mount: %w", err)
	}
	url := fmt.Sprintf("%s/api/volumes/leases/%s/archive", strings.TrimRight(req.HubURL, "/"), req.LeaseID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("X-Claw-Token", req.ClawToken)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("download volume archive: HTTP %d: %s", resp.StatusCode, string(body))
	}
	return extractTarGzip(resp.Body, req.Mount)
}

func syncVolume(ctx context.Context, req types.VolumeSyncPayload) error {
	if req.Mode != "rw" {
		return nil
	}
	if err := validateVolumeMount(req.Mount); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	writeDone := make(chan error, 1)
	go func() {
		err := writeTarGzip(pw, req.Mount)
		_ = pw.CloseWithError(err)
		writeDone <- err
	}()
	url := fmt.Sprintf("%s/api/volumes/leases/%s/archive", strings.TrimRight(req.HubURL, "/"), req.LeaseID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, url, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		<-writeDone
		return err
	}
	httpReq.Header.Set("X-Claw-Token", req.ClawToken)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		_ = pr.CloseWithError(err)
		<-writeDone
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("upload volume archive: HTTP %d: %s", resp.StatusCode, string(body))
		_ = pr.CloseWithError(err)
		<-writeDone
		return err
	}
	return nil
}

func validateVolumeMount(mount string) error {
	mount = strings.TrimSpace(mount)
	if mount == "" || !filepath.IsAbs(mount) {
		return fmt.Errorf("volume mount must be absolute")
	}
	clean := filepath.Clean(mount)
	if clean == "/" ||
		strings.HasPrefix(clean, "/home/daytona/.openclaw/workspace") ||
		strings.HasPrefix(clean, "/workspace") {
		return fmt.Errorf("volume mount must be outside the repository workspace")
	}
	return nil
}

func extractTarGzip(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	cleanDest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("unsafe archive path %q", hdr.Name)
		}
		target := filepath.Join(cleanDest, name)
		if !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) && target != cleanDest {
			return fmt.Errorf("archive path escapes mount: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0o777); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(f, tr)
			closeErr := f.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
}

func writeTarGzip(w io.Writer, root string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if closeErr := tw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gz.Close(); err == nil {
		err = closeErr
	}
	return err
}
