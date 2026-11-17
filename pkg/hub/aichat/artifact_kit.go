package aichat

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/tools"
	"golang.org/x/net/html"
)

const (
	maxDesignSystemBytes = 200 * 1024
	maxTemplateBytes     = 50 * 1024
	maxTemplates         = 20
)

const errIncompleteKitFile = "The file must be a complete HTML document, from <!doctype html> to </html>."

var templateIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
var templateSlotPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)
var designSystemClasses = regexp.MustCompile(`\.(-?[_a-zA-Z][_a-zA-Z0-9-]*)`)

type DesignSystem struct {
	// CSS joins Styles; each <style data-ds> block is injected separately so a
	// leading @import stays valid.
	CSS       string
	Styles    []string
	FontLinks []string
	Guide     string
	SHA256    string
}
type TemplateSlot struct {
	ID, Hint         string
	Repeat, Optional bool
}
type ArtifactTemplate struct {
	ID, Name, Description, When string
	HTML                        string
	Slots                       []TemplateSlot
}
type ArtifactKit struct {
	DesignSystem       *DesignSystem
	DesignSystemStatus string
	DesignSystemError  string
	Templates          []ArtifactTemplate
	Warnings           []string
}
type ArtifactKitInfo struct {
	DesignSystem DesignSystemInfo `json:"designSystem"`
	Templates    []TemplateInfo   `json:"templates"`
	Warnings     []string         `json:"warnings"`
}
type DesignSystemInfo struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}
type TemplateInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	When        string `json:"when,omitempty"`
	PreviewURL  string `json:"previewUrl,omitempty"`
}

func (k ArtifactKit) Template(id string) (ArtifactTemplate, bool) {
	for _, t := range k.Templates {
		if t.ID == id {
			return t, true
		}
	}
	return ArtifactTemplate{}, false
}
func (k ArtifactKit) TemplateOptions() []tools.TemplateOption {
	options := make([]tools.TemplateOption, 0, len(k.Templates))
	for _, t := range k.Templates {
		options = append(options, tools.TemplateOption{ID: t.ID, Name: t.Name, When: t.When})
	}
	return options
}
func (k ArtifactKit) Info() ArtifactKitInfo {
	status := k.DesignSystemStatus
	if status == "" {
		status = "absent"
	}
	info := ArtifactKitInfo{DesignSystem: DesignSystemInfo{Status: status, Error: k.DesignSystemError}, Templates: []TemplateInfo{}, Warnings: append([]string{}, k.Warnings...)}
	for _, t := range k.Templates {
		info.Templates = append(info.Templates, TemplateInfo{ID: t.ID, Name: t.Name, Description: t.Description, When: t.When})
	}
	return info
}

func readKitFile(path string, limit int) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("only regular files are allowed")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	return bytes.TrimPrefix(raw, []byte("\uFEFF")), err
}

func LoadArtifactKit(managedDir string) ArtifactKit {
	k := ArtifactKit{DesignSystemStatus: "absent", Templates: []ArtifactTemplate{}, Warnings: []string{}}
	warn := func(s string) { k.Warnings = append(k.Warnings, s); log.Printf("ai chat artifact kit: %s", s) }
	root := filepath.Join(managedDir, "ai_chat")
	if info, err := os.Lstat(root); os.IsNotExist(err) {
		return k
	} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		k.DesignSystemStatus, k.DesignSystemError = "invalid", "Unable to read ai_chat/design-system.html."
		log.Printf("ai chat artifact kit: %s", k.DesignSystemError)
		warn("Unable to read ai_chat/templates.")
		return k
	}
	raw, err := readKitFile(filepath.Join(root, "design-system.html"), maxDesignSystemBytes)
	if err == nil {
		k.DesignSystem, err = parseDesignSystem(raw)
		if err == nil {
			k.DesignSystemStatus = "ok"
		}
	}
	if err != nil && !os.IsNotExist(err) {
		k.DesignSystemStatus = "invalid"
		if raw == nil {
			k.DesignSystemError = "Unable to read ai_chat/design-system.html."
		} else {
			k.DesignSystemError = err.Error()
		}
		// Reported through DesignSystemError; Warnings are for templates.
		log.Printf("ai chat artifact kit: %s", k.DesignSystemError)
	}
	dir := filepath.Join(root, "templates")
	if info, err := os.Lstat(dir); os.IsNotExist(err) {
		return k
	} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		warn("Unable to read ai_chat/templates.")
		return k
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		warn("Unable to read ai_chat/templates.")
		return k
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".html") {
			continue
		}
		id := strings.TrimSuffix(name, ".html")
		prefix := "ai_chat/templates/" + name + ": "
		if !templateIDPattern.MatchString(id) {
			warn(prefix + "skipped, the file name must match ^[a-z0-9][a-z0-9-]{0,39}$.")
			continue
		}
		if id == "auto" || id == "free" {
			warn(prefix + "skipped, auto and free are reserved template choices.")
			continue
		}
		raw, err := readKitFile(filepath.Join(dir, name), maxTemplateBytes)
		if err != nil {
			warn(prefix + "Unable to read a regular template file. It is not offered.")
			continue
		}
		tpl, err := parseTemplate(id, raw)
		if err != nil {
			warn(prefix + strings.TrimSuffix(err.Error(), ".") + ". It is not offered.")
			continue
		}
		if len(k.Templates) == maxTemplates {
			warn(prefix + "skipped, only 20 templates are offered.")
			continue
		}
		k.Templates = append(k.Templates, tpl)
	}
	return k
}

func nodeAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}
func walkHTML(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkHTML(c, visit)
	}
}
func nodeText(n *html.Node) string {
	var b strings.Builder
	walkHTML(n, func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
	})
	return b.String()
}
func compactText(s string) string { return strings.Join(strings.Fields(s), " ") }
func capBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	end := max - len("…")
	for !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

func parseDesignSystem(raw []byte) (*DesignSystem, error) {
	if len(raw) > maxDesignSystemBytes {
		return nil, errors.New("ai_chat/design-system.html is over the 200 KB limit.")
	}
	if err := validateArtifact(string(raw)); errors.Is(err, errIncompleteArtifact) {
		return nil, errors.New("ai_chat/design-system.html: " + errIncompleteKitFile)
	} else if err != nil {
		return nil, fmt.Errorf("ai_chat/design-system.html: %w", err)
	}
	doc, err := html.Parse(strings.NewReader(string(raw)))
	if err != nil {
		return nil, errors.New("Unable to parse ai_chat/design-system.html.")
	}
	ds := &DesignSystem{}
	walkHTML(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if _, ok := nodeAttr(n, "data-ds"); n.Data == "style" && ok {
			css := nodeText(n) + "\n"
			ds.Styles = append(ds.Styles, css)
			ds.CSS += css
		}
		if n.Data == "link" {
			rel, _ := nodeAttr(n, "rel")
			stylesheet := false
			for _, token := range strings.Fields(rel) {
				if strings.EqualFold(token, "stylesheet") {
					stylesheet = true
				}
			}
			href, _ := nodeAttr(n, "href")
			u, err := url.Parse(href)
			if stylesheet && err == nil && strings.EqualFold(u.Scheme, "https") && u.User == nil && u.Port() == "" && strings.EqualFold(u.Host, "fonts.googleapis.com") {
				ds.FontLinks = append(ds.FontLinks, href)
			}
		}
	})
	if strings.TrimSpace(ds.CSS) == "" {
		return nil, errors.New("ai_chat/design-system.html has no <style data-ds> block.")
	}
	if strings.Contains(strings.ToLower(ds.CSS), "</style") {
		return nil, errors.New("ai_chat/design-system.html CSS contains a closing style tag.")
	}
	ds.Guide = designSystemGuide(doc, ds.CSS)
	sum := sha256.Sum256(raw)
	ds.SHA256 = hex.EncodeToString(sum[:])
	return ds, nil
}

func designSystemGuide(doc *html.Node, css string) string {
	classes := []string{}
	seen := map[string]bool{}
	for _, match := range designSystemClasses.FindAllStringSubmatch(css, -1) {
		if !seen[match[1]] && len(classes) < 150 {
			classes = append(classes, match[1])
			seen[match[1]] = true
		}
	}
	guide := "Classes: " + strings.Join(classes, ", ")
	count := 0
	walkHTML(doc, func(n *html.Node) {
		name, ok := nodeAttr(n, "data-ds-component")
		name = compactText(name)
		if !ok || name == "" || utf8.RuneCountInString(name) > 60 || count >= 40 {
			return
		}
		count++
		usage, _ := nodeAttr(n, "data-ds-usage")
		runes := []rune(compactText(usage))
		if len(runes) > 300 {
			runes = runes[:300]
		}
		var example strings.Builder
		_ = html.Render(&example, n)
		guide += "\n- " + name + ": " + string(runes) + " Example: " + capBytes(compactText(example.String()), 800)
	})
	return capBytes(guide, 16*1024)
}

