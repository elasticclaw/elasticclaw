package factorytest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestMockGitHubIssuesWriteHandlersReturnNotFoundForMissingIssue(t *testing.T) {
	ghi := NewMockGitHubIssues(t)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			name:   "comment",
			method: http.MethodPost,
			path:   "/repos/testorg/testrepo/issues/42/comments",
			body:   `{"body":"failed"}`,
		},
		{
			name:   "labels",
			method: http.MethodPost,
			path:   "/repos/testorg/testrepo/issues/42/labels",
			body:   `{"labels":["agent-error"]}`,
		},
		{
			name:   "patch",
			method: http.MethodPatch,
			path:   "/repos/testorg/testrepo/issues/42",
			body:   `{"assignees":["alice"]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(tt.method, ghi.URL+tt.path, bytes.NewBufferString(tt.body))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", resp.StatusCode)
			}
		})
	}
}

func TestMockGitHubIssuesAddLabelsReturnsCompleteIssueLabels(t *testing.T) {
	ghi := NewMockGitHubIssues(t)
	ghi.SetIssue("testorg/testrepo", 42, IssueState{
		Title:  "Test issue",
		Body:   "Body",
		State:  "open",
		Labels: []string{"existing"},
	})

	req, err := http.NewRequest(http.MethodPost, ghi.URL+"/repos/testorg/testrepo/issues/42/labels", bytes.NewBufferString(`{"labels":["agent-error"]}`))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var labels []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&labels); err != nil {
		t.Fatalf("decode labels: %v", err)
	}
	got := make(map[string]bool)
	for _, label := range labels {
		got[label.Name] = true
	}
	for _, want := range []string{"existing", "agent-error"} {
		if !got[want] {
			t.Fatalf("labels = %#v, missing %q", labels, want)
		}
	}
}
