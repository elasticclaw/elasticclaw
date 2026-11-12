package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// stagedCheckpointManifest mirrors the hub's .complete marker: Files[i] is
// staged as files/<i>, with the mode captured in the checkpoint.
type stagedCheckpointManifest struct {
	Files []struct {
		Path string `json:"path"`
		Mode int64  `json:"mode"`
	} `json:"files"`
}

func restoreStagedCheckpoint() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	stagingDir := filepath.Join(home, ".elasticclaw", "checkpoint-restore")
	markerPath := filepath.Join(stagingDir, ".complete")
	marker, err := os.Lstat(markerPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !marker.Mode().IsRegular() {
		return fmt.Errorf("checkpoint restore marker is not a regular file")
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return err
	}
	var manifest stagedCheckpointManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("parse checkpoint restore marker: %w", err)
	}
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer homeRoot.Close()
	count := 0
	for i, f := range manifest.Files {
		var destination, prefix string
		switch {
		case strings.HasPrefix(f.Path, "workspace/"):
			destination, prefix = ".openclaw/workspace", "workspace/"
		case strings.HasPrefix(f.Path, ".openclaw/agents/"):
			destination, prefix = ".openclaw/agents", ".openclaw/agents/"
		default:
			continue
		}
		source := filepath.Join(stagingDir, "files", strconv.Itoa(i))
		info, err := os.Lstat(source)
		if err != nil {
			return fmt.Errorf("restore %s: %w", f.Path, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		mode := os.FileMode(f.Mode).Perm()
		if mode == 0 {
			mode = 0644
		}
		content, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if err := homeRoot.MkdirAll(destination, 0755); err != nil {
			return err
		}
		root, err := homeRoot.OpenRoot(destination)
		if err != nil {
			return err
		}
		err = overlayCheckpointFile(root, strings.TrimPrefix(f.Path, prefix), content, mode)
		root.Close()
		if err != nil {
			return fmt.Errorf("restore %s: %w", f.Path, err)
		}
		count++
	}
	// Removing the marker is what keeps a later bootstrap from overlaying
	// stale checkpoint files again; the rest of the staging dir is only disk.
	if err := os.Remove(markerPath); err != nil {
		return err
	}
	if err := os.RemoveAll(stagingDir); err != nil {
		log.Printf("[bootstrap] warning: remove checkpoint staging dir: %v", err)
	}
	log.Printf("[bootstrap] restored %d checkpoint files", count)
	return nil
}

func overlayCheckpointFile(root *os.Root, rel string, data []byte, mode os.FileMode) error {
	if !filepath.IsLocal(rel) {
		return fmt.Errorf("checkpoint path escapes destination: %q", rel)
	}
	if err := root.MkdirAll(filepath.Dir(rel), 0755); err != nil {
		return err
	}
	file, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Chmod(mode)
}