func parseTemplate(id string, raw []byte) (ArtifactTemplate, error) {
	t := ArtifactTemplate{ID: id, HTML: string(raw)}
	if len(raw) > maxTemplateBytes {
		return t, errors.New("Template is over the 50 KB limit.")
	}
	if err := validateArtifact(t.HTML); errors.Is(err, errIncompleteArtifact) {
		return t, errors.New(errIncompleteKitFile)
	} else if err != nil {
		return t, err
	}
	doc, err := html.Parse(strings.NewReader(t.HTML))
	if err != nil {
		return t, errors.New("Unable to parse template.")
	}
	styles := 0
	ids := map[string]bool{}
	var invalid error
	walkHTML(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if n.Data == "title" && t.Name == "" {
			t.Name = compactText(nodeText(n))
		}
		if n.Data == "meta" {
			name, _ := nodeAttr(n, "name")
			value, _ := nodeAttr(n, "content")
			switch strings.ToLower(name) {
			case "template:description":
				t.Description = compactText(value)
			case "template:when":
				t.When = compactText(value)
			}
		}
		if n.Data == "style" {
			_, layout := nodeAttr(n, "data-template-layout")
			styles++
			if !layout {
				invalid = errors.New("Only <style data-template-layout> is allowed.")
			} else if styles > 1 {
				invalid = errors.New("Only one <style data-template-layout> is allowed.")
			}
		}
		if id, ok := nodeAttr(n, "data-slot"); ok {
			hint, _ := nodeAttr(n, "data-slot-hint")
			_, repeat := nodeAttr(n, "data-slot-repeat")
			_, optional := nodeAttr(n, "data-slot-optional")
			switch {
			case !templateSlotPattern.MatchString(id):
				invalid = errors.New("Invalid data-slot id.")
			case ids[id]:
				invalid = errors.New("Duplicate data-slot id.")
			case utf8.RuneCountInString(hint) > 300:
				invalid = errors.New("data-slot-hint exceeds 300 characters.")
			}
			ids[id] = true
			t.Slots = append(t.Slots, TemplateSlot{ID: id, Hint: hint, Repeat: repeat, Optional: optional})
		}
	})
	if invalid != nil {
		return t, invalid
	}
	switch {
	case t.Name == "" || utf8.RuneCountInString(t.Name) > 60:
		return t, errors.New("Template title must contain 1–60 characters.")
	case t.Description == "" || utf8.RuneCountInString(t.Description) > 200:
		return t, errors.New("Template description must contain 1–200 characters.")
	case utf8.RuneCountInString(t.When) > 300:
		return t, errors.New("template:when exceeds 300 characters.")
	case len(t.Slots) == 0:
		return t, errors.New("Template requires at least one data-slot.")
	}
	return t, nil
}

func injectDesignSystem(document string, ds *DesignSystem) string {
	if ds == nil {
		return document
	}
	styles := ds.Styles
	if len(styles) == 0 {
		styles = []string{ds.CSS}
	}
	block := ""
	for _, css := range styles {
		block += "<style data-ds-injected>" + css + "</style>"
	}
	for _, href := range ds.FontLinks {
		block += `<link rel="stylesheet" href="` + html.EscapeString(href) + `" data-ds-injected>`
	}
	z := html.NewTokenizer(strings.NewReader(document))
	offset, fallback, head := 0, 0, -1
	for {
		kind := z.Next()
		offset += len(z.Raw())
		if kind == html.ErrorToken {
			break
		}
		token := z.Token()
		if head >= 0 {
			if kind == html.TextToken && strings.TrimSpace(token.Data) == "" {
				continue
			}
			if (kind == html.StartTagToken || kind == html.SelfClosingTagToken) && token.Data == "meta" {
				n := &html.Node{Attr: token.Attr}
				_, charset := nodeAttr(n, "charset")
				equiv, _ := nodeAttr(n, "http-equiv")
				if charset || strings.EqualFold(equiv, "content-type") {
					head = offset
				}
			}
			return document[:head] + block + document[head:]
		}
		if kind == html.DoctypeToken && fallback == 0 {
			fallback = offset
		}
		if kind != html.StartTagToken && kind != html.SelfClosingTagToken {
			continue
		}
		switch token.Data {
		case "html":
			fallback = offset
		case "head":
			head = offset
		case "base", "link", "meta", "title", "style", "script", "noscript", "template":
		default:
			return document[:fallback] + "<head>" + block + "</head>" + document[fallback:]
		}
	}
	if head >= 0 {
		return document[:head] + block + document[head:]
	}
	return document[:fallback] + "<head>" + block + "</head>" + document[fallback:]
}

func stripInjectedDesignSystem(document string) string {
	z := html.NewTokenizer(strings.NewReader(document))
	var out strings.Builder
	skip := false
	for {
		kind := z.Next()
		raw := string(z.Raw())
		if kind == html.ErrorToken {
			if !skip {
				out.WriteString(raw)
			}
			return out.String()
		}
		token := z.Token()
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			_, injected := nodeAttr(&html.Node{Attr: token.Attr}, "data-ds-injected")
			if injected && token.Data == "style" {
				skip = kind != html.SelfClosingTagToken
				continue
			}
			if injected && token.Data == "link" {
				continue
			}
		}
		if skip {
			if kind == html.EndTagToken && token.Data == "style" {
				skip = false
			}
			continue
		}
		out.WriteString(raw)
	}
}

type templateChoice struct {
	Template *ArtifactTemplate
	// Free marks an explicit "free" choice, which a retry must repeat.
	Free   bool
	Notice string
}

func resolveTemplate(kit ArtifactKit, pin, model, inherited string) templateChoice {
	if pin == "auto" {
		pin = ""
	}
	if model == "auto" {
		model = ""
	}
	requested := pin
	if requested == "" {
		requested = model
	}
	if requested == "" {
		requested = inherited
	}
	if requested == "" || requested == "free" {
		return templateChoice{Free: requested == "free"}
	}
	if t, ok := kit.Template(requested); ok {
		return templateChoice{Template: &t}
	}
	runes := []rune(requested)
	if len(runes) > 40 {
		runes = runes[:40]
	}
	return templateChoice{Notice: fmt.Sprintf("Template %q is not available, so this page was built without a template.", string(runes))}
}
