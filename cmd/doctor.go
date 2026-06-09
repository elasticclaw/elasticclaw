package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/hub"
	"github.com/spf13/cobra"
)

func DoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "doctor",
		Short:         "Run hub diagnostics",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDoctor(cmd)
		},
	}
	return cmd
}

func runDoctor(cmd *cobra.Command) error {
	hubURL, token, err := resolveHubAdminConn()
	if err != nil {
		return err
	}

	report, err := fetchDoctorReport(cmd.Context(), hubURL, token)
	if err != nil {
		return err
	}

	if jsonOut {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return fmt.Errorf("encode doctor report: %w", err)
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}

	return renderDoctorReport(cmd.OutOrStdout(), report)
}

func resolveHubAdminConn() (hubURL, token string, err error) {
	hubURL = os.Getenv("ELASTICCLAW_HUB_URL")
	token = firstNonEmpty(os.Getenv("ELASTICCLAW_TOKEN"), os.Getenv("ELASTICCLAW_HUB_TOKEN"))

	if hubURL == "" || token == "" {
		h, _, resolveErr := config.ResolveHub(profile)
		if resolveErr == nil && h != nil {
			if hubURL == "" {
				hubURL = h.URL
			}
			if token == "" {
				token = h.Token
			}
		}
	}

	if hubURL == "" {
		return "", "", fmt.Errorf("hub URL not set - use ELASTICCLAW_HUB_URL or configure with `elasticclaw login`")
	}
	if token == "" {
		return "", "", fmt.Errorf("hub admin token not set - use ELASTICCLAW_TOKEN or configure with `elasticclaw login`")
	}
	return hubURL, token, nil
}

func fetchDoctorReport(ctx context.Context, hubURL, token string) (*hub.DoctorResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	endpoint := strings.TrimRight(hubURL, "/") + "/api/doctor?refresh=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create doctor request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("doctor request: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return nil, fmt.Errorf("read doctor response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var report hub.DoctorResponse
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, fmt.Errorf("decode doctor response: %w", err)
	}
	return &report, nil
}

func renderDoctorReport(w io.Writer, report *hub.DoctorResponse) error {
	if report == nil {
		return fmt.Errorf("doctor report is nil")
	}

	if len(report.Checks) == 0 {
		_, err := fmt.Fprintln(w, "No doctor checks returned.")
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "STATUS\tSEVERITY\tCATEGORY\tID\tTITLE"); err != nil {
		return err
	}
	for _, check := range report.Checks {
		status := "ok"
		if !check.OK {
			status = "fail"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", status, check.Severity, check.Category, check.ID, check.Title); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func init() {
	rootCmd.AddCommand(DoctorCmd())
}
