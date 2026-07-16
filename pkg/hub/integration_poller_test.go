package hub

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestPollSinceHighWaterMarkAndFailedWindow(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	n := time.Now().UTC().Truncate(time.Second)
	cases := []struct {
		name  string
		setup func()
		want  time.Time
	}{
		{"fresh database", func() {}, n.Add(-2 * time.Minute)},
		{"catch up from high-water mark", func() { s.setPollHighWaterMark("linear", n.Add(-30*time.Minute)) }, n.Add(-30 * time.Minute)},
		{"clamp old high-water mark", func() { s.setPollHighWaterMark("linear", n.Add(-72*time.Hour)) }, n.Add(-24 * time.Hour)},
		{"extend for failed trigger", func() {
			s.setPollHighWaterMark("linear", n)
			_, _ = s.db.Exec(`INSERT INTO factory_triggers(id,factory_name,integration,trigger_key,status,first_seen_at,last_seen_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?)`, "failed-window", "code", "linear", "linear:ELA-1", "failed", n.Add(-time.Hour), n, n, n)
		}, n.Add(-65 * time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _ = s.db.Exec(`DELETE FROM integration_poll_state`)
			_, _ = s.db.Exec(`DELETE FROM factory_triggers`)
			tc.setup()
			if got := s.pollSince("linear", n); !got.Equal(tc.want) {
				t.Fatalf("pollSince = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestPollHighWaterMarkAdvancesOnlyAfterSuccessfulLinearQuery(t *testing.T) {
	s := newFactoryTriggerTestServer(t)
	n := time.Now().UTC().Truncate(time.Second)
	s.setPollHighWaterMark("linear", n.Add(-30*time.Minute))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()
	s.linearBaseURL = server.URL
	factories := []*types.FactoryConfig{{Name: "code", Integration: "linear", Workspace: "eng"}}
	if s.pollLinear(factories, []*types.LinearIntegrationConfig{{Workspace: "eng", Token: "token"}}, s.pollSince("linear", n).Format(time.RFC3339)) {
		t.Fatal("failed query reported success")
	}
	got, ok := s.getPollHighWaterMark("linear")
	if !ok || !got.Equal(n.Add(-30*time.Minute)) {
		t.Fatalf("failed query advanced high-water mark to %s", got)
	}
}
