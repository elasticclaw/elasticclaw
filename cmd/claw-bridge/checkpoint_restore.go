package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
)

func restoreStagedCheckpoint() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	stagingDir := filepath.Join(home, ".elasticclaw", "checkpoint-restore")
	marker, err := os.Lstat(filepath.Join(stagingDir, ".complete"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !marker.Mode().IsRegular() {
		return fmt.Errorf("checkpoint restore marker is not a regular file")
	}
	homeRoot, err := os.OpenRoot(home)
	if err != nil {
		return err
	}
	defer homeRoot.Close()
	count := 0
	err = filepath.WalkDir(stagingDir, func(source string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(stagingDir, source)
		if err != nil {
			return err
		}
		var destination, prefix string
		switch {
		case strings.HasPrefix(rel, "workspace/"):
			destination, prefix = ".openclaw/workspace", "workspace/"
		case strings.HasPrefix(rel, ".openclaw/agents/"):
			destination, prefix = ".openclaw/agents", ".openclaw/agents/"
		default:
			return nil
		}
		if err := homeRoot.MkdirAll(destination, 0755); err != nil {
			return err
		}
		root, err := homeRoot.OpenRoot(destination)
		if err != nil {
			return err
		}
		defer root.Close()
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if err := overlayCheckpointFile(root, strings.TrimPrefix(rel, prefix), data, info.Mode().Perm()); err != nil {
			return fmt.Errorf("restore %s: %w", rel, err)
		}
		count++
		return nil
	})
	if err != nil {
		return err
	}
	if err := os.RemoveAll(stagingDir); err != nil {
		return err
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
