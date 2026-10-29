package install

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/observability/stack"
)

func GrafanaURL(publicURL string) (string, error) {
	if publicURL == "" {
		return "http://localhost:3001/grafana/", nil
	}
	u, err := url.Parse(publicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(publicURL, "\r\n\t \"\\$%#;") {
		return "", fmt.Errorf("invalid public URL %q", publicURL)
	}
	return strings.TrimRight(publicURL, "/") + "/grafana/", nil
}

func (i *Installer) path(parts ...string) string {
	return filepath.Join(append([]string{i.Root}, parts...)...)
}

func (i *Installer) RenderUnit(c Component) string {
	bin := i.path(BinaryDir, c.Name, "current", c.Binary)
	cfg, data := i.path(ConfigDir), i.path(DataDir, c.Name)
	args := ""
	switch c.Name {
	case "collector":
		args = " --config=" + cfg + "/collector.yaml"
	case "victoriametrics":
		args = " -httpListenAddr=127.0.0.1:8428 -storageDataPath=" + data + " -retentionPeriod=15d -storage.minFreeDiskSpaceBytes=2GB -opentelemetry.usePrometheusNaming=true"
	case "victorialogs":
		args = " -httpListenAddr=127.0.0.1:9428 -storageDataPath=" + data + " -retentionPeriod=7d -retention.maxDiskSpaceUsageBytes=4GiB"
	case "tempo":
		args = " -target=all -config.file=" + cfg + "/tempo.yaml -config.expand-env=true"
	case "grafana":
		args = " server --homepath=" + i.path(BinaryDir, "grafana", "current") + " --config=" + cfg + "/grafana.ini"
	}
	return fmt.Sprintf(`[Unit]
Description=ElasticClaw observability %s
After=network.target

[Service]
User=elasticclaw-obs
Group=elasticclaw-obs
EnvironmentFile=%s/%s.env
ExecStart=%s%s
Restart=on-failure
RestartSec=5
MemoryMax=%s
NoNewPrivileges=yes
ProtectSystem=full
PrivateTmp=yes
UMask=0027

[Install]
WantedBy=multi-user.target
`, c.Name, cfg, c.Name, bin, args, c.Memory)
}

func (i *Installer) renderConfigs(grafanaURL, password string) error {
	for _, name := range []string{"collector.yaml", "tempo.yaml", "grafana/provisioning/datasources/datasources.yaml"} {
		content, err := stack.Files.ReadFile(name)
		if err != nil {
			return err
		}
		if err := i.write(i.path(ConfigDir, name), content, 0644); err != nil {
			return err
		}
	}
	envs := map[string]string{
		"collector": "OTLP_GRPC_ENDPOINT=127.0.0.1:4317\nOTLP_HTTP_ENDPOINT=127.0.0.1:4318\nCOLLECTOR_HEALTH_ENDPOINT=127.0.0.1:13133\nHOST_ROOT=/\nMETRICS_ENDPOINT=http://127.0.0.1:8428/opentelemetry\nLOGS_ENDPOINT=http://127.0.0.1:9428/insert/opentelemetry\nTRACES_ENDPOINT=127.0.0.1:4417\n",
		"tempo":     "TEMPO_LISTEN_ADDRESS=127.0.0.1\nTEMPO_ADVERTISE_ADDRESS=127.0.0.1\nTEMPO_OTLP_ENDPOINT=127.0.0.1:4417\nTEMPO_DATA_PATH=" + i.path(DataDir, "tempo") + "\n",
		"grafana":   "VICTORIAMETRICS_URL=http://127.0.0.1:8428\nVICTORIALOGS_URL=http://127.0.0.1:9428\nTEMPO_URL=http://127.0.0.1:3200\nGF_SECURITY_ADMIN_PASSWORD=" + password + "\n",
	}
	for _, c := range Components {
		if err := i.write(i.path(ConfigDir, c.Name+".env"), []byte(envs[c.Name]), 0600); err != nil {
			return err
		}
		if err := i.write(i.path(UnitDir, unitName(c.Name)), []byte(i.RenderUnit(c)), 0644); err != nil {
			return err
		}
	}
	ini := fmt.Sprintf(`[server]
http_addr = 127.0.0.1
http_port = 3001
root_url = %s
serve_from_sub_path = true
[paths]
data = %s
logs = %s
plugins = %s
provisioning = %s
[users]
allow_sign_up = false
[auth.anonymous]
enabled = false
[analytics]
reporting_enabled = false
check_for_updates = false
check_for_plugin_updates = false
[security]
admin_user = admin
[log]
mode = console
`, grafanaURL, i.path(DataDir, "grafana"), i.path(DataDir, "grafana", "logs"), i.path(DataDir, "grafana", "plugins"), i.path(ConfigDir, "grafana", "provisioning"))
	return i.write(i.path(ConfigDir, "grafana.ini"), []byte(ini), 0644)
}

func (i *Installer) write(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	_, err = f.Write(content)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func (i *Installer) ensurePassword() (string, bool, error) {
	path := i.path(PasswordFile)
	recovery := i.path(DataDir, ".credentials", "grafana-admin-password")
	password := ""
	created := false
	for _, candidate := range []string{path, recovery} {
		b, err := os.ReadFile(candidate)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		password = strings.TrimSpace(string(b))
		if password == "" || strings.ContainsAny(password, "\n\r\"'\\ $`") {
			return "", false, fmt.Errorf("invalid saved Grafana password file %s", candidate)
		}
		break
	}
	if password == "" {
		// Grafana only consumes its initial admin password when the database is created.
		if _, err := os.Stat(i.path(DataDir, "grafana", "grafana.db")); err == nil {
			return "", false, fmt.Errorf("existing Grafana database has no saved admin credential; restore %s before reinstalling", path)
		} else if !os.IsNotExist(err) {
			return "", false, err
		}
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", false, err
		}
		password, created = hex.EncodeToString(b), true
	}
	if err := os.MkdirAll(filepath.Dir(recovery), 0700); err != nil {
		return "", false, err
	}
	if err := os.Chmod(filepath.Dir(recovery), 0700); err != nil {
		return "", false, err
	}
	// Keep a root-only recovery copy with retained data across an uninstall.
	for _, target := range []string{path, recovery} {
		if _, err := os.Stat(target); os.IsNotExist(err) {
			if err := i.write(target, []byte(password+"\n"), 0600); err != nil {
				return "", false, err
			}
		} else if err != nil {
			return "", false, err
		}
		if err := os.Chmod(target, 0600); err != nil {
			return "", false, err
		}
	}
	return password, created, nil
}
