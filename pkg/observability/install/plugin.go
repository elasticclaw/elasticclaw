package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const logsPlugin = "victoriametrics-logs-datasource"

func pluginVersion(path string) (string, error) {
	data, err := os.ReadFile(filepath.Join(path, "plugin.json"))
	if err != nil {
		return "", err
	}
	var manifest struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "", err
	}
	return manifest.Info.Version, nil
}

func (i *Installer) installPlugin(ctx context.Context) error {
	plugins := i.path(DataDir, "grafana", "plugins")
	dest := filepath.Join(plugins, logsPlugin)
	if version, err := pluginVersion(dest); err == nil && version == LogsPluginVersion {
		return nil
	}
	if err := os.MkdirAll(plugins, 0755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(plugins, ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if _, err := i.Runner.Run(ctx, i.path(BinaryDir, "grafana", "current", "bin/grafana"), "cli", "--homepath", i.path(BinaryDir, "grafana", "current"), "--pluginsDir", staging, "plugins", "install", logsPlugin, LogsPluginVersion); err != nil {
		return err
	}
	next := filepath.Join(staging, logsPlugin)
	version, err := pluginVersion(next)
	if err != nil {
		return err
	}
	if version != LogsPluginVersion {
		return fmt.Errorf("Grafana plugin version %q does not match pinned %s", version, LogsPluginVersion)
	}
	previous := filepath.Join(staging, "previous")
	hadPrevious := false
	if err := os.Rename(dest, previous); err == nil {
		hadPrevious = true
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(next, dest); err != nil {
		if hadPrevious {
			return errors.Join(err, os.Rename(previous, dest))
		}
		return err
	}
	return nil
}
