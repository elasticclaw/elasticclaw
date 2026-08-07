package hub

import "testing"

func TestSettingsWorkspaceStaticPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		// Existing HTML mappings (must not regress).
		{"workspace root", "settings/eng", "settings/_workspace/index.html", true},
		{"workspace root trailing slash", "settings/eng/", "settings/_workspace/index.html", true},
		{"workspace section", "settings/eng/github", "settings/_workspace/github/index.html", true},
		{"workspace section leading slash", "/settings/eng/github", "settings/_workspace/github/index.html", true},

		// New RSC payload mappings.
		{"workspace payload", "settings/eng/__next._tree.txt", "settings/_workspace/__next._tree.txt", true},
		{"workspace payload full", "settings/elasticclaw-test/__next._full.txt", "settings/_workspace/__next._full.txt", true},
		{"workspace section payload", "settings/eng/github/__next._tree.txt", "settings/_workspace/github/__next._tree.txt", true},
		{"workspace section payload page", "settings/elasticclaw-test/models/__next.settings.$oc$parts.__PAGE__.txt", "settings/_workspace/models/__next.settings.$oc$parts.__PAGE__.txt", true},

		// Non-mappings.
		{"underscore workspace", "settings/_workspace/github/index.html", "", false},
		{"underscore workspace payload", "settings/_workspace/github/__next._tree.txt", "", false},
		{"first segment is a section", "settings/models/__next._tree.txt", "", false},
		{"section as workspace", "settings/models", "", false},
		{"unknown section", "settings/eng/nonsense", "", false},
		{"unknown section payload", "settings/eng/nonsense/__next._tree.txt", "", false},
		{"non-payload final segment", "settings/eng/github/style.css", "", false},
		{"payload prefix only", "settings/eng/github/__next.js", "", false},
		{"txt without prefix", "settings/eng/github/notes.txt", "", false},
		{"outside settings", "sessions/eng/__next._tree.txt", "", false},
		{"root", "", "", false},
		{"settings root", "settings", "", false},
		{"too deep", "settings/eng/github/extra/__next._tree.txt", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := settingsWorkspaceStaticPath(tt.in)
			if got != tt.want || ok != tt.ok {
				t.Errorf("settingsWorkspaceStaticPath(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.ok)
			}
		})
	}
}
