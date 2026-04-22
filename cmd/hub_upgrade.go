package cmd

import (
	"fmt"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/spf13/cobra"
)

var hubUpgradeServer string

var hubUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade the hub binary on a remote server",
	Long: `Upgrade the elasticclaw hub on a remote server via SSH.

Examples:
  elasticclaw hub upgrade --server ssh://root@elasticclaw.example.com
`,
	RunE: runHubUpgrade,
}

func init() {
	hubCmd.AddCommand(hubUpgradeCmd)
	hubUpgradeCmd.Flags().StringVar(&hubUpgradeServer, "server", "", "SSH target, e.g. ssh://root@host (required)")
}

func runHubUpgrade(cmd *cobra.Command, args []string) error {
	if hubUpgradeServer == "" {
		// Try to infer from --profile (or active profile)
		hubUpgradeServer = inferSSHFromProfile(profile)
	}
	if hubUpgradeServer == "" {
		return fmt.Errorf("--server required, e.g. --server ssh://root@elasticclaw.example.com")
	}

	user, host, err := parseSSHHost(hubUpgradeServer)
	if err != nil {
		return err
	}

	fmt.Printf("Connecting to %s@%s...\n", user, host)

	client, err := dialSSH(user, host, "")
	if err != nil {
		return fmt.Errorf("SSH connection failed: %w", err)
	}
	defer client.Close()

	// Check current version on server
	fmt.Println("Checking remote version...")
	remoteVerOut, err := sshRunClient(client, "elasticclaw version 2>/dev/null || echo 'unknown'")
	if err != nil {
		return fmt.Errorf("failed to check remote version: %w", err)
	}
	remoteVer := parseVersionFromOutput(strings.TrimSpace(remoteVerOut))

	// Check latest release
	latest, err := latestGitHubRelease("elasticclaw", "elasticclaw")
	if err != nil {
		return fmt.Errorf("failed to fetch latest version: %w", err)
	}

	fmt.Printf("Remote: %s  →  Latest: %s\n", remoteVer, latest)

	if remoteVer == latest {
		fmt.Println("Already up to date.")
		return nil
	}

	// Build download URL for linux/amd64 (server is always linux)
	downloadURL := fmt.Sprintf(
		"https://github.com/elasticclaw/elasticclaw/releases/download/%s/elasticclaw-linux-amd64",
		latest,
	)

	script := fmt.Sprintf(`set -euo pipefail
echo "Downloading %s..."
curl -fsSL %q -o /tmp/elasticclaw-new
chmod +x /tmp/elasticclaw-new
SELF=$(which elasticclaw || echo /usr/local/bin/elasticclaw)
mv /tmp/elasticclaw-new "$SELF"
echo "Upgraded to $(elasticclaw version 2>/dev/null)"
if systemctl is-active --quiet elasticclaw 2>/dev/null; then
  echo "Restarting hub service..."
  systemctl restart elasticclaw
  echo "Hub service restarted."
fi
`, latest, downloadURL)

	fmt.Printf("Upgrading remote hub to %s...\n", latest)
	out, err := sshRunClient(client, script)
	if out != "" {
		fmt.Print(out)
	}
	if err != nil {
		return fmt.Errorf("upgrade failed: %w", err)
	}

	fmt.Printf("✓ Hub upgraded to %s\n", latest)
	return nil
}

// inferSSHFromProfile tries to guess the SSH target from the given profile's hub URL.
// Pass "" to use the active profile.
func inferSSHFromProfile(profileName string) string {
	hubProfile, _, err := config.ResolveHub(profileName)
	if err != nil || hubProfile == nil || hubProfile.URL == "" {
		return ""
	}
	// Strip scheme and port to get hostname
	host := hubProfile.URL
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	host = strings.SplitN(host, "/", 2)[0]
	host = strings.SplitN(host, ":", 2)[0]
	if host == "" || host == "localhost" || host == "127.0.0.1" {
		return ""
	}
	return fmt.Sprintf("ssh://root@%s", host)
}

// parseVersionFromOutput extracts the version tag from "elasticclaw vX.Y.Z ..." output.
func parseVersionFromOutput(s string) string {
	// Handle "Using config file: ..." prefix lines
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "elasticclaw v") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[1]
			}
		}
	}
	return "unknown"
}


