package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestLinearCLI_NoAPIKey verifies the CLI exits with error when LINEAR_API_KEY is missing.
func TestLinearCLI_NoAPIKey(t *testing.T) {
	os.Unsetenv("LINEAR_API_KEY")
	if got := runLinearCLI([]string{"teams"}); got != 1 {
		t.Fatalf("expected exit 1 without LINEAR_API_KEY, got %d", got)
	}
}

// TestLinearCLI_Teams verifies the teams command makes a GraphQL request and prints results.
func TestLinearCLI_Teams(t *testing.T) {
	// Mock Linear API server
	var requestBody map[string]interface{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "test-key" {
			t.Errorf("missing or wrong Authorization header")
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &requestBody)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"data":{"teams":{"nodes":[{"id":"t1","name":"Engineering","key":"ENG"},{"id":"t2","name":"Design","key":"DSN"}]}}}`)
	}))
	defer ts.Close()

	// Override the API URL for this test
	oldURL := linearAPIURL
	defer func() { linearAPIURL = oldURL }()
	linearAPIURL = ts.URL + "/graphql"

	os.Setenv("LINEAR_API_KEY", "test-key")
	defer os.Unsetenv("LINEAR_API_KEY")

	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	got := runLinearCLI([]string{"teams"})

	w.Close()
	os.Stdout = oldStdout
	out, _ := io.ReadAll(r)

	if got != 0 {
		t.Fatalf("expected exit 0, got %d", got)
	}

	output := string(out)
	if !strings.Contains(output, "ENG — Engineering") {
		t.Errorf("expected 'ENG — Engineering' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "DSN — Design") {
		t.Errorf("expected 'DSN — Design' in output, got:\n%s", output)
	}

	query, _ := requestBody["query"].(string)
	if !strings.Contains(query, "teams") {
		t.Errorf("expected teams query, got: %s", query)
	}
}

// TestLinearCLI_IssueGet verifies issue get fetches by identifier.
func TestLinearCLI_IssueGet(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		json.Unmarshal(b, &body)

		vars, _ := body["variables"].(map[string]interface{})
		id, _ := vars["id"].(string)
		if id != "CAN-61" {
			t.Errorf("expected id CAN-61, got %s", id)
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"data":{"issue":{"id":"issue-uuid","identifier":"CAN-61","title":"Fix auth","state":{"name":"In Progress"},"team":{"name":"Engineering","key":"ENG"}}}}`)
	}))
	defer ts.Close()

	oldURL := linearAPIURL
	defer func() { linearAPIURL = oldURL }()
	linearAPIURL = ts.URL + "/graphql"

	os.Setenv("LINEAR_API_KEY", "test-key")
	defer os.Unsetenv("LINEAR_API_KEY")

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	got := runLinearCLI([]string{"issue", "get", "CAN-61"})

	w.Close()
	os.Stdout = oldStdout
	out, _ := io.ReadAll(r)

	if got != 0 {
		t.Fatalf("expected exit 0, got %d", got)
	}

	output := string(out)
	if !strings.Contains(output, "Fix auth") {
		t.Errorf("expected 'Fix auth' in output, got:\n%s", output)
	}
}

// TestLinearCLI_IssueUpdate_MutationSuccessFalse verifies the CLI reports failure when Linear returns success=false.
func TestLinearCLI_IssueUpdate_MutationSuccessFalse(t *testing.T) {
	sawIssueUpdate := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		json.Unmarshal(b, &body)

		query, _ := body["query"].(string)
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(query, "issue(id:") {
			// Issue lookup response
			fmt.Fprintln(w, `{"data":{"issue":{"id":"issue-uuid","team":{"id":"team-uuid","name":"Eng"}}}}`)
		} else if strings.Contains(query, "workflowStates") {
			fmt.Fprintln(w, `{"data":{"workflowStates":{"nodes":[{"id":"state-uuid","name":"Done","team":{"name":"Eng"}}]}}}`)
		} else if strings.Contains(query, "issueUpdate") {
			sawIssueUpdate = true
			// Return success: false (application-level failure)
			fmt.Fprintln(w, `{"data":{"issueUpdate":{"success":false,"issue":null}}}`)
		} else {
			fmt.Fprintln(w, `{"data":{}}`)
		}
	}))
	defer ts.Close()

	oldURL := linearAPIURL
	defer func() { linearAPIURL = oldURL }()
	linearAPIURL = ts.URL + "/graphql"

	os.Setenv("LINEAR_API_KEY", "test-key")
	defer os.Unsetenv("LINEAR_API_KEY")

	got := runLinearCLI([]string{"issue", "update", "CAN-61", "--state=Done"})
	if got != 1 {
		t.Fatalf("expected exit 1 when mutation returns success=false, got %d", got)
	}
	if !sawIssueUpdate {
		t.Fatal("expected test to reach issueUpdate mutation")
	}
}

// TestLinearCLI_IssueSearch_MultiWord verifies multi-word queries are joined.
func TestLinearCLI_IssueSearch_MultiWord(t *testing.T) {
	var capturedQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		json.Unmarshal(b, &body)

		vars, _ := body["variables"].(map[string]interface{})
		capturedQuery, _ = vars["query"].(string)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"data":{"issueSearch":{"nodes":[]}}}`)
	}))
	defer ts.Close()

	oldURL := linearAPIURL
	defer func() { linearAPIURL = oldURL }()
	linearAPIURL = ts.URL + "/graphql"

	os.Setenv("LINEAR_API_KEY", "test-key")
	defer os.Unsetenv("LINEAR_API_KEY")

	got := runLinearCLI([]string{"issue", "search", "fix", "the", "login", "bug"})
	if got != 0 {
		t.Fatalf("expected exit 0, got %d", got)
	}

	if capturedQuery != "fix the login bug" {
		t.Errorf("expected query 'fix the login bug', got %q", capturedQuery)
	}
}
