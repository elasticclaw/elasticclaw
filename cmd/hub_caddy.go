package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const caddyfilePath = "/etc/caddy/Caddyfile"

var hubCaddyCmd = &cobra.Command{
	Use:   "caddy",
	Short: "Manage Caddy reverse proxy for the hub",
}

var hubCaddyDomain string

var hubCaddyInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Caddy and configure it to proxy to the hub",
	Long: `Install Caddy and write a minimal Caddyfile that reverse proxies
the given domain to the hub on localhost:8080. Caddy handles TLS automatically
via Let's Encrypt.

  sudo elasticclaw hub caddy install --domain hub.example.com

Prerequisites: the domain must resolve to this server's IP.`,
	RunE: runHubCaddyInstall,
}

var hubCaddyUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop and remove Caddy",
	RunE:  runHubCaddyUninstall,
}

func init() {
	hubCmd.AddCommand(hubCaddyCmd)
	hubCaddyCmd.AddCommand(hubCaddyInstallCmd)
	hubCaddyCmd.AddCommand(hubCaddyUninstallCmd)

	hubCaddyInstallCmd.Flags().StringVar(&hubCaddyDomain, "domain", "", "domain name for TLS (e.g. hub.example.com)")
	hubCaddyInstallCmd.MarkFlagRequired("domain")
}

func runHubCaddyInstall(cmd *cobra.Command, args []string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("caddy install is only supported on Linux")
	}

	// Install Caddy if not present
	if _, err := exec.LookPath("caddy"); err != nil {
		fmt.Println("Installing Caddy...")
		script := `apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl 2>/dev/null
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | tee /etc/apt/sources.list.d/caddy-stable.list
apt-get update -qq
apt-get install -y caddy`
		c := exec.Command("bash", "-c", script)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			return fmt.Errorf("failed to install Caddy: %w", err)
		}
	} else {
		fmt.Println("Caddy already installed.")
	}

	// Write Caddyfile
	caddyfile := fmt.Sprintf("%s {\n\treverse_proxy localhost:8080\n}\n", hubCaddyDomain)

	fmt.Printf("Writing %s... ", caddyfilePath)
	if err := os.WriteFile(caddyfilePath, []byte(caddyfile), 0644); err != nil {
		return fmt.Errorf("failed to write Caddyfile (run with sudo?): %w", err)
	}
	fmt.Println("OK")

	// Reload or restart Caddy
	fmt.Print("Starting Caddy... ")
	reload := exec.Command("systemctl", "reload", "caddy")
	if err := reload.Run(); err != nil {
		restart := exec.Command("systemctl", "restart", "caddy")
		restart.Stdout = os.Stdout
		restart.Stderr = os.Stderr
		if err := restart.Run(); err != nil {
			return fmt.Errorf("failed to start Caddy: %w", err)
		}
	}
	fmt.Println("OK")

	fmt.Println()
	fmt.Printf("✓ Caddy configured for %s\n", hubCaddyDomain)
	fmt.Printf("  TLS will be provisioned automatically by Let's Encrypt\n")
	fmt.Printf("  Web UI: https://%s\n", hubCaddyDomain)
	return nil
}

func runHubCaddyUninstall(cmd *cobra.Command, args []string) error {
	_ = exec.Command("systemctl", "stop", "caddy").Run()
	_ = exec.Command("systemctl", "disable", "caddy").Run()

	// Remove our Caddyfile
	data, err := os.ReadFile(caddyfilePath)
	if err == nil && strings.Contains(string(data), "localhost:8080") {
		fmt.Printf("Removing %s... ", caddyfilePath)
		_ = os.Remove(caddyfilePath)
		fmt.Println("OK")
	}

	fmt.Println("✓ Caddy stopped and disabled")
	return nil
}
