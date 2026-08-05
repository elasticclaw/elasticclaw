package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elasticclaw/elasticclaw/pkg/types"
)

func TestCallLLMForRoutineDraftUsesCodexAuthProfile(t *testing.T) {
	installRoutineDraftCodexHelper(t)

	raw, err := callLLMForRoutineDraft(
		context.Background(),
		"Draft a weekday dependency review.",
		types.LLMKeysList{{
			Name:         "codex-chatgpt",
			Provider:     "codex",
			Default:      true,
			DefaultModel: "codex/gpt-5.5",
			AuthProfile:  "codex-default",
		}},
		"",
		[]*types.ModelAuthProfileConfig{{
			Name:      "codex-default",
			Provider:  "codex",
			Mode:      "device",
			AuthState: testCodexAuthState(t),
		}},
	)
	if err != nil {
		t.Fatalf("draft with Codex auth profile: %v", err)
	}
	draft, err := parseRoutineDraft(raw, "UTC")
	if err != nil {
		t.Fatalf("parse Codex routine draft: %v", err)
	}
	if draft.Name != "dependency-health" {
		t.Fatalf("draft name = %q, want dependency-health", draft.Name)
	}
}

func TestCallLLMForRoutineDraftRejectsMissingCodexAuthProfile(t *testing.T) {
	_, err := callLLMForRoutineDraft(
		context.Background(),
		"Draft a routine.",
		types.LLMKeysList{{
			Name:         "codex-chatgpt",
			Provider:     "codex",
			DefaultModel: "codex/gpt-5.5",
			AuthProfile:  "missing",
		}},
		"",
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), `Codex auth profile "missing" is not configured`) {
		t.Fatalf("error = %v, want missing Codex auth profile", err)
	}
}

func TestCallCodexCLIForRoutineDraftCleansTemporaryAuth(t *testing.T) {
	marker := installRoutineDraftCodexHelper(t)

	raw, err := callCodexCLIForRoutineDraft(
		context.Background(),
		"Draft a weekday dependency review.",
		"gpt-5.5",
		testCodexAuthState(t),
	)
	if err != nil {
		t.Fatalf("call Codex CLI: %v", err)
	}
	if !strings.Contains(raw, `"dependency-health"`) {
		t.Fatalf("unexpected Codex output: %s", raw)
	}
	authRootBytes, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read helper marker: %v", err)
	}
	authRoot := strings.TrimSpace(string(authRootBytes))
	if authRoot == "" {
		t.Fatal("helper did not record temporary auth root")
	}
	if _, err := os.Stat(authRoot); !os.IsNotExist(err) {
		t.Fatalf("temporary auth root still exists: %s", authRoot)
	}
}

func TestRestoreCLIAuthBundleRejectsUnsafePath(t *testing.T) {
	bundle := cliAuthBundle{
		Files: map[string]string{
			"../auth.json": base64.StdEncoding.EncodeToString([]byte("not-secret")),
		},
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal auth bundle: %v", err)
	}
	state := base64.StdEncoding.EncodeToString(data)
	if err := restoreCLIAuthBundle(t.TempDir(), state); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("error = %v, want unsafe path rejection", err)
	}
}

func installRoutineDraftCodexHelper(t *testing.T) string {
	t.Helper()
	original := routineDraftCommandContext
	routineDraftCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		helperArgs := append([]string{"-test.run=TestRoutineDraftCodexHelperProcess", "--"}, args...)
		return exec.CommandContext(ctx, os.Args[0], helperArgs...)
	}
	t.Cleanup(func() {
		routineDraftCommandContext = original
	})
	marker := filepath.Join(t.TempDir(), "auth-root")
	t.Setenv("ELASTICCLAW_ROUTINE_DRAFT_HELPER", "1")
	t.Setenv("ELASTICCLAW_ROUTINE_DRAFT_MARKER", marker)
	return marker
}

func testCodexAuthState(t *testing.T) string {
	t.Helper()
	bundle := cliAuthBundle{
		Files: map[string]string{
			".codex/auth.json": base64.StdEncoding.EncodeToString([]byte(`{"test":"credential"}`)),
		},
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("marshal auth bundle: %v", err)
	}
	return base64.StdEncoding.EncodeToString(data)
}

