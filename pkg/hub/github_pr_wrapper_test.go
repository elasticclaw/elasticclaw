package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func githubWrapperHeredoc(t *testing.T, script, marker string) string {
	t.Helper()
	_, body, ok := strings.Cut(script, "<< '"+marker+"'\n")
	if !ok {
		t.Fatalf("missing %s heredoc", marker)
	}
	body, _, ok = strings.Cut(body, "\n"+marker+"\n")
	if !ok {
		t.Fatalf("missing %s terminator", marker)
	}
	return body + "\n"
}

func TestGitHubPRWrapperCommands(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required by the installed GitHub helper")
	}
	for _, tc := range []struct {
		name   string
		args   []string
		stdout string
		status int
		report bool
		exec   bool
	}{
		{"create", []string{"pr", "create", "--title", "a title"}, "created\x00\xff\nhttps://github.com/org/repo/pull/42\n\n", 0, true, false},
		{"failure", []string{"pr", "create"}, "https://github.com/org/repo/pull/42\n", 17, false, false},
		{"api_method", []string{"api", "repos/org/repo/pulls", "-X", "POST"}, `{"html_url":"https://github.com/org/repo/pull/42"}`, 0, true, false},
		{"api_long_method", []string{"api", "--method", "POST", "/repos/org/repo/pulls"}, `{"html_url":"https://github.com/org/repo/pull/42"}`, 0, true, false},
		{"api_compact_method", []string{"api", "-XPOST", "repos/org/repo/pulls"}, `{"html_url":"https://github.com/org/repo/pull/42"}`, 0, true, false},
		{"api_equals_method", []string{"api", "--method=POST", "repos/org/repo/pulls"}, `{"html_url":"https://github.com/org/repo/pull/42"}`, 0, true, false},
		{"api_field", []string{"api", "repos/org/repo/pulls", "-f", "title=test"}, `{"html_url":"https://github.com/org/repo/pull/42"}`, 0, true, false},
		{"api_typed_field", []string{"api", "repos/org/repo/pulls", "-F", "draft=true"}, `{"html_url":"https://github.com/org/repo/pull/42"}`, 0, true, false},
		{"api_long_field", []string{"api", "repos/org/repo/pulls", "--field", "title=test"}, `{"html_url":"https://github.com/org/repo/pull/42"}`, 0, true, false},
		{"api_raw_field", []string{"api", "repos/org/repo/pulls", "--raw-field=title=test"}, `{"html_url":"https://github.com/org/repo/pull/42"}`, 0, true, false},
		{"api_input", []string{"api", "repos/org/repo/pulls", "--input", "-"}, `{"html_url":"https://github.com/org/repo/pull/42"}`, 0, true, false},
		{"api_get_overrides_field", []string{"api", "repos/org/repo/pulls", "-f", "title=test", "-X", "GET"}, "unchanged", 0, false, true},
		{"api_get", []string{"api", "repos/org/repo/pulls"}, "unchanged", 0, false, true},
		{"api_wrong_path", []string{"api", "repos/org/repo/issues", "-X", "POST"}, "unchanged", 0, false, true},
		{"api_malformed_json", []string{"api", "repos/org/repo/pulls", "-X", "POST"}, "not-json", 0, false, false},
		{"api_failure", []string{"api", "repos/org/repo/pulls", "-X", "POST"}, `{"html_url":"https://github.com/org/repo/pull/42"}`, 9, false, false},
		{"non_create", []string{"pr", "view"}, "bytes\x00\n\n", 23, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, content string) string {
				t.Helper()
				path := filepath.Join(dir, name)
				if err := os.WriteFile(path, []byte(content), 0700); err != nil {
					t.Fatal(err)
				}
				return path
			}
			script := buildGitHubCLIWrapperInstallScript("https://hub.example.com", "claw-123", "token&with spaces")
			reporter := write("reporter", githubWrapperHeredoc(t, script, "REPORTEOF"))
			fakeGH := write("real-gh", "#!/bin/bash\nprintf '%s' \"$$\" > \"$TEST_DIR/pid\"\ncat > \"$TEST_DIR/stdin\"\ncat \"$TEST_DIR/stdout\"\nprintf 'stderr bytes\\n\\n' >&2\nexit \"$TEST_STATUS\"\n")
			write("curl", "#!/bin/bash\nprintf '%s\\n' \"$@\" > \"$TEST_DIR/report.tmp\"\nmv \"$TEST_DIR/report.tmp\" \"$TEST_DIR/report\"\nexit 28\n")
			write("stdout", tc.stdout)
			wrapper := githubWrapperHeredoc(t, script, "GHEOF")
			wrapper = strings.ReplaceAll(wrapper, "__ELASTICCLAW_REAL_GH__", fakeGH)
			wrapper = strings.ReplaceAll(wrapper, "/usr/local/bin/elasticclaw-report-pr", reporter)
			wrapper = strings.ReplaceAll(wrapper, "/usr/local/bin/elasticclaw-git-credentials", filepath.Join(dir, "missing-credentials"))
			wrapperPath := write("gh", wrapper)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", append([]string{wrapperPath}, tc.args...)...)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_DIR="+dir, "TEST_STATUS="+strconv.Itoa(tc.status), "TMPDIR="+dir)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			cmd.Stdin = strings.NewReader("stdin bytes\x00\n")
			_ = cmd.Run()
			if got := cmd.ProcessState.ExitCode(); got != tc.status {
				t.Fatalf("exit=%d, want %d; stderr=%s", got, tc.status, stderr.String())
			}
			if stdout.String() != tc.stdout || stderr.String() != "stderr bytes\n\n" {
				t.Fatalf("output changed: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			input, err := os.ReadFile(filepath.Join(dir, "stdin"))
			if err != nil || string(input) != "stdin bytes\x00\n" {
				t.Fatalf("stdin changed: %q, %v", input, err)
			}
			if tc.exec {
				pid, _ := os.ReadFile(filepath.Join(dir, "pid"))
				if string(pid) != strconv.Itoa(cmd.Process.Pid) {
					t.Fatalf("non-create command did not exec: gh pid=%s wrapper pid=%d", pid, cmd.Process.Pid)
				}
			}
			// Completion is observable from temp-file cleanup, even with no report.
			// BSD mktemp (macOS) ignores TMPDIR, so also wait for the report itself.
			deadline := time.Now().Add(3 * time.Second)
			for {
				files, _ := filepath.Glob(filepath.Join(dir, "tmp.*"))
				_, reportErr := os.Stat(filepath.Join(dir, "report"))
				done := len(files) == 0 && (!tc.report || reportErr == nil)
				if done || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			report, err := os.ReadFile(filepath.Join(dir, "report"))
			if !tc.report {
				if err == nil {
					t.Fatalf("unexpected report: %s", report)
				}
				return
			}
			if err != nil {
				t.Fatalf("missing report: %v", err)
			}
			args := strings.Split(strings.TrimSpace(string(report)), "\n")
			if !strings.Contains(string(report), "--max-time\n10\n") || args[len(args)-1] != "https://hub.example.com/api/claws/claw-123/prs?claw_token=token%26with+spaces" {
				t.Fatalf("unexpected curl invocation: %s", report)
			}
			var payload map[string]string
			for i, arg := range args {
				if arg == "--data" {
					if err := json.Unmarshal([]byte(args[i+1]), &payload); err != nil {
						t.Fatal(err)
					}
				}
			}
			if payload["url"] != "https://github.com/org/repo/pull/42" || payload["source"] != "gh_create" {
				t.Fatalf("unexpected report payload: %v", payload)
			}
		})
	}
}

