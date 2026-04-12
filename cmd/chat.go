package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	"github.com/elasticclaw/elasticclaw/pkg/provider/local"
	"github.com/elasticclaw/elasticclaw/pkg/state"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/spf13/cobra"
)

var (
	chatNoStream bool
)

var chatCmd = &cobra.Command{
	Use:   "chat <claw-id-or-name> [message]",
	Short: "Send messages to a claw",
	Long: `Send messages to a running claw.

One-shot:
  elasticclaw chat support-01 "what's the status?"

Interactive:
  elasticclaw chat support-01`,
	Args: cobra.MinimumNArgs(1),
	RunE: runChat,
}

func init() {
	rootCmd.AddCommand(chatCmd)
	chatCmd.Flags().BoolVar(&chatNoStream, "no-stream", false, "wait for complete response instead of streaming")
}

func runChat(cmd *cobra.Command, args []string) error {
	target := args[0]
	cfg, _ := config.LoadGlobalConfig()

	if cfg != nil && cfg.Hub != nil && cfg.Hub.URL != "" {
		return runChatHub(cfg.Hub, target, args[1:])
	}

	// Fallback: direct provider chat
	return runChatDirect(target, args[1:])
}

// ─── Hub chat ─────────────────────────────────────────────────────────────────

func runChatHub(h *types.HubConfig, clawID string, rest []string) error {
	client := hub.NewClient(h.URL, h.Token)
	ctx := context.Background()

	// Resolve by name if needed (list and find)
	claws, err := client.ListClaws(ctx)
	if err != nil {
		return fmt.Errorf("hub error: %w", err)
	}
	resolvedID := clawID
	for _, c := range claws {
		if c.Name == clawID {
			resolvedID = c.ID
			break
		}
	}

	// Print history on interactive open
	if len(rest) == 0 {
		msgs, err := client.GetMessages(ctx, resolvedID)
		if err == nil && len(msgs) > 0 {
			fmt.Println("── Recent history ──────────────────")
			for _, m := range msgs {
				prefix := "You"
				if m.Role == "claw" {
					prefix = "Claw"
				}
				fmt.Printf("[%s] %s: %s\n", m.CreatedAt.Format(time.Kitchen), prefix, m.Content)
			}
			fmt.Println("────────────────────────────────────")
			fmt.Println()
		}
	}

	send := func(content string) error {
		msg, err := client.SendMessage(ctx, resolvedID, content)
		if err != nil {
			return fmt.Errorf("send error: %w", err)
		}
		fmt.Printf("✓ sent (id: %s)\n", msg.ID[:8])
		fmt.Print("Waiting for reply")
		// Poll up to 90s for a claw reply after our sent message
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(2 * time.Second)
			fmt.Print(".")
			msgs, err := client.GetMessages(ctx, resolvedID)
			if err != nil {
				break
			}
			// Find last claw message after our sent message
			for i := len(msgs) - 1; i >= 0; i-- {
				m := msgs[i]
				if m.Role == "claw" && m.CreatedAt.After(msg.CreatedAt) {
					fmt.Printf("\n\nClaw: %s\n", m.Content)
					return nil
				}
			}
		}
		fmt.Println("\n\n(no reply received)")
		return nil
	}

	if len(rest) > 0 {
		return send(strings.Join(rest, " "))
	}

	// Interactive
	fmt.Printf("Chat with %s via hub (Ctrl+D to exit)\n\n", clawID)
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println()
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if err := send(line); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		fmt.Println()
	}
	return nil
}

// ─── Direct provider chat (legacy) ───────────────────────────────────────────

func runChatDirect(instanceName string, rest []string) error {
	store, err := state.NewStore()
	if err != nil {
		return err
	}
	instance, err := store.Get(instanceName)
	if err != nil {
		return err
	}

	ctx := context.Background()
	var execFn func(ctx context.Context, cmdArgs []string) (string, error)

	switch instance.Provider {
	case "daytona":
		p, pErr := daytona.New(nil)
		if pErr != nil {
			return fmt.Errorf("failed to initialize daytona provider: %w", pErr)
		}
		execFn = func(ctx context.Context, cmdArgs []string) (string, error) {
			result, err := p.Exec(ctx, instance.Name, cmdArgs)
			if err != nil {
				return "", err
			}
			return result.Stdout, nil
		}
	case "local":
		p := local.New()
		execFn = func(ctx context.Context, cmdArgs []string) (string, error) {
			result, err := p.Exec(ctx, instance.Name, cmdArgs)
			if err != nil {
				return "", err
			}
			return result.Stdout, nil
		}
	default:
		return fmt.Errorf("chat not supported for provider %s", instance.Provider)
	}

	if len(rest) > 0 {
		return sendDirectMessage(ctx, execFn, strings.Join(rest, " "))
	}

	fmt.Printf("Chat with %s (Ctrl+D to exit)\n\n", instanceName)
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println()
			break
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if err := sendDirectMessage(ctx, execFn, line); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		fmt.Println()
	}
	return nil
}

func sendDirectMessage(ctx context.Context, execFn func(context.Context, []string) (string, error), message string) error {
	escaped := strings.ReplaceAll(message, "'", "'\"'\"'")
	cmd := fmt.Sprintf("cd /home/daytona && source ~/.openclaw/env 2>/dev/null; openclaw agent --local --message '%s'", escaped)
	output, err := execFn(ctx, []string{"bash", "-c", cmd})
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}
	if output != "" {
		fmt.Print(output)
	}
	return nil
}
