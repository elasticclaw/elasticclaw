package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
	"github.com/elasticclaw/elasticclaw/pkg/provider/local"
	"github.com/elasticclaw/elasticclaw/pkg/state"
	"github.com/spf13/cobra"
)

var (
	chatNoStream bool
)

var chatCmd = &cobra.Command{
	Use:   "chat <instance> [message]",
	Short: "Send messages to an instance",
	Long: `Send messages to a running instance and stream responses.

One-shot message:
  elasticclaw chat acme-support-01 "what's the status?"

Interactive session:
  elasticclaw chat acme-support-01
  > what's the status?
  Working on it...
  > (Ctrl+D to exit)`,
	Args: cobra.MinimumNArgs(1),
	RunE: runChat,
}

func init() {
	rootCmd.AddCommand(chatCmd)

	chatCmd.Flags().BoolVar(&chatNoStream, "no-stream", false, "wait for complete response instead of streaming")
}

func runChat(cmd *cobra.Command, args []string) error {
	instanceName := args[0]

	// Load instance
	store, err := state.NewStore()
	if err != nil {
		return err
	}

	instance, err := store.Get(instanceName)
	if err != nil {
		return err
	}

	ctx := context.Background()

	// Get provider
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

	// One-shot message
	if len(args) > 1 {
		message := strings.Join(args[1:], " ")
		return sendMessage(ctx, execFn, message)
	}

	// Interactive mode
	fmt.Printf("Chat with %s (Ctrl+D to exit)\n", instanceName)
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("> ")
		message, err := reader.ReadString('\n')
		if err != nil {
			// EOF
			fmt.Println()
			break
		}

		message = strings.TrimSpace(message)
		if message == "" {
			continue
		}

		if err := sendMessage(ctx, execFn, message); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		fmt.Println()
	}

	return nil
}

func sendMessage(ctx context.Context, execFn func(context.Context, []string) (string, error), message string) error {
	// Use openclaw agent --local to run without a gateway
	// Need to source env file for API keys and run from workspace directory
	escapedMessage := strings.ReplaceAll(message, "'", "'\"'\"'")
	
	// Build command that:
	// 1. Sources the env file (API keys)
	// 2. Changes to workspace directory
	// 3. Runs openclaw agent in local mode
	cmd := fmt.Sprintf("cd /home/daytona && source ~/.openclaw/env 2>/dev/null; openclaw agent --local --message '%s'", escapedMessage)
	cmdArgs := []string{"bash", "-c", cmd}

	output, err := execFn(ctx, cmdArgs)
	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	if output != "" {
		fmt.Print(output)
	} else {
		fmt.Println("(no response - check that ANTHROPIC_API_KEY or other LLM key was provided via --env)")
	}
	return nil
}
