package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/observability/stack"
	"gopkg.in/yaml.v3"
)

type runnerFunc func(context.Context, string, ...string) (string, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) (string, error) {
	return f(ctx, name, args...)
}

type clientFunc func(*http.Request) (*http.Response, error)

func (f clientFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func testInstaller(t *testing.T) *Installer {
	t.Helper()
	i := New(t.TempDir(), io.Discard)
	i.OS, i.Arch, i.UID = "linux", "amd64", 0
	i.Runner = runnerFunc(func(context.Context, string, ...string) (string, error) { return "", nil })
	i.HTTP = clientFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	if err := os.MkdirAll(i.path("/run/systemd/system"), 0755); err != nil {
		t.Fatal(err)
	}
	return i
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func contains(t *testing.T, s string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(s, p) {
			t.Errorf("missing %q in:\n%s", p, s)
		}
	}
}

func TestRenderUnitsAndConfigs(t *testing.T) {
	i := testInstaller(t)
	if err := i.renderConfigs("https://hub.example.com/grafana/", "test-password"); err != nil {
		t.Fatal(err)
	}
	expected := map[string][]string{
		"collector":       {"--config="},
		"victoriametrics": {"-httpListenAddr=127.0.0.1:8428", "-retentionPeriod=15d", "-storage.minFreeDiskSpaceBytes=2GB"},
		"victorialogs":    {"-httpListenAddr=127.0.0.1:9428", "-retentionPeriod=7d", "-retention.maxDiskSpaceUsageBytes=4GiB"},
		"tempo":           {"-config.expand-env=true", "-target=all"},
		"grafana":         {"server --homepath=", "--config="},
	}
	for _, c := range Components {
		unit := read(t, i.path(UnitDir, unitName(c.Name)))
		contains(t, unit, "User=elasticclaw-obs", "Group=elasticclaw-obs", "MemoryMax="+c.Memory, "NoNewPrivileges=yes", "ProtectSystem=full", "PrivateTmp=yes", "Restart=on-failure", "StartLimitBurst=5", i.path(BinaryDir, c.Name, "current", c.Binary))
		contains(t, unit, expected[c.Name]...)
		info, err := os.Stat(i.path(ConfigDir, c.Name+".env"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Errorf("env mode %v", info.Mode())
		}
	}
	contains(t, read(t, i.path(ConfigDir, "collector.env")), "HOST_ROOT=/\n", "127.0.0.1:4317", "127.0.0.1:4318", "127.0.0.1:4417", "127.0.0.1:13133", "/opentelemetry", "/insert/opentelemetry")
	contains(t, read(t, i.path(ConfigDir, "tempo.env")), "TEMPO_LISTEN_ADDRESS=127.0.0.1", "TEMPO_ADVERTISE_ADDRESS=127.0.0.1", "TEMPO_OTLP_ENDPOINT=127.0.0.1:4417")
	contains(t, read(t, i.path(ConfigDir, "grafana.ini")), "http_addr = 127.0.0.1", "http_port = 3001", "root_url = https://hub.example.com/grafana/", "serve_from_sub_path = true", "allow_sign_up = false", "[auth.anonymous]\nenabled = false", "reporting_enabled = false", "check_for_updates = false", "check_for_plugin_updates = false", "[plugins]\npreinstall_disabled = true")
	for _, name := range []string{"collector.yaml", "tempo.yaml", "grafana/provisioning/datasources/datasources.yaml"} {
		got := read(t, i.path(ConfigDir, name))
		canonical, _ := stack.Files.ReadFile(name)
		if got != string(canonical) {
			t.Errorf("config %s differs from embedded source", name)
		}
		var parsed any
		if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
			t.Fatal(err)
		}
	}
	contains(t, read(t, i.path(ConfigDir, "tempo.yaml")), "backend_worker:\n  compaction:\n    block_retention: 72h", "grpc_listen_address: ${TEMPO_LISTEN_ADDRESS}", "bind_addr:", "path: ${TEMPO_DATA_PATH}/live-store/traces", "shutdown_marker_dir: ${TEMPO_DATA_PATH}/live-store/shutdown-marker", "local_work_path: ${TEMPO_DATA_PATH}/work", "usage_report:\n  reporting_enabled: false")
	contains(t, read(t, i.path(ConfigDir, "collector.yaml")), "host_metrics:", "memory_limiter:", "batch:", "extensions: [health_check]", "level: none")
	contains(t, read(t, i.path(ConfigDir, "grafana/provisioning/datasources/datasources.yaml")), "uid: victoriametrics", "uid: victorialogs", "uid: tempo", "$${__trace.traceId}")
}

func TestGrafanaURL(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"", "http://localhost:3001/grafana/"}, {"https://hub.example.com/", "https://hub.example.com/grafana/"}} {
		got, err := GrafanaURL(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("%q: %q %v", tc.input, got, err)
		}
	}
	for _, bad := range []string{"localhost:8080", "https://user:secret@hub/", "https://hub/?token=secret", "https://hub/\n[security]", "https://hub/#fragment"} {
		if _, err := GrafanaURL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestPasswordIdempotent(t *testing.T) {
	i := testInstaller(t)
	first, created, err := i.ensurePassword()
	if err != nil || !created || len(first) != 64 {
		t.Fatalf("first password: created %v, %v", created, err)
	}
	second, created, err := i.ensurePassword()
	if err != nil || created || second != first {
		t.Fatalf("password changed on reinstall: %v", err)
	}
	info, err := os.Stat(i.path(PasswordFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("password mode %v", info.Mode())
	}
}

func archive(t *testing.T, names ...string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		data := []byte("binary")
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(data))}); err != nil {
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
	return b.Bytes()
}

