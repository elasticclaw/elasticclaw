package aichat

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func fixtureKit(t *testing.T) ArtifactKit {
	t.Helper()
	k := LoadArtifactKit("testdata/artifact-kit")
	if k.DesignSystemStatus != "ok" || len(k.Templates) != 2 || len(k.Warnings) != 0 {
		t.Fatalf("kit = %+v", k)
	}
	return k
}
func copyKit(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir("testdata/artifact-kit", func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("testdata/artifact-kit", path)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestArtifactKitFixture(t *testing.T) {
	k := fixtureKit(t)
	if k.Templates[0].ID != "one-pager" || k.Templates[1].ID != "status-report" {
		t.Fatal(k.Templates)
	}
	ds := k.DesignSystem
	for _, s := range []string{"Classes: ds-page, ds-card, ds-btn", "- card: Use for grouped content.", "Example:", "- table:"} {
		if !strings.Contains(ds.Guide, s) {
			t.Fatal(ds.Guide)
		}
	}
	if strings.Contains(ds.CSS, "docs-chrome") || len(ds.FontLinks) != 1 || !strings.Contains(ds.FontLinks[0], "&display=swap") {
		t.Fatal(ds)
	}
	raw, err := os.ReadFile("testdata/artifact-kit/ai_chat/design-system.html")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if ds.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal(ds.SHA256)
	}
	tpl := k.Templates[0]
	if !tpl.Slots[1].Repeat || !tpl.Slots[len(tpl.Slots)-1].Optional {
		t.Fatal(tpl.Slots)
	}
	if len(k.TemplateOptions()) != 2 || len(k.Info().Templates) != 2 {
		t.Fatal(k)
	}
}
func TestDesignSystemInvalid(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"size", strings.Repeat("x", maxDesignSystemBytes+1), "200 KB"},
		{"incomplete", "<style data-ds>.a{}</style>", "complete HTML"},
		{"host", `<!doctype html><html><style data-ds>.a{background:url(https://evil.test/a)}</style></html>`, "allowlist"},
		{"refresh", `<!doctype html><html><meta http-equiv="refresh" content="0"><style data-ds>.a{}</style></html>`, "refresh"},
		{"missing", testHTML, "no <style data-ds>"},
		{"empty", `<!doctype html><html><style data-ds>  </style></html>`, "no <style data-ds>"},
		{"closing style", `<!doctype html><html><style data-ds>.a{content:"</stylex>"}</style></html>`, "closing style"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseDesignSystem([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal(err)
			}
		})
	}
}
func TestDesignSystemGuideLimits(t *testing.T) {
	var raw strings.Builder
	raw.WriteString(`<!doctype html><html><style data-ds>.only{}</style><body>`)
	raw.WriteString(`<div data-ds-component="` + strings.Repeat("a", 61) + `">Ignored</div>`)
	for i := 0; i < 45; i++ {
		fmt.Fprintf(&raw, `<div data-ds-component="c%d" data-ds-usage="%s">%s</div>`, i, strings.Repeat("語", 310), strings.Repeat("語", 400))
	}
	raw.WriteString(`</body></html>`)
	ds, err := parseDesignSystem([]byte(raw.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Guide) > 16*1024 || !utf8.ValidString(ds.Guide) || strings.Contains(ds.Guide, "Ignored") || strings.Contains(ds.Guide, "c40:") {
		t.Fatal(len(ds.Guide))
	}
	ds, err = parseDesignSystem([]byte(`<!doctype html><html><style data-ds>.one{}.one{}.two{}</style></html>`))
	if err != nil || ds.Guide != "Classes: one, two" {
		t.Fatal(ds, err)
	}
}
func TestTemplateInvalid(t *testing.T) {
	raw, err := os.ReadFile("testdata/artifact-kit/ai_chat/templates/one-pager.html")
	if err != nil {
		t.Fatal(err)
	}
	valid := string(raw)
	for _, tc := range []struct{ name, raw, want string }{
		{"size", strings.Repeat("x", maxTemplateBytes+1), "50 KB"},
		{"incomplete", "<html>", "complete HTML"},
		{"host", strings.Replace(valid, "</head>", `<script src="https://evil.test/a"></script></head>`, 1), "allowlist"},
		{"refresh", strings.Replace(valid, "</head>", `<meta http-equiv="refresh" content="0"></head>`, 1), "refresh"},
		{"missing title", strings.Replace(valid, "<title>One-pager</title>", "", 1), "title"},
		{"blank title", strings.Replace(valid, "One-pager", "  ", 1), "title"},
		{"long title", strings.Replace(valid, "One-pager", strings.Repeat("語", 61), 1), "title"},
		{"missing description", strings.Replace(valid, "template:description", "other", 1), "description"},
		{"blank description", strings.Replace(valid, "A concise proposal with evidence, recommendations and next steps.", "  ", 1), "description"},
		{"long description", strings.Replace(valid, "A concise proposal with evidence, recommendations and next steps.", strings.Repeat("a", 201), 1), "description"},
		{"long when", strings.Replace(valid, "Use for a proposal, executive brief or focused product idea.", strings.Repeat("a", 301), 1), "template:when"},
		{"other style", strings.Replace(valid, "data-template-layout", "", 1), "Only <style"},
		{"two styles", strings.Replace(valid, "</head>", "<style data-template-layout>.a{}</style></head>", 1), "Only one"},
		{"no slots", strings.ReplaceAll(valid, "data-slot=", "other="), "at least one"},
		{"invalid slot", strings.Replace(valid, `data-slot="summary"`, `data-slot="Bad slot"`, 1), "Invalid data-slot"},
		{"duplicate slot", strings.Replace(valid, `data-slot="metric"`, `data-slot="summary"`, 1), "Duplicate"},
		{"hint", strings.Replace(valid, "State the title and concise proposal.", strings.Repeat("a", 301), 1), "data-slot-hint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseTemplate("one-pager", []byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal(err)
			}
		})
	}
	normalized := strings.Replace(valid, "<title>One-pager</title>", "<title> One-\n pager </title>", 1)
	tpl, err := parseTemplate("one-pager", []byte(normalized))
	if err != nil || tpl.Name != "One- pager" {
		t.Fatal(tpl, err)
	}
}
func TestArtifactKitLoading(t *testing.T) {
	dir := t.TempDir()
	k := LoadArtifactKit(dir)
	if k.DesignSystemStatus != "absent" || len(k.Warnings) != 0 || k.Info().Templates == nil || k.Info().Warnings == nil {
		t.Fatal(k)
	}
	copyKit(t, dir)
	templates := filepath.Join(dir, "ai_chat", "templates")
	raw, _ := os.ReadFile(filepath.Join(templates, "one-pager.html"))
	for i := 0; i < 22; i++ {
		if err := os.WriteFile(filepath.Join(templates, fmt.Sprintf("extra-%02d.html", i)), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"Bad Name.html", "bad.html"} {
		if err := os.WriteFile(filepath.Join(templates, name), []byte("bad"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(templates, "one-pager.html"), filepath.Join(templates, "linked.html")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(templates, "directory.html"), 0700); err != nil {
		t.Fatal(err)
	}
	k = LoadArtifactKit(dir)
	warnings := strings.Join(k.Warnings, "\n")
	for _, want := range []string{"Bad Name.html: skipped, the file name", "only 20 templates", "bad.html:", "linked.html:", "directory.html:"} {
		if !strings.Contains(warnings, want) {
			t.Fatal(warnings)
		}
	}
	if len(k.Templates) != 20 {
		t.Fatal(len(k.Templates))
	}
	dsPath := filepath.Join(dir, "ai_chat", "design-system.html")
	if err := os.WriteFile(dsPath, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	k = LoadArtifactKit(dir)
	if k.DesignSystemStatus != "invalid" || k.DesignSystem != nil || k.DesignSystemError == "" {
		t.Fatal(k)
	}
	if err := os.Remove(dsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(templates, "one-pager.html"), dsPath); err != nil {
		t.Fatal(err)
	}
	k = LoadArtifactKit(dir)
	if k.DesignSystemStatus != "invalid" || strings.Contains(k.DesignSystemError, dir) {
		t.Fatal(k)
	}
}
func TestDesignSystemInjection(t *testing.T) {
	ds := &DesignSystem{CSS: ".a{}", FontLinks: []string{"https://fonts.googleapis.com/css2?a=1&b=2"}}
	block := `<style data-ds-injected>.a{}</style><link rel="stylesheet" href="https://fonts.googleapis.com/css2?a=1&amp;b=2" data-ds-injected>`
	for _, tc := range []struct{ name, input, want string }{
		{"head", testHTML, strings.Replace(testHTML, "<head>", "<head>"+block, 1)},
		{"charset", `<!doctype html><html><head> 
<meta charset="utf-8"><title>A</title></head><body></body></html>`, `<!doctype html><html><head> 
<meta charset="utf-8">` + block + `<title>A</title></head><body></body></html>`},
		{"content type", `<html><head><meta http-equiv="Content-Type" content="text/html; charset=utf-8"></head></html>`, `<html><head><meta http-equiv="Content-Type" content="text/html; charset=utf-8">` + block + `</head></html>`},
		{"no head", `<!doctype html><html lang="en"><body>A</body></html>`, `<!doctype html><html lang="en"><head>` + block + `</head><body>A</body></html>`},
		{"doctype", `<!doctype html><body>A</body></html>`, `<!doctype html><head>` + block + `</head><body>A</body></html>`},
		{"fragment", `<div>A</div>`, `<head>` + block + `</head><div>A</div>`},
		{"late head", `<html><body><head></head></body></html>`, `<html><head>` + block + `</head><body><head></head></body></html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := injectDesignSystem(tc.input, ds); got != tc.want {
				t.Fatalf("got %s\nwant %s", got, tc.want)
			}
		})
	}
	injected := injectDesignSystem(testHTML, ds)
	if err := validateArtifact(injected); err != nil {
		t.Fatal(err)
	}
	if got := stripInjectedDesignSystem(injected); got != testHTML {
		t.Fatal(got)
	}
	if got := injectDesignSystem(stripInjectedDesignSystem(injected), ds); got != injected {
		t.Fatal(got)
	}
	other := `<html><head><style>.other{}</style><link href="#local"></head><body> &#65; </body></html>`
	if stripInjectedDesignSystem(injectDesignSystem(other, ds)) != other || injectDesignSystem(other, nil) != other {
		t.Fatal("changed original bytes")
	}
}
func TestResolveTemplate(t *testing.T) {
	k := fixtureKit(t)
	for _, tc := range []struct {
		name, pin, model, inherited, want string
		notice                            bool
	}{
		{"pin", "one-pager", "status-report", "status-report", "one-pager", false},
		{"model", "", "one-pager", "status-report", "one-pager", false},
		{"inherited", "", "", "status-report", "status-report", false},
		{"free", "free", "one-pager", "status-report", "", false},
		{"unknown pin", "old", "one-pager", "status-report", "", true},
		{"unknown inherited", "", "", "old", "", true},
		{"empty", "", "", "", "", false},
		{"auto", "auto", "one-pager", "status-report", "one-pager", false},
		{"model auto", "", "auto", "status-report", "status-report", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := resolveTemplate(k, tc.pin, tc.model, tc.inherited)
			got := ""
			if c.Template != nil {
				got = c.Template.ID
			}
			if got != tc.want || (c.Notice != "") != tc.notice {
				t.Fatal(c)
			}
		})
	}
	if got := resolveTemplate(k, "old", "", "").Notice; got != `Template "old" is not available, so this page was built without a template.` {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(k.Info().Warnings, []string{}) {
		t.Fatal(k.Info())
	}
}

func TestDesignSystemCollectsOnlySharedStylesAndFonts(t *testing.T) {
	raw := `<!doctype html><html><head><style data-ds>.first{}</style><style>.docs{}</style><style data-ds>.second{}</style><link rel="alternate STYLESHEET" href="https://fonts.googleapis.com/css2?a=1&amp;b=2"><link rel="stylesheet" href="https://cdn.jsdelivr.net/layout.css"><link rel="preconnect" href="https://fonts.gstatic.com"></head><body></body></html>`
	ds, err := parseDesignSystem([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if ds.CSS != ".first{}\n.second{}\n" || !reflect.DeepEqual(ds.FontLinks, []string{"https://fonts.googleapis.com/css2?a=1&b=2"}) {
		t.Fatal(ds)
	}
}

func TestArtifactKitRejectsSymlinkDirectories(t *testing.T) {
	source := t.TempDir()
	copyKit(t, source)
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(source, "ai_chat"), filepath.Join(dir, "ai_chat")); err != nil {
		t.Fatal(err)
	}
	kit := LoadArtifactKit(dir)
	if kit.DesignSystemStatus != "invalid" || len(kit.Templates) != 0 || len(kit.Warnings) == 0 {
		t.Fatal(kit)
	}
	dir = t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "ai_chat"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, "ai_chat", "templates"), filepath.Join(dir, "ai_chat", "templates")); err != nil {
		t.Fatal(err)
	}
	kit = LoadArtifactKit(dir)
	if kit.DesignSystemStatus != "absent" || len(kit.Templates) != 0 || len(kit.Warnings) == 0 {
		t.Fatal(kit)
	}
}
