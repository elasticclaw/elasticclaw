package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/elasticclaw/elasticclaw/pkg/config"
	"github.com/elasticclaw/elasticclaw/pkg/types"
	"github.com/google/uuid"
)

type modelAuthLoginJob struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Profile   string `json:"profile"`
	Status    string `json:"status"`
	URL       string `json:"url,omitempty"`
	Code      string `json:"code,omitempty"`
	Output    string `json:"output,omitempty"`
	Error     string `json:"error,omitempty"`
	StartedAt string `json:"startedAt"`
	UpdatedAt string `json:"updatedAt"`
	authDir   string
}

type cliAuthBundle struct {
	Files map[string]string `json:"files"`
}

var (
	modelAuthANSIRE        = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	modelAuthOSC8RE        = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	modelAuthURLRE         = regexp.MustCompile(`https?://[^\s"'\x00-\x1f\x7f]+`)
	modelAuthCodeRE        = regexp.MustCompile(`(?i:\b(?:device code|user code|verification code|code)\b)[^A-Za-z0-9]*([A-Za-z0-9][A-Za-z0-9-]{3,})\b`)
	modelAuthOneTimeCodeRE = regexp.MustCompile(`(?is)\bone-time code\b.*?\n\s*([A-Z0-9]{4,5}-[A-Z0-9]{4,5})\b`)
	modelAuth9DigitCodeRE  = regexp.MustCompile(`\b\d(?:[ -]?\d){8}\b`)
)

func (s *Server) handleModelAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Provider string `json:"provider"`
		Profile  string `json:"profile"`
		Mode     string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.Profile = strings.TrimSpace(req.Profile)
	if req.Mode == "" {
		req.Mode = "device"
	}
	if req.Profile == "" {
		req.Profile = req.Provider + "-default"
	}
	if req.Provider != "codex" && req.Provider != "grok" {
		http.Error(w, "provider must be codex or grok", http.StatusBadRequest)
		return
	}
	if req.Mode != "device" {
		http.Error(w, "only device login is supported", http.StatusBadRequest)
		return
	}

	job := &modelAuthLoginJob{
		ID:        uuid.NewString(),
		Provider:  req.Provider,
		Profile:   req.Profile,
		Status:    "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.modelAuthJobsMu.Lock()
	if s.modelAuthJobs == nil {
		s.modelAuthJobs = map[string]*modelAuthLoginJob{}
	}
	s.modelAuthJobs[job.ID] = job
	s.modelAuthJobsMu.Unlock()

	go s.runModelAuthLoginJob(job)
	jsonOK(w, job)
}

func (s *Server) handleModelAuthLoginStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.PathValue("id")
	s.modelAuthJobsMu.Lock()
	job := s.modelAuthJobs[id]
	s.modelAuthJobsMu.Unlock()
	if job == nil {
		http.NotFound(w, r)
		return
	}
	jsonOK(w, job)
}

func (s *Server) runModelAuthLoginJob(job *modelAuthLoginJob) {
	authDir, err := os.MkdirTemp("", "elasticclaw-model-auth-*")
	if err != nil {
		s.finishModelAuthJob(job, "error", "", err)
		return
	}
	defer os.RemoveAll(authDir)
	job.authDir = authDir

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	var args []string
	switch job.Provider {
	case "codex":
		args = []string{"codex", "login", "--device-auth"}
	case "grok":
		args = []string{"grok", "login", "--device-auth"}
	default:
		s.finishModelAuthJob(job, "error", "", fmt.Errorf("unsupported provider %q", job.Provider))
		return
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "HOME="+authDir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.finishModelAuthJob(job, "error", "", err)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		s.finishModelAuthJob(job, "error", "", err)
		return
	}
	if err := cmd.Start(); err != nil {
		s.finishModelAuthJob(job, "error", "", err)
		return
	}

	done := make(chan struct{}, 2)
	go s.captureModelAuthOutput(job, stdout, done)
	go s.captureModelAuthOutput(job, stderr, done)
	<-done
	<-done
	err = cmd.Wait()
	if err != nil {
		s.finishModelAuthJob(job, "error", "", err)
		return
	}
	bundle, err := encodeCLIAuthBundle(authDir)
	if err != nil {
		s.finishModelAuthJob(job, "error", "", err)
		return
	}
	if err := s.saveModelAuthProfile(job.Provider, job.Profile, "device", bundle); err != nil {
		s.finishModelAuthJob(job, "error", "", err)
		return
	}
	s.finishModelAuthJob(job, "complete", bundle, nil)
}