func TestGitHubPRWrapperTTY(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required by the installed GitHub helper")
	}
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	fakeGH := write("real-gh", `#!/bin/bash
if [ "$2" = "create" ]; then
  [ -t 1 ] || exit 91
  cat > "$TEST_DIR/stdin"
  printf 'interactive output\n'
  printf 'interactive stderr\n' >&2
else
  printf '%s\n' "$@" > "$TEST_DIR/view"
  printf 'https://github.com/org/repo/pull/42\n'
fi
`)
	reporter := write("reporter", "#!/bin/bash\ncat \"$2\" > \"$TEST_DIR/reported\"\n")
	wrapper := githubWrapperHeredoc(t, buildGitHubCLIWrapperInstallScript("https://hub.example.com", "claw", "token"), "GHEOF")
	wrapper = strings.ReplaceAll(wrapper, "__ELASTICCLAW_REAL_GH__", fakeGH)
	wrapper = strings.ReplaceAll(wrapper, "/usr/local/bin/elasticclaw-report-pr", reporter)
	wrapper = strings.ReplaceAll(wrapper, "/usr/local/bin/elasticclaw-git-credentials", filepath.Join(dir, "missing-credentials"))
	wrapperPath := write("gh", wrapper)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", `import errno, json, os, pty, select, subprocess, sys, termios
master, slave = pty.openpty()
attrs = termios.tcgetattr(slave)
attrs[1] = 0
termios.tcsetattr(slave, termios.TCSANOW, attrs)
p = subprocess.Popen(["bash", sys.argv[1], "pr", "create", "--repo", "org/repo"], stdin=subprocess.PIPE, stdout=slave, stderr=subprocess.PIPE)
_, stderr = p.communicate(b"interactive stdin\n")
stdout = b""
while select.select([master], [], [], 0.1)[0]:
    try:
        chunk = os.read(master, 4096)
        if not chunk:
            break
        stdout += chunk
    except OSError as e:
        if e.errno != errno.EIO:
            raise
        break
os.close(slave)
os.close(master)
print(json.dumps({"status": p.returncode, "stdout": stdout.decode(), "stderr": stderr.decode()}))
`, wrapperPath)
	cmd.Env = append(os.Environ(), "TEST_DIR="+dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("TTY harness: %v: %s", err, output)
	}
	var result struct {
		Status int
		Stdout string
		Stderr string
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != 0 || result.Stdout != "interactive output\n" || result.Stderr != "interactive stderr\n" {
		t.Fatalf("TTY behavior changed: %+v", result)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		reported, err := os.ReadFile(filepath.Join(dir, "reported"))
		if err == nil && string(reported) == "https://github.com/org/repo/pull/42\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("missing TTY report: %q, %v", reported, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	view, _ := os.ReadFile(filepath.Join(dir, "view"))
	if string(view) != "pr\nview\n--repo\norg/repo\n--json\nurl\n-q\n.url\n" {
		t.Fatalf("unexpected TTY fallback: %s", view)
	}
	input, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if string(input) != "interactive stdin\n" {
		t.Fatalf("TTY stdin changed: %q", input)
	}
}

func TestGitHubPRWrapperTTYLookupTimeout(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required by the installed GitHub helper")
	}
	fakeGH := filepath.Join(t.TempDir(), "slow-gh")
	if err := os.WriteFile(fakeGH, []byte("#!/bin/bash\nsleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := githubWrapperHeredoc(t, buildGitHubCLIWrapperInstallScript("https://hub.example.com", "claw", "token"), "VIEWEOF")
	script = strings.ReplaceAll(script, "timeout=10", "timeout=0.05")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", script, fakeGH)
	output, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil || len(output) != 0 {
		t.Fatalf("TTY lookup did not time out quietly: err=%v context=%v output=%q", err, ctx.Err(), output)
	}
}
