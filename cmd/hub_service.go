package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/spf13/cobra"
)

const systemdUnitPath = "/etc/systemd/system/elasticclaw.service"

var hubServiceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage the elasticclaw hub systemd service",
}

var hubServiceInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install and start the hub as a systemd service",
	Long: `Install the elasticclaw hub as a systemd service.

Must be run as root (or with sudo) on a Linux system with systemd.

  sudo elasticclaw hub service install

This will:
  - Write the systemd unit file to /etc/systemd/system/elasticclaw.service
  - Enable the service to start on boot
  - Start it immediately`,
	RunE: runHubServiceInstall,
}

var hubServiceUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop and remove the hub systemd service",
	RunE:  runHubServiceUninstall,
}

var hubServiceStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the hub service status",
	RunE: func(cmd *cobra.Command, args []string) error {
		return systemctlRun("status", "elasticclaw")
	},
}

func init() {
	hubCmd.AddCommand(hubServiceCmd)
	hubServiceCmd.AddCommand(hubServiceInstallCmd)
	hubServiceCmd.AddCommand(hubServiceUninstallCmd)
	hubServiceCmd.AddCommand(hubServiceStatusCmd)
}

func runHubServiceInstall(cmd *cobra.Command, args []string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("service install is only supported on Linux (systemd)")
	}

	// Resolve the path to the current binary
	binaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine binary path: %w", err)
	}
	binaryPath, err = filepath.EvalSymlinks(binaryPath)
	if err != nil {
		return fmt.Errorf("could not resolve binary path: %w", err)
	}

	// Resolve home dir for WorkingDirectory
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/root"
	}

	unit := fmt.Sprintf(`[Unit]
Description=ElasticClaw Hub
After=network.target

[Service]
Type=simple
Environment=HOME=%s
ExecStart=%s hub
Restart=always
RestartSec=5
WorkingDirectory=%s

[Install]
WantedBy=multi-user.target
`, home, binaryPath, home)

	fmt.Printf("Writing %s... ", systemdUnitPath)
	if err := os.WriteFile(systemdUnitPath, []byte(unit), 0644); err != nil {
		return fmt.Errorf("failed to write unit file (run with sudo?): %w", err)
	}
	fmt.Println("OK")

	for _, args := range [][]string{
		{"daemon-reload"},
		{"enable", "elasticclaw"},
		{"restart", "elasticclaw"},
	} {
		label := "systemctl " + args[0]
		if len(args) > 1 {
			label += " " + args[1]
		}
		fmt.Printf("%s... ", label)
		if err := systemctlRun(args...); err != nil {
			return fmt.Errorf("%s failed: %w", label, err)
		}
		fmt.Println("OK")
	}

	fmt.Println()
	fmt.Println("✓ ElasticClaw hub service installed and running")
	fmt.Println("  systemctl status elasticclaw")
	fmt.Println("  journalctl -u elasticclaw -f")
	return nil
}

func runHubServiceUninstall(cmd *cobra.Command, args []string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("service uninstall is only supported on Linux (systemd)")
	}

	for _, a := range [][]string{
		{"stop", "elasticclaw"},
		{"disable", "elasticclaw"},
	} {
		_ = systemctlRun(a...) // ignore errors (may not be running)
	}

	fmt.Printf("Removing %s... ", systemdUnitPath)
	if err := os.Remove(systemdUnitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove unit file: %w", err)
	}
	fmt.Println("OK")

	_ = systemctlRun("daemon-reload")
	fmt.Println("✓ ElasticClaw hub service removed")
	return nil
}

func systemctlRun(args ...string) error {
	c := exec.Command("systemctl", args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
