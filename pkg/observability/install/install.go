package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type CommandRunner interface {
	Run(context.Context, string, ...string) (string, error)
}
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

type Installer struct {
	Root, OS, Arch string
	UID            int
	Runner         CommandRunner
	HTTP           HTTPClient
	Out            io.Writer
	HealthTimeout  time.Duration
}

type Options struct {
	Force     bool
	PublicURL string
}

func New(root string, out io.Writer) *Installer {
	return &Installer{Root: root, OS: runtime.GOOS, Arch: runtime.GOARCH, UID: os.Geteuid(), Runner: ExecRunner{}, HTTP: &http.Client{Timeout: 5 * time.Minute}, Out: out, HealthTimeout: 90 * time.Second}
}

func (i *Installer) Install(ctx context.Context, opts Options) error {
	grafanaURL, err := GrafanaURL(opts.PublicURL)
	if err != nil {
		return err
	}
	if err := i.Preflight(ctx, opts.Force); err != nil {
		return err
	}
	// The service user must read binaries and configs regardless of root's umask.
	defer setUmask(0o022)()
	if err := os.MkdirAll(i.path(BinaryDir), 0755); err != nil {
		return err
	}
	for _, c := range Components {
		fmt.Fprintf(i.Out, "Installing %s %s...\n", c.Name, c.Version)
		if err := i.installComponent(ctx, c); err != nil {
			return fmt.Errorf("install %s: %w", c.Name, err)
		}
	}
	if _, err := i.Runner.Run(ctx, "id", "-u", ServiceUser); err != nil {
		if _, err := i.Runner.Run(ctx, "useradd", "--system", "--user-group", "--no-create-home", "--shell", "/usr/sbin/nologin", ServiceUser); err != nil {
			return err
		}
	}
	// The data root stays root-owned but must be traversable by the service user.
	if err := os.MkdirAll(i.path(DataDir), 0755); err != nil {
		return err
	}
	if err := os.Chmod(i.path(DataDir), 0755); err != nil {
		return err
	}
	for _, name := range []string{"victoriametrics", "victorialogs", "tempo", "grafana"} {
		if err := os.MkdirAll(i.path(DataDir, name), 0750); err != nil {
			return err
		}
	}
	password, created, err := i.ensurePassword()
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(i.Out, "Grafana admin password (shown once): %s\n", password)
	}
	if err := i.renderConfigs(grafanaURL, password); err != nil {
		return err
	}
	if err := i.installPlugin(ctx); err != nil {
		return err
	}
	for _, name := range []string{"victoriametrics", "victorialogs", "tempo", "grafana"} {
		if _, err := i.Runner.Run(ctx, "chown", "-R", ServiceUser+":"+ServiceUser, i.path(DataDir, name)); err != nil {
			return err
		}
	}
	if _, err := i.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	for _, c := range Components {
		if _, err := i.Runner.Run(ctx, "systemctl", "enable", "--now", unitName(c.Name)); err != nil {
			return err
		}
		// enable --now does not restart already-running units after an upgrade, and a unit
		// that hit its start limit on an earlier attempt refuses to start until reset.
		if _, err := i.Runner.Run(ctx, "systemctl", "reset-failed", unitName(c.Name)); err != nil {
			return err
		}
		if _, err := i.Runner.Run(ctx, "systemctl", "restart", unitName(c.Name)); err != nil {
			return err
		}
	}
	if err := i.waitHealthy(ctx); err != nil {
		return fmt.Errorf("%w\nInspect logs with: journalctl -u 'elasticclaw-obs-*'\nRemove the stack with: sudo elasticclaw hub observability uninstall", err)
	}
	for _, c := range Components {
		i.pruneVersions(c)
	}
	return i.write(i.path(MarkerFile), []byte(grafanaURL+"\n"), 0644)
}

func (i *Installer) health(ctx context.Context, c Component) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Health, nil)
	if err != nil {
		return err
	}
	resp, err := i.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (i *Installer) waitHealthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, i.HealthTimeout)
	defer cancel()
	for {
		var pending []string
		for _, c := range Components {
			if err := i.health(ctx, c); err != nil {
				pending = append(pending, c.Name+": "+err.Error())
			}
		}
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stack health timeout: %s: %w", strings.Join(pending, "; "), ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

func (i *Installer) Uninstall(ctx context.Context, purge bool) error {
	if err := i.platform(ctx); err != nil {
		return err
	}
	for _, c := range Components {
		path := i.path(UnitDir, unitName(c.Name))
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		// Do not delete binaries or data if a service could still be using them.
		if _, err := i.Runner.Run(ctx, "systemctl", "disable", "--now", unitName(c.Name)); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	if _, err := i.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := i.UpdateCaddy(ctx, false); err != nil {
		return err
	}
	for _, dir := range []string{BinaryDir, ConfigDir} {
		if err := os.RemoveAll(i.path(dir)); err != nil {
			return err
		}
	}
	if purge {
		return os.RemoveAll(i.path(DataDir))
	}
	return nil
}

func (i *Installer) Status(ctx context.Context) error {
	if err := i.platform(ctx); err != nil {
		return err
	}
	if !i.installed() {
		fmt.Fprintln(i.Out, "Observability stack is not installed")
		if size, err := dirSize(i.path(DataDir)); err == nil {
			fmt.Fprintf(i.Out, "Retained data: %s in %s\n", formatMiB(size), i.path(DataDir))
		}
		return nil
	}
	var failures []error
	for _, c := range Components {
		active, err := i.Runner.Run(ctx, "systemctl", "is-active", unitName(c.Name))
		if err != nil {
			failures = append(failures, fmt.Errorf("%s is not active", c.Name))
		}
		health := "healthy"
		if err := i.health(ctx, c); err != nil {
			health = err.Error()
			failures = append(failures, fmt.Errorf("%s: %w", c.Name, err))
		}
		version := "not installed"
		if target, err := os.Readlink(i.path(BinaryDir, c.Name, "current")); err == nil {
			version = strings.TrimPrefix(filepath.Base(target), c.Name+"-")
		}
		size, walkErr := dirSize(i.path(DataDir, c.Name))
		usage := formatMiB(size)
		if walkErr != nil && !os.IsNotExist(walkErr) {
			usage = walkErr.Error()
			failures = append(failures, walkErr)
		}
		fmt.Fprintf(i.Out, "%s: %s; %s; version %s (pinned %s); data %s\n", c.Name, strings.TrimSpace(active), health, version, c.Version, usage)
	}
	if url, err := os.ReadFile(i.path(MarkerFile)); err == nil {
		fmt.Fprintf(i.Out, "Grafana: %s", url)
	}
	return errors.Join(failures...)
}

// installed reports whether any part of the stack is present, including a failed install.
func (i *Installer) installed() bool {
	if _, err := os.Stat(i.path(MarkerFile)); err == nil {
		return true
	}
	for _, c := range Components {
		if _, err := os.Stat(i.path(UnitDir, unitName(c.Name))); err == nil {
			return true
		}
	}
	return false
}

func dirSize(root string) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func formatMiB(size int64) string { return fmt.Sprintf("%.1f MiB", float64(size)/(1<<20)) }
