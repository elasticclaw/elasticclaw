package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVersionFromOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "calver beta",
			in:   "elasticclaw 2026.8.8-beta.2 (commit: 4125c5f, built: 2026-08-08T12:35:51Z)",
			want: "2026.8.8-beta.2",
		},
		{
			name: "calver stable",
			in:   "elasticclaw 2026.8.4 (commit: abc1234, built: 2026-08-04T00:00:00Z)",
			want: "2026.8.4",
		},
		{
			name: "legacy v-prefix",
			in:   "elasticclaw v0.1.0 (commit: abc, built: unknown)",
			want: "0.1.0",
		},
		{
			name: "config file noise on stderr-style lines",
			in:   "Using config file: /root/.elasticclaw/config.yaml\nelasticclaw 2026.8.8-beta.2 (commit: x, built: y)\n",
			want: "2026.8.8-beta.2",
		},
		{
			name: "json",
			in:   `{"version":"2026.8.8-beta.2","commit":"4125c5f","buildDate":"2026-08-08T12:35:51Z"}`,
			want: "2026.8.8-beta.2",
		},
		{
			name: "empty",
			in:   "",
			want: "unknown",
		},
		{
			name: "unrelated",
			in:   "command not found",
			want: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parseVersionFromOutput(tc.in); got != tc.want {
				t.Fatalf("parseVersionFromOutput(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSameReleaseVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.0", "v0.1.0", true},
		{"v0.1.0", "v0.1.0", true},
		{"2026.8.8-beta.2", "2026.8.8-beta.2", true},
		{"v2026.8.8-beta.2", "2026.8.8-beta.2", true},
		{"0.1.0", "0.1.1", false},
		{"unknown", "v0.1.0", false},
		{"", "v0.1.0", false},
		{"0.1.0", "unknown", false},
	}
	for _, tc := range cases {
		name := tc.a + " vs " + tc.b
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := sameReleaseVersion(tc.a, tc.b); got != tc.want {
				t.Fatalf("sameReleaseVersion(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestHubUpgradeScriptGuardsDisk(t *testing.T) {
	cases := []struct {
		name    string
		freeKB  string
		wantOut string
	}{
		{name: "refuses when /tmp is nearly full", freeKB: "1024", wantOut: "Refusing to upgrade"},
		{name: "removes partial download on curl failure", freeKB: "10000000", wantOut: "Downloading"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			writeStub(t, bin, "df", "echo header; echo \"fs 0 0 "+tc.freeKB+" 0% /tmp\"")
			writeStub(t, bin, "curl", "echo partial > /tmp/elasticclaw-new; exit 23")

			cmd := exec.Command("bash", "-c", hubUpgradeScript("v1", "https://example.invalid", false))
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("script succeeded, want failure; output:\n%s", out)
			}
			if !strings.Contains(string(out), tc.wantOut) {
				t.Fatalf("output missing %q:\n%s", tc.wantOut, out)
			}
			if _, statErr := os.Stat("/tmp/elasticclaw-new"); !os.IsNotExist(statErr) {
				t.Fatalf("/tmp/elasticclaw-new left behind (stat err: %v)", statErr)
			}
		})
	}
}

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
