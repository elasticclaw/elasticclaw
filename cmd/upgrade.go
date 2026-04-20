package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade elasticclaw to the latest release",
	RunE:  runUpgrade,
}

func init() {
	rootCmd.AddCommand(upgradeCmd)
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	fmt.Println("Checking for updates...")

	latest, err := fetchLatestVersion()
	if err != nil {
		return fmt.Errorf("failed to check latest version: %w", err)
	}

	current := Version
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
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
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

func fetchLatestVersion() (string, error) {
	resp, err := http.Get("https://api.github.com/repos/elasticclaw/elasticclaw/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("no release found")
	}
	return rel.TagName, nil
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

	// Strip leading 'v' from version for asset name consistency
	ver := strings.TrimPrefix(version, "v")
	_ = ver // asset name uses full tag

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
	if err := exec.Command("systemctl", "restart", "elasticclaw").Run(); err != nil {
		fmt.Printf("  Warning: could not restart service: %v\n", err)
		fmt.Println("  Run: sudo systemctl restart elasticclaw")
		return
	}
	fmt.Println("✓ Hub service restarted")
}