func (s *Server) captureModelAuthOutput(job *modelAuthLoginJob, r io.Reader, done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s.appendModelAuthOutput(job, string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) appendModelAuthOutput(job *modelAuthLoginJob, raw string) {
	chunk := normalizeModelAuthOutput(raw)
	s.modelAuthJobsMu.Lock()
	defer s.modelAuthJobsMu.Unlock()
	job.Output += chunk
	if match := modelAuthURLRE.FindString(raw); match != "" {
		updateModelAuthURL(job, match)
	}
	if match := modelAuthURLRE.FindString(job.Output); match != "" {
		updateModelAuthURL(job, match)
	}
	if code := extractModelAuthCode(job.Output); code != "" {
		job.Code = code
	}
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
}

func updateModelAuthURL(job *modelAuthLoginJob, url string) {
	url = strings.TrimRight(url, ".,)")
	if len(url) > len(job.URL) {
		job.URL = url
	}
}

func extractModelAuthCode(output string) string {
	if match := modelAuthOneTimeCodeRE.FindStringSubmatch(output); len(match) == 2 {
		return match[1]
	}
	matches := modelAuthCodeRE.FindAllStringSubmatch(output, -1)
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		code := strings.Trim(match[1], ".,)")
		if isValidModelAuthCode(code) {
			return code
		}
	}
	if match := modelAuth9DigitCodeRE.FindString(output); match != "" {
		return strings.NewReplacer(" ", "", "-", "").Replace(match)
	}
	return ""
}

func isValidModelAuthCode(code string) bool {
	if len(code) < 4 {
		return false
	}
	lower := strings.ToLower(code)
	switch lower {
	case "authorization", "authenticate", "authentication", "login", "device", "verify", "verification":
		return false
	}
	hasDigit := false
	hasHyphen := false
	hasUpper := false
	for _, r := range code {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '-':
			hasHyphen = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			// Lowercase-only English words are usually CLI prose, not device codes.
		default:
			return false
		}
	}
	return hasDigit || hasHyphen || hasUpper
}

func normalizeModelAuthOutput(line string) string {
	line = modelAuthOSC8RE.ReplaceAllString(line, "")
	line = modelAuthANSIRE.ReplaceAllString(line, "")
	var b strings.Builder
	b.Grow(len(line))
	for _, r := range line {
		if (r >= 0 && r < 32 && r != '\n' && r != '\t') || r == 127 {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (s *Server) finishModelAuthJob(job *modelAuthLoginJob, status, _ string, err error) {
	s.modelAuthJobsMu.Lock()
	defer s.modelAuthJobsMu.Unlock()
	job.Status = status
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if err != nil {
		job.Error = err.Error()
	}
}

func encodeCLIAuthBundle(root string) (string, error) {
	bundle := cliAuthBundle{Files: map[string]string{}}
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".npm" || name == ".cache" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > 2*1024*1024 {
			return nil
		}
		total += info.Size()
		if total > 10*1024*1024 {
			return fmt.Errorf("auth bundle exceeds 10MiB")
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		bundle.Files[filepath.ToSlash(rel)] = base64.StdEncoding.EncodeToString(data)
		return nil
	})
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(bundle)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func (s *Server) saveModelAuthProfile(provider, name, mode, authState string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	updatedCfg := *s.hubCfg
	profiles := make([]*types.ModelAuthProfileConfig, len(updatedCfg.ModelAuthProfiles))
	for i, profile := range updatedCfg.ModelAuthProfiles {
		if profile == nil {
			continue
		}
		copy := *profile
		profiles[i] = &copy
	}
	var found *types.ModelAuthProfileConfig
	for _, profile := range profiles {
		if profile != nil && profile.Name == name {
			found = profile
			break
		}
	}
	if found == nil {
		found = &types.ModelAuthProfileConfig{Name: name}
		profiles = append(profiles, found)
	}
	found.Provider = provider
	found.Mode = mode
	found.AuthState = authState
	found.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	updatedCfg.ModelAuthProfiles = profiles
	if err := config.SaveHubConfig(&updatedCfg); err != nil {
		return err
	}
	s.hubCfg = &updatedCfg
	return nil
}

func buildModelAuthEnv(cfg *types.HubConfig, selectedKeyName string) string {
	if cfg == nil {
		return ""
	}
	key := resolveActiveKey(cfg.LLMKeys, selectedKeyName)
	if key == nil || key.AuthProfile == "" {
		return ""
	}
	for _, profile := range cfg.ModelAuthProfiles {
		if profile == nil || profile.Name != key.AuthProfile || profile.AuthState == "" {
			continue
		}
		if profile.Provider != key.Provider {
			continue
		}
		return fmt.Sprintf("export ELASTICCLAW_MODEL_AUTH_PROVIDER=%q\nexport ELASTICCLAW_MODEL_AUTH_STATE=%q\n", profile.Provider, profile.AuthState)
	}
	return ""
}

func buildModelAuthRestoreShell(modelAuthEnv string) string {
	if strings.TrimSpace(modelAuthEnv) == "" {
		return ""
	}
	return modelAuthEnv + `
python3 <<'PYEOF'
import base64, json, os
state = os.environ.get('ELASTICCLAW_MODEL_AUTH_STATE', '')
if state:
    bundle = json.loads(base64.b64decode(state).decode())
    home = os.path.expanduser('~')
    for rel, encoded in bundle.get('files', {}).items():
        clean = os.path.normpath(rel)
        if clean == '.' or clean == '..' or clean.startswith('../') or os.path.isabs(clean):
            continue
        path = os.path.join(home, clean)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, 'wb') as f:
            f.write(base64.b64decode(encoded))
        os.chmod(path, 0o600)
PYEOF
`
}