func TestChecksumAndExtraction(t *testing.T) {
	data := archive(t, "binary")
	sum := fmt.Sprintf("%x", sha256.Sum256(data))
	if err := VerifyChecksum(bytes.NewReader(data), sum); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksum(bytes.NewReader(append(data, '!')), sum); err == nil {
		t.Fatal("tampered archive accepted")
	}
	dir := t.TempDir()
	if err := ExtractArchive(bytes.NewReader(data), dir); err != nil {
		t.Fatal(err)
	}
	contains(t, read(t, filepath.Join(dir, "binary")), "binary")
	for _, name := range []string{"../escape", "/absolute"} {
		if err := ExtractArchive(bytes.NewReader(archive(t, name)), t.TempDir()); err == nil {
			t.Errorf("unsafe path %s accepted", name)
		}
	}
}

func TestInstallArchiveAndAtomicLink(t *testing.T) {
	for _, name := range []string{"collector", "grafana"} {
		t.Run(name, func(t *testing.T) {
			i := testInstaller(t)
			binary := "binary"
			entry := binary
			if name == "grafana" {
				binary = "bin/grafana"
				entry = "arbitrary-top-level/" + binary
			}
			data := archive(t, entry)
			c := Component{Name: name, Version: "1", URL: "https://example.invalid/%s", Binary: binary, AMD64: fmt.Sprintf("%x", sha256.Sum256(data))}
			downloads := 0
			i.HTTP = clientFunc(func(*http.Request) (*http.Response, error) {
				downloads++
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data))}, nil
			})
			if err := os.MkdirAll(i.path(BinaryDir), 0755); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := i.installComponent(t.Context(), c); err != nil {
					t.Fatal(err)
				}
			}
			if downloads != 1 {
				t.Fatalf("downloads %d", downloads)
			}
			link := i.path(BinaryDir, name, "current")
			contains(t, read(t, filepath.Join(link, binary)), "binary")
			old, err := os.Readlink(link)
			if err != nil {
				t.Fatal(err)
			}
			c.Version = "2"
			c.AMD64 = strings.Repeat("0", 64)
			if err := i.installComponent(t.Context(), c); err == nil {
				t.Fatal("bad checksum installed")
			}
			current, _ := os.Readlink(link)
			if current != old {
				t.Fatal("failed install changed current")
			}
			c.AMD64 = fmt.Sprintf("%x", sha256.Sum256(data))
			if err := i.installComponent(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			current, _ = os.Readlink(link)
			if current == old {
				t.Fatal("upgrade did not change current")
			}
		})
	}
}

func TestCaddyRegeneration(t *testing.T) {
	content := "# before\n" + Caddyfile("hub.example.com", false) + "# after\n"
	installed, managed, err := UpdateCaddyfile(content, true)
	if err != nil || !managed {
		t.Fatal(err)
	}
	contains(t, installed, "# before", GrafanaRoute, "# after")
	if strings.Index(installed, GrafanaRoute) > strings.Index(installed, "reverse_proxy localhost:8080") {
		t.Fatal("Grafana route must precede hub")
	}
	repeated, _, err := UpdateCaddyfile(installed, true)
	if err != nil || repeated != installed {
		t.Fatal("Caddy install not idempotent")
	}
	removed, _, err := UpdateCaddyfile(installed, false)
	if err != nil || removed != content {
		t.Fatal("Caddy uninstall changed other content")
	}
	for _, unmanaged := range []string{"hub.example.com {\n reverse_proxy localhost:8080\n}\n", CaddyManagedStart + "\npartial"} {
		got, managed, err := UpdateCaddyfile(unmanaged, true)
		if err != nil || managed || got != unmanaged {
			t.Fatal("unmanaged Caddyfile touched")
		}
	}
}

func TestPreflightRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Installer)
	}{
		{"os", func(i *Installer) { i.OS = "darwin" }}, {"root", func(i *Installer) { i.UID = 1000 }}, {"arch", func(i *Installer) { i.Arch = "386" }}, {"systemd", func(i *Installer) { os.RemoveAll(i.path("/run/systemd/system")) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := testInstaller(t)
			tc.mutate(i)
			if err := i.Preflight(t.Context(), true); err == nil {
				t.Fatal("preflight accepted unsupported host even with force")
			}
		})
	}
	for _, r := range []Resources{{1 << 30, 20 << 30}, {2 << 30, 11 << 30}} {
		if err := CheckResources(r, false); err == nil {
			t.Fatal("accepted insufficient resources")
		}
		if err := CheckResources(r, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckResources(Resources{1536 << 20, 12 << 30}, false); err != nil {
		t.Fatal(err)
	}
}

func TestPortsFreeOrOwned(t *testing.T) {
	for _, tc := range []struct {
		line, pid string
		ok        bool
	}{
		{"", "", true},
		{`tcp LISTEN 0 4096 127.0.0.1:4318 0.0.0.0:* users:(("otelcol",pid=123,fd=8))`, "123\n", true},
		{`tcp LISTEN 0 4096 127.0.0.1:4318 0.0.0.0:* users:(("other",pid=124,fd=8))`, "123\n", false},
		{`tcp LISTEN 0 4096 0.0.0.0:4318 0.0.0.0:* users:(("otelcol",pid=123,fd=8))`, "123\n", false},
		{`tcp LISTEN 0 4096 127.0.0.1:4318 0.0.0.0:*`, "123\n", false},
	} {
		i := testInstaller(t)
		i.Runner = runnerFunc(func(_ context.Context, name string, _ ...string) (string, error) {
			if name == "ss" {
				return tc.line, nil
			}
			return tc.pid, nil
		})
		err := i.checkPorts(t.Context())
		if (err == nil) != tc.ok {
			t.Errorf("listener %q: %v", tc.line, err)
		}
	}
}

func TestHealthBoundedAndStatus(t *testing.T) {
	i := testInstaller(t)
	if err := i.waitHealthy(t.Context()); err != nil {
		t.Fatal(err)
	}
	i.HTTP = clientFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("starting"))}, nil
	})
	i.HealthTimeout = 10 * time.Millisecond
	if err := i.waitHealthy(t.Context()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("health timeout: %v", err)
	}
}

