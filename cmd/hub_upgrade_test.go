package cmd

import "testing"

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
