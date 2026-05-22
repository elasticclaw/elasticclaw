package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
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
	if h, _, err := config.ResolveHub(profile); err == nil {
		return runChatHub(h, target, args[1:])
	}
	// Fallback: direct provider chat
	return runChatDirect(target, args[1:])
}

// ─── Hub chat ─────────────────────────────────────────────────────────────────

func runChatHub(h *types.HubProfile, clawID string, rest []string) error {
	client := hub.NewClient(h.URL, h.Token)
	ctx := context.Background()

	// Resolve by name if needed (list and find)
	claws, err := client.ListClaws(ctx)
	if err != nil {
		return fmt.Errorf("hub error: %w", err)
	}
	resolvedID := clawID
	for _, c := range claws {
		if c.Name == clawID || c.ID == clawID || strings.HasPrefix(c.ID, clawID) {
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

	// Connect WebSocket for streaming and reply delivery
	wsCtx, wsCancel := context.WithCancel(ctx)
	defer wsCancel()
	replyCh := make(chan struct{}, 4)

	// clearOnce is reset per-send; callbacks and send coordinate through spinnerMu.
	var spinnerMu sync.Mutex
	var spinnerDone chan struct{}
	var clearOnce *sync.Once
	clearSpinner := func() bool {
		spinnerMu.Lock()
		done := spinnerDone
		once := clearOnce
		spinnerMu.Unlock()

		cleared := false
		if done != nil && once != nil {
			once.Do(func() {
				close(done)
				fmt.Print("\r\033[K")
				cleared = true
			})
		}
		return cleared
	}

	go func() {
		_ = client.WatchStream(wsCtx, resolvedID,
			func(chunk string) {
				// Synchronously erase spinner and print prefix on first chunk
				if clearSpinner() {
					fmt.Print("Claw: ")
				}
				fmt.Print(chunk)
			},
			func(msg types.HubMessage) {
				// Final message: ensure newline after streamed text
				fmt.Println()
				select {
				case replyCh <- struct{}{}:
				default:
				}
			},
		)
	}()

	send := func(content string) error {
		if _, err := client.SendMessage(ctx, resolvedID, content); err != nil {
			return fmt.Errorf("send error: %w", err)
		}

		// Reset per-send state
		spinnerMu.Lock()
		spinnerDone = make(chan struct{})
		clearOnce = &sync.Once{}
		done := spinnerDone
		spinnerMu.Unlock()

		// Spinner animates until closed (by the chunk callback)
		go func(done chan struct{}) {
			frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
			i := 0
			for {
				select {
				case <-done:
					return
				case <-time.After(80 * time.Millisecond):
					fmt.Printf("\r%s thinking...", frames[i%len(frames)])
					i++
				}
			}
		}(done)

		// Wait up to 90s for reply
		select {
		case <-replyCh:
			clearSpinner()
		case <-time.After(90 * time.Second):
			clearSpinner()
			fmt.Println("(no reply received within 90s)")
		}
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
