package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newLinearIssueDetailsServer(t *testing.T, response string, capturedQuery *string) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if capturedQuery != nil {
			*capturedQuery = body.Query
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(srv.Close)
	return &Server{linearBaseURL: srv.URL}
}

func TestFetchLinearIssueDetailsRequestsCreatorEmail(t *testing.T) {
	var query string
	s := newLinearIssueDetailsServer(t, `{"data":{"issue":{"identifier":"CAN-61","title":"Ship it","creator":{"name":"Adrian Costa","email":"adrian@example.com"}}}}`, &query)

	details, err := s.fetchLinearIssueDetails("token", "CAN-61")
	if err != nil {
		t.Fatalf("fetchLinearIssueDetails: %v", err)
	}
	if !strings.Contains(query, "creator { name email }") {
		t.Errorf("query = %q, want it to request creator name and email", query)
	}
	if details.Creator.Name != "Adrian Costa" || details.Creator.Email != "adrian@example.com" {
		t.Errorf("creator = %+v, want name and email populated", details.Creator)
	}
}

func TestFetchLinearIssueDetailsKeepsIssueWhenOnlyFieldsFail(t *testing.T) {
	s := newLinearIssueDetailsServer(t, `{"data":{"issue":{"identifier":"CAN-61","title":"Ship it","priorityLabel":"Urgent","creator":{"name":"Adrian Costa"}}},"errors":[{"message":"You are not authorized to read User.email"}]}`, nil)

	details, err := s.fetchLinearIssueDetails("token", "CAN-61")
	if err != nil {
		t.Fatalf("fetchLinearIssueDetails: %v", err)
	}
	if details.Identifier != "CAN-61" || details.PriorityLabel != "Urgent" || details.Creator.Name != "Adrian Costa" {
		t.Errorf("details = %+v, want the partial issue preserved", details)
	}
	if details.Creator.Email != "" {
		t.Errorf("creator email = %q, want empty", details.Creator.Email)
	}
}

func TestFetchLinearIssueDetailsFailsWhenErrorsLeaveNoIssue(t *testing.T) {
	s := newLinearIssueDetailsServer(t, `{"data":{"issue":null},"errors":[{"message":"Entity not found"}]}`, nil)

	if _, err := s.fetchLinearIssueDetails("token", "CAN-61"); err == nil {
		t.Fatal("fetchLinearIssueDetails succeeded, want an error")
	}
}
