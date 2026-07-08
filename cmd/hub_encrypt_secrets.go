package cmd

import (
	"fmt"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/spf13/cobra"
)

var hubEncryptSecretsCmd = &cobra.Command{
	Use:   "encrypt-secrets",
	Short: "Encrypt all sensitive fields in hub.yaml at rest",
	Long: `Rewrite hub.yaml so every sensitive field (tokens, passwords, private keys,
webhook secrets) is stored as an enc:v1: AES-256-GCM envelope.

The master key is read from the ELASTICCLAW_MASTER_KEY environment variable
(32 bytes, base64) or from the master.key file next to the hub data
(~/.elasticclaw/master.key by default); a new key is generated on first use.

Plaintext secrets are also migrated automatically the next time the hub saves
its config — this command just forces the full migration immediately.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.LoadHubConfig()
		if err != nil {
			return fmt.Errorf("failed to load hub config: %w", err)
		}
		if err := config.SaveHubConfig(cfg); err != nil {
			return fmt.Errorf("failed to save hub config: %w", err)
		}
		path, err := config.ActiveHubConfigPath()
		if err != nil {
			return err
		}
		fmt.Printf("Encrypted secrets in %s\n", path)
		return nil
	},
}

func init() {
	hubCmd.AddCommand(hubEncryptSecretsCmd)
}