func TestUninstallPreservesData(t *testing.T) {
	for _, purge := range []bool{false, true} {
		i := testInstaller(t)
		if err := i.renderConfigs("http://localhost:3001/grafana/", "secret"); err != nil {
			t.Fatal(err)
		}
		path := i.path(DataDir, "tempo", "blocks", "block")
		if err := i.write(path, []byte("trace"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := i.Uninstall(t.Context(), purge); err != nil {
			t.Fatal(err)
		}
		_, err := os.Stat(path)
		if purge != os.IsNotExist(err) {
			t.Errorf("purge=%v, data stat %v", purge, err)
		}
		if _, err := os.Stat(i.path(ConfigDir)); !os.IsNotExist(err) {
			t.Fatal("configs retained")
		}
		var out bytes.Buffer
		i.Out = &out
		if err := i.Status(t.Context()); err != nil {
			t.Fatal(err)
		}
		contains(t, out.String(), "Observability stack is not installed")
		if !purge {
			contains(t, out.String(), "Retained data: ")
		}
	}
}

func TestPasswordSurvivesRetainedData(t *testing.T) {
	i := testInstaller(t)
	first, _, err := i.ensurePassword()
	if err != nil {
		t.Fatal(err)
	}
	if err := i.Uninstall(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	second, created, err := i.ensurePassword()
	if err != nil || created || first != second {
		t.Fatalf("retained password changed: created=%v err=%v", created, err)
	}
	info, err := os.Stat(i.path(DataDir, ".credentials"))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("recovery directory mode: %v %v", info, err)
	}
	if err := i.Uninstall(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	third, created, err := i.ensurePassword()
	if err != nil || !created || third == first {
		t.Fatal("purged reinstall must generate new password")
	}
}

func TestInstallLifecycle(t *testing.T) {
	i := testInstaller(t)
	if err := i.write(i.path("/proc/meminfo"), []byte("MemAvailable: 2097152 kB\n"), 0644); err != nil {
		t.Fatal(err)
	}
	original := Components
	Components = append([]Component(nil), Components...)
	t.Cleanup(func() { Components = original })
	archives := map[string][]byte{}
	for index, c := range Components {
		entry := c.Binary
		if c.Name == "grafana" {
			entry = "grafana-any-version/" + entry
		}
		data := archive(t, entry)
		Components[index].AMD64 = fmt.Sprintf("%x", sha256.Sum256(data))
		archives[fmt.Sprintf(c.URL, "amd64")] = data
	}
	downloads, pluginInstalls, hubRestarts := 0, 0, 0
	var commands []string
	i.HTTP = clientFunc(func(r *http.Request) (*http.Response, error) {
		data := []byte("healthy")
		if strings.HasPrefix(r.URL.String(), "https://") {
			var ok bool
			data, ok = archives[r.URL.String()]
			if !ok {
				t.Fatalf("unexpected download %s", r.URL)
			}
			downloads++
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})
	i.Runner = runnerFunc(func(_ context.Context, name string, args ...string) (string, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if name == "id" {
			return "", fmt.Errorf("missing user")
		}
		if name == "systemctl" && len(args) == 2 && args[0] == "restart" && args[1] == "elasticclaw" {
			hubRestarts++
		}
		if strings.HasSuffix(name, "bin/grafana") {
			pluginInstalls++
			for index, arg := range args {
				if arg == "--pluginsDir" {
					if err := i.write(filepath.Join(args[index+1], logsPlugin, "plugin.json"), []byte(`{"info":{"version":"`+LogsPluginVersion+`"}}`), 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		return "", nil
	})
	// A restrictive root umask must not leave paths unreadable for the service user.
	restore := setUmask(0o077)
	for range 2 {
		if err := i.Install(t.Context(), Options{Force: true, PublicURL: "https://hub.example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	restore()
	for _, path := range []string{DataDir, BinaryDir, ConfigDir, BinaryDir + "/collector-0.164.0/otelcol-contrib", ConfigDir + "/collector.yaml", ConfigDir + "/grafana/provisioning/datasources"} {
		info, err := os.Stat(i.path(path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0004 == 0 {
			t.Errorf("%s mode %v is not world-readable", path, info.Mode().Perm())
		}
		if info.IsDir() && info.Mode().Perm()&0001 == 0 {
			t.Errorf("%s mode %v is not traversable", path, info.Mode().Perm())
		}
	}
	if downloads != len(Components) || pluginInstalls != 1 || hubRestarts != 0 {
		t.Fatalf("downloads=%d plugin installs=%d hub restarts=%d", downloads, pluginInstalls, hubRestarts)
	}
	all := strings.Join(commands, "\n")
	contains(t, all, "useradd --system --user-group --no-create-home --shell /usr/sbin/nologin elasticclaw-obs", "systemctl daemon-reload")
	for _, c := range Components {
		contains(t, all, "systemctl enable --now "+unitName(c.Name), "systemctl reset-failed "+unitName(c.Name), "systemctl restart "+unitName(c.Name))
	}
	contains(t, read(t, i.path(MarkerFile)), "https://hub.example.com/grafana/")
	var out bytes.Buffer
	i.Out = &out
	if err := i.Status(t.Context()); err != nil {
		t.Fatal(err)
	}
	contains(t, out.String(), "healthy", "version v1.155.0", "Grafana: https://hub.example.com/grafana/")
}

func TestCaddyValidationRollback(t *testing.T) {
	i := testInstaller(t)
	original := Caddyfile("hub.example.com", false)
	if err := i.write(i.path(CaddyPath), []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	i.Runner = runnerFunc(func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "caddy" {
			return "", fmt.Errorf("invalid config")
		}
		return "", nil
	})
	if err := i.UpdateCaddy(t.Context(), true); err == nil {
		t.Fatal("validation failure ignored")
	}
	if got := read(t, i.path(CaddyPath)); got != original {
		t.Fatal("Caddy config not restored")
	}
	// A managed Caddyfile without the caddy binary only gets a manual hint.
	var out bytes.Buffer
	i.Out = &out
	i.Runner = runnerFunc(func(_ context.Context, name string, _ ...string) (string, error) {
		if name == "caddy" {
			return "", fmt.Errorf("caddy validate: %w", exec.ErrNotFound)
		}
		return "", nil
	})
	if err := i.UpdateCaddy(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if got := read(t, i.path(CaddyPath)); got != original {
		t.Fatal("Caddy config changed without caddy installed")
	}
	contains(t, out.String(), "Caddy is not installed", GrafanaRoute)
}

func TestPinnedComponents(t *testing.T) {
	for _, c := range Components {
		if len(c.AMD64) != 64 || len(c.ARM64) != 64 {
			t.Errorf("invalid checksum length for %s", c.Name)
		}
		if !strings.HasPrefix(c.URL, "https://") {
			t.Errorf("non-TLS download for %s", c.Name)
		}
	}
}
