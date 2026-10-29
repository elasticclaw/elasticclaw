package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	CaddyPath         = "/etc/caddy/Caddyfile"
	CaddyManagedStart = "# BEGIN ElasticClaw managed Caddy config"
	CaddyManagedEnd   = "# END ElasticClaw managed Caddy config"
	GrafanaRoute      = "reverse_proxy /grafana* localhost:3001"
)

func Caddyfile(domain string, installed bool) string {
	route := ""
	if installed {
		route = "\t" + GrafanaRoute + "\n"
	}
	return fmt.Sprintf("%s\n%s {\n%s\treverse_proxy localhost:8080\n}\n%s\n", CaddyManagedStart, domain, route, CaddyManagedEnd)
}

// UpdateCaddyfile changes only the marked block, preserving other sites and directives.
func UpdateCaddyfile(content string, installed bool) (string, bool, error) {
	start, end := strings.Index(content, CaddyManagedStart), strings.Index(content, CaddyManagedEnd)
	if start < 0 || end < start {
		return content, false, nil
	}
	if strings.Count(content, CaddyManagedStart) != 1 || strings.Count(content, CaddyManagedEnd) != 1 {
		return content, true, fmt.Errorf("ambiguous managed Caddy markers")
	}
	block := content[start:end]
	lines := strings.Split(block, "\n")
	var out []string
	inserted := false
	for _, line := range lines {
		if strings.TrimSpace(line) == GrafanaRoute {
			continue
		}
		if strings.TrimSpace(line) == "reverse_proxy localhost:8080" && installed {
			out = append(out, "\t"+GrafanaRoute)
			inserted = true
		}
		out = append(out, line)
	}
	if installed && !inserted {
		return content, true, fmt.Errorf("managed Caddy block has no recognized hub proxy; add %q manually", GrafanaRoute)
	}
	return content[:start] + strings.Join(out, "\n") + content[end:], true, nil
}

func (i *Installer) UpdateCaddy(ctx context.Context, installed bool) error {
	path := i.path(CaddyPath)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	updated, managed, err := UpdateCaddyfile(string(data), installed)
	if err != nil {
		return err
	}
	if !managed {
		i.caddyHint("Caddyfile is not ElasticClaw-managed", installed)
		return nil
	}
	if updated == string(data) {
		return nil
	}
	if err := i.write(path, []byte(updated), 0644); err != nil {
		return err
	}
	if _, err := i.Runner.Run(ctx, "caddy", "validate", "--config", path); err != nil {
		_ = i.write(path, data, 0644)
		if errors.Is(err, exec.ErrNotFound) {
			i.caddyHint("Caddy is not installed, so the Caddyfile was left unchanged", installed)
			return nil
		}
		return fmt.Errorf("Caddy validation failed (original file restored): %w", err)
	}
	if _, err := i.Runner.Run(ctx, "systemctl", "reload", "caddy"); err != nil {
		_ = i.write(path, data, 0644)
		return fmt.Errorf("Caddy reload failed (original file restored): %w", err)
	}
	return nil
}

func (i *Installer) caddyHint(reason string, installed bool) {
	if installed {
		fmt.Fprintf(i.Out, "%s. Add this inside the hub site, ahead of the hub proxy:\n  %s\n", reason, GrafanaRoute)
	} else {
		fmt.Fprintf(i.Out, "%s; remove any Grafana route manually.\n", reason)
	}
}
