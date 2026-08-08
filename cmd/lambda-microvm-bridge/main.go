// lambda-microvm-bridge adapts the AWS Lambda MicroVM HTTPS and lifecycle-hook
// APIs to the ElasticClaw claw-bridge process. It is baked into the MicroVM
// image and snapshotted before any per-claw credentials are supplied.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultAddress   = ":8080"
	defaultBridgeBin = "/usr/local/bin/claw-bridge"
	defaultWorkspace = "/home/claw/workspace"
	maxRequestBytes  = 16 << 20
	bridgeStartGrace = 2 * time.Second
)

type server struct {
	bridgeBinary string
	workspaceDir string

	mu           sync.Mutex
	bridge       *exec.Cmd
	started      bool
	bridgeExited bool
	startGrace   time.Duration
	runtimeEnv   []string
	microVMID    string
}

type lifecycleRequest struct {
	MicroVMID      string `json:"microvmId"`
	RunHookPayload string `json:"runHookPayload"`
}

type runPayload struct {
	Version       int               `json:"version"`
	Name          string            `json:"name"`
	Env           map[string]string `json:"env"`
	TemplateFiles map[string]string `json:"templateFiles"`
	StateMount    string            `json:"stateMount"`
}

type execRequest struct {
	Command []string `json:"command"`
}

type execResponse struct {
	ExitCode int    `json:"ExitCode"`
	Stdout   string `json:"Stdout"`
	Stderr   string `json:"Stderr"`
}

type writeFileRequest struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func main() {
	s := &server{
		bridgeBinary: envOrDefault("ELASTICCLAW_BRIDGE_BINARY", defaultBridgeBin),
		workspaceDir: envOrDefault("ELASTICCLAW_WORKSPACE_DIR", defaultWorkspace),
	}
	address := envOrDefault("ELASTICCLAW_LAMBDA_BRIDGE_ADDR", defaultAddress)
	log.Printf("lambda microvm bridge listening on %s", address)
	if err := http.ListenAndServe(address, s.routes()); err != nil {
		log.Fatal(err)
	}
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/aws/lambda-microvms/runtime/v1/ready", okHook)
	mux.HandleFunc("/aws/lambda-microvms/runtime/v1/validate", okHook)
	mux.HandleFunc("/aws/lambda-microvms/runtime/v1/resume", okHook)
	mux.HandleFunc("/aws/lambda-microvms/runtime/v1/suspend", okHook)
	mux.HandleFunc("/aws/lambda-microvms/runtime/v1/terminate", okHook)
	mux.HandleFunc("/aws/lambda-microvms/runtime/v1/run", s.runHook)
	mux.HandleFunc("/elasticclaw/v1/init", s.initialize)
	mux.HandleFunc("/elasticclaw/v1/exec", s.exec)
	mux.HandleFunc("/elasticclaw/v1/write-file", s.writeFile)
	mux.HandleFunc("/healthz", s.health)
	return http.MaxBytesHandler(mux, maxRequestBytes)
}

func okHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) runHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var lifecycle lifecycleRequest
	if err := decodeJSON(r.Body, &lifecycle); err != nil {
		http.Error(w, "invalid lifecycle payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	var payload runPayload
	if err := json.Unmarshal([]byte(lifecycle.RunHookPayload), &payload); err != nil {
		http.Error(w, "invalid run hook payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if payload.Version != 1 {
		http.Error(w, fmt.Sprintf("unsupported run hook payload version %d", payload.Version), http.StatusBadRequest)
		return
	}
	if payload.Env == nil {
		payload.Env = map[string]string{}
	}
	s.mu.Lock()
	s.microVMID = lifecycle.MicroVMID
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *server) initialize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload runPayload
	if err := decodeJSON(r.Body, &payload); err != nil {
		http.Error(w, "invalid initialization payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if payload.Version != 1 {
		http.Error(w, fmt.Sprintf("unsupported initialization payload version %d", payload.Version), http.StatusBadRequest)
		return
	}
	if payload.Env == nil {
		payload.Env = map[string]string{}
	}
	s.mu.Lock()
	microVMID := s.microVMID
	s.mu.Unlock()
	if microVMID != "" {
		payload.Env["ELASTICCLAW_MICROVM_ID"] = microVMID
	}
	if err := s.startBridge(payload); err != nil {
		log.Printf("initialization failed: %v", err)
		http.Error(w, "failed to initialize ElasticClaw", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) startBridge(payload runPayload) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	for name := range payload.Env {
		if !validEnvName(name) {
			return fmt.Errorf("invalid environment variable name %q", name)
		}
	}
	if err := writeTemplateFiles(s.workspaceDir, payload.TemplateFiles); err != nil {
		return err
	}
	cmd := exec.Command(s.bridgeBinary, "--bootstrap")
	s.runtimeEnv = mergeEnv(os.Environ(), payload.Env)
	cmd.Env = s.runtimeEnv
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claw-bridge: %w", err)
	}
	s.bridge = cmd
	s.bridgeExited = false
	exited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		exited <- err
		if err != nil {
			log.Printf("claw-bridge exited: %v", err)
		}
		s.mu.Lock()
		if s.bridge == cmd {
			s.started = false
			s.bridgeExited = true
		}
		s.mu.Unlock()
	}()
	grace := s.startGrace
	if grace <= 0 {
		grace = bridgeStartGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case err := <-exited:
		s.bridge = nil
		s.started = false
		s.bridgeExited = true
		if err != nil {
			return fmt.Errorf("claw-bridge exited during startup: %w", err)
		}
		return errors.New("claw-bridge exited during startup")
	case <-timer.C:
		// Prefer an exit that raced with the timer over reporting a dead bridge
		// as initialized successfully.
		select {
		case err := <-exited:
			s.bridge = nil
			s.started = false
			s.bridgeExited = true
			if err != nil {
				return fmt.Errorf("claw-bridge exited during startup: %w", err)
			}
			return errors.New("claw-bridge exited during startup")
		default:
		}
	}
	s.started = true
	return nil
}

func writeTemplateFiles(workspace string, files map[string]string) error {
	if err := os.MkdirAll(workspace, 0755); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	for _, name := range sortedKeys(files) {
		rel, err := cleanRelativePath(name)
		if err != nil {
			return err
		}
		content, err := base64.StdEncoding.DecodeString(files[name])
		if err != nil {
			return fmt.Errorf("decode template file %q: %w", name, err)
		}
		destination := filepath.Join(workspace, rel)
		if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
			return fmt.Errorf("create template directory for %q: %w", name, err)
		}
		mode := os.FileMode(0644)
		if strings.HasPrefix(rel, "scripts"+string(filepath.Separator)) {
			mode = 0755
		}
		if err := os.WriteFile(destination, content, mode); err != nil {
			return fmt.Errorf("write template file %q: %w", name, err)
		}
	}
	readyPath := filepath.Join(workspace, ".elasticclaw-workspace-ready")
	if err := os.WriteFile(readyPath, []byte("ready\n"), 0644); err != nil {
		return fmt.Errorf("write workspace readiness marker: %w", err)
	}
	return nil
}

func cleanRelativePath(name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || strings.ContainsRune(name, '\x00') {
		return "", fmt.Errorf("invalid template file path %q", name)
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("template file path escapes workspace: %q", name)
	}
	return clean, nil
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func mergeEnv(base []string, overlay map[string]string) []string {
	values := make(map[string]string, len(base)+len(overlay))
	for _, item := range base {
		if index := strings.IndexByte(item, '='); index > 0 {
			values[item[:index]] = item[index+1:]
		}
	}
	for key, value := range overlay {
		if validEnvName(key) {
			values[key] = value
		}
	}
	keys := sortedKeys(values)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+values[key])
	}
	return result
}

func validEnvName(name string) bool {
	if name == "" || !isEnvFirst(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !isEnvFirst(name[i]) && (name[i] < '0' || name[i] > '9') {
			return false
		}
	}
	return true
}

func isEnvFirst(char byte) bool {
	return char == '_' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z'
}

func (s *server) exec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request execRequest
	if err := decodeJSON(r.Body, &request); err != nil || len(request.Command) == 0 {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, request.Command[0], request.Command[1:]...)
	s.mu.Lock()
	if s.runtimeEnv != nil {
		cmd.Env = append([]string(nil), s.runtimeEnv...)
	}
	s.mu.Unlock()
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	response := execResponse{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			response.ExitCode = exitError.ExitCode()
		} else {
			response.ExitCode = 1
			response.Stderr += err.Error()
		}
	}
	writeJSON(w, response)
}

func (s *server) writeFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request writeFileRequest
	if err := decodeJSON(r.Body, &request); err != nil {
		http.Error(w, "invalid request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(request.Path) || strings.ContainsRune(request.Path, '\x00') {
		http.Error(w, "path must be absolute", http.StatusBadRequest)
		return
	}
	content, err := base64.StdEncoding.DecodeString(request.Content)
	if err != nil {
		http.Error(w, "content must be base64 encoded", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(filepath.Dir(request.Path), 0755); err != nil {
		http.Error(w, "create parent directory failed", http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(request.Path, content, 0644); err != nil {
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := "ready"
	if s.started {
		status = "running"
	} else if s.bridgeExited {
		status = "bridge-exited"
	}
	writeJSON(w, map[string]string{"status": status})
}

func decodeJSON(reader io.Reader, target interface{}) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func writeJSON(w http.ResponseWriter, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func (s *server) shutdownBridge() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bridge != nil && s.bridge.Process != nil && s.bridge.ProcessState == nil {
		_ = s.bridge.Process.Signal(syscall.SIGTERM)
	}
}
