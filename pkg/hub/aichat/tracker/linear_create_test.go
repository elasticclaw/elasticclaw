package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestLinearCreateResolution(t *testing.T) {
	for _, team := range []string{"Product", "PRO", "ab12cd34-0000-1111-2222-ab12cd345678"} {
		t.Run(team, func(t *testing.T) {
			sent, marked := 0, false
			l := NewLinear(team, secret, fake(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if strings.Contains(string(raw), secret) {
					t.Fatal("secret in request body")
				}
				var request struct {
					Query     string
					Variables map[string]any
				}
				if err := json.Unmarshal(raw, &request); err != nil {
					t.Fatal(err)
				}
				switch request.Query {
				case createTeamQuery:
					expected, _ := json.Marshal(teamFilter(team))
					got, _ := json.Marshal(request.Variables["filter"])
					if string(got) != string(expected) {
						t.Fatalf("team filter %s", got)
					}
					fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"team-id","name":"Product"}]}}}`)
				case createStatesQuery:
					want := map[string]any{"team": map[string]any{"id": map[string]any{"eq": "team-id"}}}
					if !reflect.DeepEqual(request.Variables["filter"], want) {
						t.Fatal(request.Variables)
					}
					fmt.Fprint(w, `{"data":{"workflowStates":{"nodes":[{"id":"backlog","name":"Backlog"}]}}}`)
				case createLabelsQuery:
					want := map[string]any{"or": []any{map[string]any{"team": map[string]any{"id": map[string]any{"eq": "team-id"}}}, map[string]any{"team": map[string]any{"null": true}}}}
					if !reflect.DeepEqual(request.Variables["filter"], want) {
						t.Fatal(request.Variables)
					}
					if request.Variables["after"] == nil {
						fmt.Fprint(w, `{"data":{"issueLabels":{"nodes":[{"id":"workspace-label","name":"ai-chat"}],"pageInfo":{"hasNextPage":true,"endCursor":"next"}}}}`)
					} else {
						fmt.Fprint(w, `{"data":{"issueLabels":{"nodes":[{"id":"team-label","name":"Product work"}]}}}`)
					}
				case createIssueMutation:
					sent++
					if !marked {
						t.Fatal("mutation before durable marker")
					}
					want := map[string]any{"teamId": "team-id", "title": "Swap [redacted]", "description": "31 requests\n\nArtifact: Prototype — https://hub.test/artifacts/prototype\n\n---\nFrom AI Chat · ana", "stateId": "backlog", "labelIds": []any{"workspace-label", "team-label"}}
					if !reflect.DeepEqual(request.Variables, map[string]any{"input": want}) {
						t.Fatalf("variables: %#v", request.Variables)
					}
					fmt.Fprint(w, `{"data":{"issueCreate":{"success":true,"issue":{"identifier":"PRO-1423","url":"https://linear.app/PRO-1423","title":"Swap","state":{"name":"Backlog"}}}}}`)
				default:
					t.Fatal(request.Query)
				}
			}))
			issue, err := l.Create(context.Background(), Draft{Title: "Swap " + secret, Body: "31 requests\n\nArtifact: Prototype — https://hub.test/artifacts/prototype\n\n---\nFrom AI Chat · ana", State: "backlog", Labels: []string{"AI-CHAT", "Product work"}, BeforeSend: func() error { marked = true; return nil }})
			if err != nil || sent != 1 || issue.Identifier != "PRO-1423" || issue.Team != "Product" || issue.State != "Backlog" || !reflect.DeepEqual(issue.Labels, []string{"ai-chat", "Product work"}) {
				t.Fatal(issue, err, sent)
			}
		})
	}
}

func TestLinearCreateUnknownFieldsAndDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, state string
		labels      []string
		want        string
	}{
		{name: "unknown state", state: "Missing", want: `State "Missing" does not exist in Linear team Product.`},
		{name: "unknown label", labels: []string{"ai-chat"}, want: `Label "ai-chat" does not exist in Linear team Product.`},
		{name: "defaults"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			l := NewLinear("Product", secret, fake(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Query     string
					Variables map[string]any
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				switch req.Query {
				case createTeamQuery:
					fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"team","name":"Product"}]}}}`)
				case createStatesQuery:
					fmt.Fprint(w, `{"data":{"workflowStates":{"nodes":[]}}}`)
				case createLabelsQuery:
					fmt.Fprint(w, `{"data":{"issueLabels":{"nodes":[]}}}`)
				case createIssueMutation:
					writes++
					input := req.Variables["input"].(map[string]any)
					if len(input) != 3 || input["stateId"] != nil || input["labelIds"] != nil {
						t.Fatal(input)
					}
					fmt.Fprint(w, `{"data":{"issueCreate":{"success":true,"issue":{"identifier":"PRO-1","url":"https://linear.app/PRO-1","title":"Title","state":{"name":"Backlog"}}}}}`)
				}
			}))
			_, err := l.Create(context.Background(), Draft{Title: "Title", Body: "Body", State: tc.state, Labels: tc.labels})
			if tc.want != "" {
				if err == nil || err.Error() != tc.want || writes != 0 {
					t.Fatal(err, writes)
				}
			} else if err != nil || writes != 1 {
				t.Fatal(err, writes)
			}
		})
	}
}

func TestLinearCreateSafeErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, want string
	}{
		{"401", 401, secret, "The workspace token cannot create issues in team Product."},
		{"403", 403, secret, "The workspace token cannot create issues in team Product."},
		{"permission", 200, `{"errors":[{"message":"` + secret + `","extensions":{"code":"FORBIDDEN"}}]}`, "The workspace token cannot create issues in team Product."},
		{"permission text", 200, `{"errors":[{"message":"Permission denied ` + secret + `"}]}`, "The workspace token cannot create issues in team Product."},
		{"graphql", 200, `{"errors":[{"message":"` + secret + `"}]}`, LinearRejected},
		{"http", 400, secret, LinearRejected},
		{"gateway", 502, secret, LinearUnconfirmed},
		{"malformed", 200, `{`, LinearUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLinear("Product", secret, fake(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Query string }
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.Query == createTeamQuery {
					fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"team","name":"Product"}]}}}`)
					return
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			_, err := l.Create(context.Background(), Draft{Title: "Title", Body: "Body"})
			if err == nil || err.Error() != tc.want || strings.Contains(err.Error(), secret) {
				t.Fatal(err)
			}
		})
	}
}

func TestLinearCreateCancellationBoundaries(t *testing.T) {
	for _, phase := range []string{"before resolution", "before mutation", "after mutation", "marker failure"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writes := 0
			l := NewLinear("Product", secret, fake(func(w http.ResponseWriter, r *http.Request) {
				var req struct{ Query string }
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.Query == createTeamQuery {
					fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"team","name":"Product"}]}}}`)
					if phase == "before mutation" {
						cancel()
					}
					return
				}
				writes++
				cancel()
			}))
			if phase == "before resolution" {
				cancel()
			}
			_, err := l.Create(ctx, Draft{Title: "Title", Body: "Body", BeforeSend: func() error {
				if phase == "marker failure" {
					return errors.New("database unavailable")
				}
				return nil
			}})
			if err == nil {
				t.Fatal("missing error")
			}
			if phase == "after mutation" {
				if writes != 1 || err.Error() != LinearUnconfirmed {
					t.Fatal(writes, err)
				}
			} else if writes != 0 {
				t.Fatal("created after cancellation")
			}
		})
	}
}

func TestLinearCreateSkipsLabelGroups(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(fmt.Sprint(child), func(t *testing.T) {
			writes := 0
			l := NewLinear("Product", secret, fake(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Query     string
					Variables map[string]any
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				switch req.Query {
				case createTeamQuery:
					fmt.Fprint(w, `{"data":{"teams":{"nodes":[{"id":"team","name":"Product"}]}}}`)
				case createLabelsQuery:
					if !strings.Contains(req.Query, "isGroup") {
						t.Fatal("label group metadata not requested")
					}
					nodes := []map[string]any{{"id": "group", "name": "Priority", "isGroup": true}}
					if child {
						nodes = append(nodes, map[string]any{"id": "label", "name": "Priority", "isGroup": false})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issueLabels": map[string]any{"nodes": nodes}}})
				case createIssueMutation:
					writes++
					input := req.Variables["input"].(map[string]any)
					if !reflect.DeepEqual(input["labelIds"], []any{"label"}) {
						t.Fatal(input)
					}
					fmt.Fprint(w, `{"data":{"issueCreate":{"success":true,"issue":{"identifier":"PRO-1","url":"https://linear.app/PRO-1","title":"Draft"}}}}`)
				}
			}))
			_, err := l.Create(context.Background(), Draft{Title: "Draft", Body: "Evidence", Labels: []string{"priority"}})
			if child {
				if err != nil || writes != 1 {
					t.Fatal(err, writes)
				}
			} else {
				var failure *CreateError
				if !errors.As(err, &failure) || !failure.Validation || failure.Message != `Label "priority" does not exist in Linear team Product.` || writes != 0 {
					t.Fatal(err, writes)
				}
			}
		})
	}
}
