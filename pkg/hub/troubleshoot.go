package hub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
)

var Commit = "none"

type troubleshootRequest struct {
	Problem        string `json:"problem"`
	TimeRange      string `json:"time_range"`
	StillHappening *bool  `json:"still_happening,omitempty"`
}

func (s *Server) handleTroubleshootStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req troubleshootRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if req.Problem == "" {
		http.Error(w, "problem required", http.StatusBadRequest)
		return
	}
	if req.TimeRange == "" {
		req.TimeRange = "1hour"
	}

	logs := gatherJournalLogs(req.TimeRange)

	var sanitizedCfg string
	if diskCfg, err := config.LoadHubConfig(); err == nil {
		sanitizedCfg, _ = sanitizeHubConfig(diskCfg)
	}

	sources := fetchGitHubSources(Commit)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	sendEvent := func(v interface{}) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	s.mu.RLock()
	llmKeys := s.hubCfg.LLMKeys
	defaultModel := s.hubCfg.DefaultModel
	s.mu.RUnlock()

	systemPrompt := buildTroubleshootPrompt(sanitizedCfg, logs, sources)
	msgs := []aiChatMessage{{Role: "user", Content: buildTroubleshootUserMsg(req)}}

	if err := streamLLMWithSystemPrompt(r.Context(), systemPrompt, msgs, llmKeys, defaultModel, func(token string) {
		sendEvent(map[string]string{"type": "token", "content": token})
	}); err != nil {
		sendEvent(map[string]string{"type": "error", "content": err.Error()})
		return
	}

	sendEvent(map[string]string{"type": "done"})
}

func gatherJournalLogs(timeRange string) string {
	since := timeRangeToSince(timeRange)
	var parts []string

	statusOut, err := exec.Command("systemctl", "status", "elasticclaw", "--no-pager", "-l").CombinedOutput()
	if err == nil {
		parts = append(parts, "=== systemctl status elasticclaw ===\n"+string(statusOut))
	}

	logOut, err := exec.Command("journalctl", "-u", "elasticclaw", "--since", since, "--no-pager", "-n", "500", "-o", "short-iso").CombinedOutput()
	if err != nil {
		logOut2, err2 := exec.Command("journalctl", "--since", since, "--no-pager", "-n", "200", "-o", "short-iso", "--grep", "elasticclaw").CombinedOutput()
		if err2 != nil {
			parts = append(parts, "(journalctl unavailable on this system)")
		} else {
			parts = append(parts, "=== journalctl (grep elasticclaw) ===\n"+string(logOut2))
		}
	} else {
		parts = append(parts, "=== journalctl -u elasticclaw ===\n"+string(logOut))
	}

	return strings.Join(parts, "\n\n")
}

func timeRangeToSince(r string) string {
	switch r {
	case "just-now":
		return "5 minutes ago"
	case "15min":
		return "15 minutes ago"
	case "4hours":
		return "4 hours ago"
	case "today":
		return "today"
	default:
		return "1 hour ago"
	}
}

var troubleshootSourceFiles = []string{
	"pkg/hub/bootstrap.go",
	"pkg/hub/factory_triggers.go",
	"pkg/hub/factory_trigger.go",
	"pkg/hub/github_issues.go",
}

type githubContentsResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

func fetchGitHubSources(commit string) map[string]string {
	ref := commit
	if ref == "" || ref == "none" || ref == "dev" {
		ref = "main"
	}

	client := &http.Client{Timeout: 10 * time.Second}
	result := make(map[string]string, len(troubleshootSourceFiles))

	for _, path := range troubleshootSourceFiles {
		url := fmt.Sprintf("https://api.github.com/repos/elasticclaw/elasticclaw/contents/%s?ref=%s", path, ref)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			result[path] = fmt.Sprintf("(request error: %s)", err)
			continue
		}
		req.Header.Set("Accept", "application/vnd.github.v3+json")

		resp, err := client.Do(req)
		if err != nil {
			result[path] = fmt.Sprintf("(fetch error: %s)", err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			result[path] = fmt.Sprintf("(unavailable: HTTP %d)", resp.StatusCode)
			continue
		}

		var ghResp githubContentsResponse
		if err := json.Unmarshal(body, &ghResp); err != nil || ghResp.Encoding != "base64" {
			result[path] = "(parse error)"
			continue
		}

		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(ghResp.Content, "\n", ""))
		if err != nil {
			result[path] = "(decode error)"
			continue
		}

		content := string(decoded)
		if len(content) > 12000 {
			content = content[:12000] + "\n... (truncated)"
		}
		result[path] = content
	}
	return result
}

func buildTroubleshootPrompt(sanitizedCfg, logs string, sources map[string]string) string {
	var sb strings.Builder

	sb.WriteString("You are a diagnostic assistant for ElasticClaw, an AI coding agent platform.\n\n")
	sb.WriteString(fmt.Sprintf("Running version: %s (commit: %s)\n\n", Version, Commit))

	if sanitizedCfg != "" {
		sb.WriteString("Current hub.yaml (secrets masked as ***):\n---\n")
		sb.WriteString(sanitizedCfg)
		sb.WriteString("---\n\n")
	}

	sb.WriteString("System logs:\n```\n")
	if logs == "" {
		sb.WriteString("(no logs available)\n")
	} else {
		sb.WriteString(logs)
	}
	sb.WriteString("```\n\n")

	if len(sources) > 0 {
		sb.WriteString("Relevant source code at this commit:\n\n")
		for _, path := range troubleshootSourceFiles {
			content, ok := sources[path]
			if !ok {
				continue
			}
			sb.WriteString(fmt.Sprintf("// %s\n```go\n%s\n```\n\n", path, content))
		}
	}

	sb.WriteString("Your job: analyze the logs, config, and source code to diagnose the user's reported problem. ")
	sb.WriteString("Be specific — cite log lines, config fields, or code paths when relevant. ")
	sb.WriteString("Provide clear, actionable next steps.")

	return sb.String()
}

func buildTroubleshootUserMsg(req troubleshootRequest) string {
	var sb strings.Builder
	sb.WriteString(req.Problem)
	if req.StillHappening != nil {
		if *req.StillHappening {
			sb.WriteString("\n\nThis issue is still happening right now.")
		} else {
			sb.WriteString("\n\nThis issue has since resolved itself.")
		}
	}
	return sb.String()
}