func TestRoutineDraftCodexHelperProcess(t *testing.T) {
	if os.Getenv("ELASTICCLAW_ROUTINE_DRAFT_HELPER") != "1" {
		return
	}
	fail := func(format string, args ...any) {
		_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
		os.Exit(2)
	}

	authPath := filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")
	info, err := os.Stat(authPath)
	if err != nil {
		fail("stat restored auth: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		fail("restored auth mode = %o, want 600", info.Mode().Perm())
	}
	auth, err := os.ReadFile(authPath)
	if err != nil || string(auth) != `{"test":"credential"}` {
		fail("restored auth content is invalid")
	}
	if os.Getenv("OPENAI_API_KEY") != "" || os.Getenv("CODEX_API_KEY") != "" {
		fail("host API key leaked into Codex draft environment")
	}

	prompt, err := io.ReadAll(os.Stdin)
	if err != nil || !strings.Contains(string(prompt), routineDraftSystemPrompt) {
		fail("routine draft prompt was not provided")
	}

	var outputPath, schemaPath, model string
	args := os.Args
	for i, arg := range args {
		switch arg {
		case "--output-last-message":
			if i+1 < len(args) {
				outputPath = args[i+1]
			}
		case "--output-schema":
			if i+1 < len(args) {
				schemaPath = args[i+1]
			}
		case "--model":
			if i+1 < len(args) {
				model = args[i+1]
			}
		}
	}
	if outputPath == "" || schemaPath == "" || model == "" {
		fail("missing required Codex arguments")
	}
	if _, err := os.Stat(schemaPath); err != nil {
		fail("routine draft schema is unavailable: %v", err)
	}
	if err := os.WriteFile(
		outputPath,
		[]byte(`{"name":"dependency-health","task":"Inspect dependencies and report results.","schedule":"0 9 * * 1-5","timezone":"UTC","overlapPolicy":"skip","timeout":"2h"}`),
		0o600,
	); err != nil {
		fail("write Codex output: %v", err)
	}
	if marker := os.Getenv("ELASTICCLAW_ROUTINE_DRAFT_MARKER"); marker != "" {
		if err := os.WriteFile(marker, []byte(os.Getenv("HOME")), 0o600); err != nil {
			fail("write helper marker: %v", err)
		}
	}
	os.Exit(0)
}

func TestParseRoutineDraft(t *testing.T) {
	draft, err := parseRoutineDraft(`{
		"name": "dependency-health",
		"task": "Inspect dependencies, apply safe updates, run tests, and report the result.",
		"schedule": "0 9 * * 1-5",
		"timezone": "America/Argentina/Buenos_Aires",
		"overlapPolicy": "skip",
		"timeout": "2h"
	}`, "UTC")
	if err != nil {
		t.Fatalf("parse routine draft: %v", err)
	}
	if draft.Name != "dependency-health" || draft.Schedule != "0 9 * * 1-5" {
		t.Fatalf("unexpected draft: %#v", draft)
	}
}

func TestParseRoutineDraftAcceptsFencedJSONAndDefaults(t *testing.T) {
	draft, err := parseRoutineDraft("```json\n"+
		`{"name":"daily-review","task":"Review open work and report blockers.","schedule":"0 9 * * *","timezone":"","overlapPolicy":"","timeout":""}`+
		"\n```", "America/Argentina/Buenos_Aires")
	if err != nil {
		t.Fatalf("parse routine draft: %v", err)
	}
	if draft.Timezone != "America/Argentina/Buenos_Aires" || draft.OverlapPolicy != "skip" || draft.Timeout != "2h" {
		t.Fatalf("defaults not applied: %#v", draft)
	}
}

func TestParseRoutineDraftRejectsInvalidFields(t *testing.T) {
	tests := map[string]string{
		"name":     `{"name":"Bad Name","task":"Do work.","schedule":"0 9 * * *","timezone":"UTC","overlapPolicy":"skip","timeout":"1h"}`,
		"schedule": `{"name":"good-name","task":"Do work.","schedule":"not cron","timezone":"UTC","overlapPolicy":"skip","timeout":"1h"}`,
		"timezone": `{"name":"good-name","task":"Do work.","schedule":"0 9 * * *","timezone":"Moon/Base","overlapPolicy":"skip","timeout":"1h"}`,
		"overlap":  `{"name":"good-name","task":"Do work.","schedule":"0 9 * * *","timezone":"UTC","overlapPolicy":"queue","timeout":"1h"}`,
		"timeout":  `{"name":"good-name","task":"Do work.","schedule":"0 9 * * *","timezone":"UTC","overlapPolicy":"skip","timeout":"later"}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRoutineDraft(raw, "UTC"); err == nil {
				t.Fatal("expected invalid draft to fail")
			}
		})
	}
}
