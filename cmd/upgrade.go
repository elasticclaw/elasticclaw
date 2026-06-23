package cmd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/install"
	"github.com/spf13/cobra"
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade elasticclaw to the latest release on the current track",
	Long: `Upgrades the elasticclaw binary to the latest release on the same track.

Stable clients upgrade to the latest stable release; prerelease clients
(e.g. beta, rc) upgrade to the latest release on the same prerelease track.
Cross-track jumps (beta → stable) are prevented.`,
	RunE: runUpgrade,
}

func init() {
	rootCmd.AddCommand(upgradeCmd)
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	fmt.Println("Checking for updates...")

	current := Version
	if current == "dev" {
		return fmt.Errorf("cannot upgrade a dev build — download a release from https://github.com/elasticclaw/elasticclaw/releases")
	}

	// Find the latest release on the same track (stable→stable, beta→beta, etc.)
	latest, err := latestReleaseOnTrack("elasticclaw", "elasticclaw", current)
	if err != nil {
		return fmt.Errorf("no releases found on track %s: %w", extractTrack(current), err)
	}

	if current == latest {
		fmt.Printf("Already up to date (%s)\n", current)
		return nil
	}

	fmt.Printf("Upgrading %s → %s\n", current, latest)

	downloadURL, err := buildDownloadURL(latest)
	if err != nil {
		return err
	}

	// Determine current binary path
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot determine current binary path: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return fmt.Errorf("cannot resolve symlink: %w", err)
	}

	fmt.Printf("Downloading %s...\n", downloadURL)

	// Download to a temp file next to the current binary
	dir := filepath.Dir(self)
	tmp, err := os.CreateTemp(dir, ".elasticclaw-upgrade-*")
	if err != nil {
		return fmt.Errorf("cannot create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { os.Remove(tmpPath) }() // clean up on failure

	resp, err := http.Get(downloadURL)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("write failed: %w", err)
	}
	tmp.Close()

	// Make executable
	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("chmod failed: %w", err)
	}

	// Atomic replace
	if err := os.Rename(tmpPath, self); err != nil {
		return fmt.Errorf("failed to replace binary (try sudo): %w", err)
	}

	fmt.Printf("✓ Upgraded to %s\n", latest)

	// Restart hub service if it's running
	restartHub()

	return nil
}

func buildDownloadURL(version string) (string, error) {
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	// Map to release asset names
	var suffix string
	switch {
	case goos == "linux" && goarch == "amd64":
		suffix = "linux-amd64"
	case goos == "linux" && goarch == "arm64":
		suffix = "linux-arm64"
	case goos == "darwin" && goarch == "amd64":
		suffix = "darwin-amd64"
	case goos == "darwin" && goarch == "arm64":
		suffix = "darwin-arm64"
	default:
		return "", fmt.Errorf("unsupported platform: %s/%s — download manually from https://github.com/elasticclaw/elasticclaw/releases", goos, goarch)
	}

	return fmt.Sprintf(
		"https://github.com/elasticclaw/elasticclaw/releases/download/%s/elasticclaw-%s",
		version, suffix,
	), nil
}

// restartHub attempts to restart the hub systemd service if it's running.
// Non-fatal — we just print a message either way.
func restartHub() {
	out, err := exec.Command("systemctl", "is-active", "elasticclaw").Output()
	if err != nil {
		return // systemctl not available or service not found
	}
	if strings.TrimSpace(string(out)) != "active" {
		return
	}
	fmt.Println("Restarting hub service...")
	if err := installNodeNPMForLocalHub(); err != nil {
		fmt.Printf("  Warning: could not install Node.js/npm dependency: %v\n", err)
		fmt.Println("  Grok/Codex model login may fail until npm is installed on this server.")
	}
	if err := exec.Command("systemctl", "restart", "elasticclaw").Run(); err != nil {
		fmt.Printf("  Warning: could not restart service: %v\n", err)
		fmt.Println("  Run: sudo systemctl restart elasticclaw")
		return
	}
	fmt.Println("✓ Hub service restarted")
}

func installNodeNPMForLocalHub() error {
	if runtime.GOOS != "linux" {
		return nil
	}
	if _, err := exec.LookPath("npm"); err == nil {
		return nil
	}
	useSudo := os.Geteuid() != 0
	cmd := exec.Command("bash", "-c", install.ScriptInstallNodeNPM(useSudo))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
