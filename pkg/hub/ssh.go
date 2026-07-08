// SSH helpers for running scripts and writing files on remote VMs.
//
// Split out of the former server.go; same package, no behavior changes.
package hub

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// syncedWriter wraps a bytes.Buffer with a mutex to make it safe for concurrent writes.
type syncedWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w *syncedWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// sshRun connects to host via the hub's SSH identity and runs a script.
func (s *Server) sshRun(user, host, script string) error {
	output, err := s.sshRunWithTimeout(user, host, script, 0)
	if err != nil {
		return err
	}
	logf("bootstrap output:\n%s", output)
	return nil
}

// sshRunWithTimeout connects to host via the hub's SSH identity and runs a script.
// A zero timeout waits for the remote command to finish.
func (s *Server) sshRunWithTimeout(user, host, script string, timeout time.Duration) (string, error) {
	pubKeyType := s.identity.PrivateKey.PublicKey().Type()
	pubKeyFP := gossh.FingerprintSHA256(s.identity.PrivateKey.PublicKey())
	logf("SSH attempting: user=%s host=%s key-type=%s fingerprint=%s", user, host, pubKeyType, pubKeyFP)
	logf("SSH public key being used:\n%s", s.identity.PublicKey)

	sshCfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(s.identity.PrivateKey)},
		HostKeyCallback: s.sshHostKeyCallback(host),
		Timeout:         30 * time.Second,
	}

	client, err := gossh.Dial("tcp", host, sshCfg)
	if err != nil {
		return "", fmt.Errorf("ssh dial %s: %w", host, err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	// Pipe the script to bash via stdin — avoids the server's default shell (/bin/sh,
	// often dash on Ubuntu) which may not support bash-specific syntax.
	var buf bytes.Buffer
	var mu sync.Mutex
	syncWriter := &syncedWriter{buf: &buf, mu: &mu}
	sess.Stdout = syncWriter
	sess.Stderr = syncWriter
	sess.Stdin = strings.NewReader(script)

	runDone := make(chan error, 1)
	go func() {
		runDone <- sess.Run("/bin/bash")
	}()
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case err := <-runDone:
			if err != nil {
				mu.Lock()
				output := buf.String()
				mu.Unlock()
				return output, fmt.Errorf("ssh script failed: %w\noutput: %s", err, output)
			}
		case <-timer.C:
			_ = sess.Close()
			_ = client.Close()
			mu.Lock()
			output := buf.String()
			mu.Unlock()
			return output, fmt.Errorf("ssh script timed out after %s\noutput: %s", timeout, output)
		}
	} else if err := <-runDone; err != nil {
		mu.Lock()
		output := buf.String()
		mu.Unlock()
		return output, fmt.Errorf("ssh script failed: %w\noutput: %s", err, output)
	}
	mu.Lock()
	output := buf.String()
	mu.Unlock()
	return output, nil
}

func cleanWorkspaceFilePath(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.Contains(trimmed, "\x00") {
		return "", fmt.Errorf("path contains NUL byte")
	}
	cleaned := path.Clean(filepath.ToSlash(trimmed))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("path must stay inside workspace")
	}
	return cleaned, nil
}

func sshHomeDir(user string) (string, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return "", fmt.Errorf("empty SSH user")
	}
	if strings.ContainsAny(user, "/\x00") {
		return "", fmt.Errorf("SSH user contains invalid characters")
	}
	if user == "root" {
		return "/root", nil
	}
	return "/home/" + user, nil
}

// sshWriteFiles writes a map of filename->content to a remote directory via SSH.
func (s *Server) sshWriteFiles(user, host, dir string, files map[string]string) error {
	sshCfg := &gossh.ClientConfig{
		User:            user,
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(s.identity.PrivateKey)},
		HostKeyCallback: s.sshHostKeyCallback(host),
		Timeout:         30 * time.Second,
	}
	client, err := gossh.Dial("tcp", host, sshCfg)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer client.Close()

	for name, content := range files {
		sess, err := client.NewSession()
		if err != nil {
			return fmt.Errorf("ssh session: %w", err)
		}
		safeName, err := cleanWorkspaceFilePath(name)
		if err != nil {
			sess.Close()
			return fmt.Errorf("invalid template file path %q: %w", name, err)
		}
		cmd := remoteWriteFileCommand(dir, safeName, content)
		out, err := sess.CombinedOutput(cmd)
		sess.Close()
		if err != nil {
			return fmt.Errorf("write %s: %w\n%s", name, err, string(out))
		}
	}
	return nil
}

func remoteWriteFileCommand(dir, name, content string) string {
	remotePath := strings.TrimRight(dir, "/") + "/" + name
	remoteDir := path.Dir(remotePath)
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	return fmt.Sprintf("mkdir -p -- %s && base64 -d > %s << 'ELASTICCLAW_B64'\n%s\nELASTICCLAW_B64",
		shellDoubleQuote(remoteDir),
		shellDoubleQuote(remotePath),
		encoded,
	)
}
