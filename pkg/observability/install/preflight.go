package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type Resources struct{ Memory, Disk uint64 }

func CheckResources(r Resources, force bool) error {
	if force {
		return nil
	}
	if r.Memory < 1536<<20 {
		return fmt.Errorf("need at least 1.5 GiB MemAvailable (have %.2f GiB); use --force to override", float64(r.Memory)/(1<<30))
	}
	if r.Disk < 12<<30 {
		return fmt.Errorf("need at least 12 GiB free on the data filesystem (have %.2f GiB); use --force to override", float64(r.Disk)/(1<<30))
	}
	return nil
}

func (i *Installer) platform(ctx context.Context) error {
	if i.OS != "linux" {
		return fmt.Errorf("observability commands require Linux with systemd")
	}
	if i.UID != 0 {
		return fmt.Errorf("observability commands require root (run with sudo)")
	}
	if i.Arch != "amd64" && i.Arch != "arm64" {
		return fmt.Errorf("unsupported architecture %q; supported: amd64, arm64", i.Arch)
	}
	if _, err := os.Stat(i.path("/run/systemd/system")); err != nil {
		return fmt.Errorf("systemd is not running: %w", err)
	}
	if _, err := i.Runner.Run(ctx, "systemctl", "--version"); err != nil {
		return fmt.Errorf("systemctl is required: %w", err)
	}
	return nil
}

func (i *Installer) Preflight(ctx context.Context, force bool) error {
	if err := i.platform(ctx); err != nil {
		return err
	}
	mem, err := os.ReadFile(i.path("/proc/meminfo"))
	if err != nil {
		return err
	}
	var available uint64
	for _, line := range strings.Split(string(mem), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			available, err = strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return err
			}
			available *= 1024
		}
	}
	path := i.path(DataDir)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return fmt.Errorf("cannot locate data filesystem")
		}
		path = parent
	}
	disk, err := freeDisk(path)
	if err != nil {
		return err
	}
	if err := CheckResources(Resources{available, disk}, force); err != nil {
		return err
	}
	return i.checkPorts(ctx)
}

var listenerPID = regexp.MustCompile(`pid=(\d+)`)

func (i *Installer) checkPorts(ctx context.Context) error {
	// ss inspects listeners without briefly binding or disrupting any service.
	output, err := i.Runner.Run(ctx, "ss", "-H", "-lntup")
	if err != nil {
		return fmt.Errorf("ss is required to check listener ownership: %w", err)
	}
	ports := map[string]string{"3001": "grafana", "3200": "tempo", "4317": "collector", "4318": "collector", "4417": "tempo", "8428": "victoriametrics", "9428": "victorialogs", "13133": "collector", "9095": "tempo", "7946": "tempo"}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		local := fields[4]
		colon := strings.LastIndex(local, ":")
		if colon < 0 {
			continue
		}
		component, needed := ports[local[colon+1:]]
		if !needed {
			continue
		}
		matches := listenerPID.FindAllStringSubmatch(line, -1)
		if len(matches) == 0 || local[:colon] != "127.0.0.1" {
			return fmt.Errorf("port %s is occupied by an unrecognized or non-loopback listener", local[colon+1:])
		}
		pid, err := i.Runner.Run(ctx, "systemctl", "show", "--property=MainPID", "--value", unitName(component))
		if err != nil {
			return fmt.Errorf("port %s is occupied: %w", local[colon+1:], err)
		}
		for _, match := range matches {
			if match[1] != strings.TrimSpace(pid) {
				return fmt.Errorf("port %s is occupied by another process (PID %s)", local[colon+1:], match[1])
			}
		}
	}
	return nil
}
