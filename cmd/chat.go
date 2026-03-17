package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/elasticclaw/elasticclaw/pkg/provider/daytona"
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
		p := daytona.New(nil)
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
	// Use openclaw chat command inside the instance
	// The message needs to be properly escaped for shell
	escapedMessage := strings.ReplaceAll(message, "'", "'\\''")
	cmdArgs := []string{"openclaw", "chat", "--message", escapedMessage}

	output, err := execFn(ctx, cmdArgs)
	if err != nil {
		// Try alternative: openclaw agent --message
		cmdArgs = []string{"openclaw", "agent", "--message", escapedMessage}
		output, err = execFn(ctx, cmdArgs)
		if err != nil {
			return fmt.Errorf("failed to send message: %w", err)
		}
	}

	fmt.Print(output)
	return nil
}
