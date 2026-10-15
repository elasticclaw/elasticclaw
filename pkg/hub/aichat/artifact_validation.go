package aichat

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/internal/readhttp"
	"golang.org/x/net/html"
)

const maxArtifactBytes = 2 * 1024 * 1024

var errArtifactSize = errors.New("The page is over the 2 MB limit. Nothing was published.")

type artifactHostsError []string

func (e artifactHostsError) Error() string {
	return "The page referenced hosts outside the allowlist: " + strings.Join(e, ", ") + "."
}

var cssComments = regexp.MustCompile(`(?s)/\*.*?\*/`)
var cssEscapes = regexp.MustCompile(`\\([0-9a-fA-F]{1,6}[\t\n\r\f ]?|[^\n\r\f])`)
var cssURLs = regexp.MustCompile(`(?is)url\(\s*(?:"([^"]*)"|'([^']*)'|([^)]*))\s*\)|@import\s+(?:"([^"]*)"|'([^']*)')`)

func stripHTMLFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") && strings.HasSuffix(s, "```") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = strings.TrimSpace(s[i+1 : len(s)-3])
		}
	}
	return s
}

// Validation constrains static resource references; sandbox and CSP remain the
// security boundary for arbitrary JavaScript, including dynamically built URLs.
func validateArtifact(s string) error {
	if len(s) > maxArtifactBytes {
		return errArtifactSize
	}
	lower := strings.ToLower(strings.TrimSpace(s))
	if !(strings.HasPrefix(lower, "<!doctype html>") || strings.HasPrefix(lower, "<html")) || !strings.Contains(lower, "</html>") {
		return errors.New("The model did not return a complete HTML document.")
	}
	bad := map[string]bool{}
	check := func(value string) {
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		// Browsers remove embedded ASCII tabs/newlines when parsing URL schemes.
		value = strings.Map(func(r rune) rune {
			if r == '\t' || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, value)
		if value == "" || strings.HasPrefix(value, "#") || strings.HasPrefix(strings.ToLower(value), "data:") {
			return
		}
		u, err := url.Parse(value)
		if err == nil && strings.EqualFold(u.Scheme, "https") && u.User == nil && u.Port() == "" {
			switch strings.ToLower(u.Hostname()) {
			case "cdnjs.cloudflare.com", "cdn.jsdelivr.net", "fonts.googleapis.com", "fonts.gstatic.com":
				return
			}
		}
		host := "relative URL"
		if err == nil {
			if u.Host != "" {
				host = u.Host
			} else if u.Scheme != "" {
				host = u.Scheme + ":"
			}
		} else {
			host = "invalid URL"
		}
		bad[host] = true
	}
	css := func(value string) {
		value = strings.NewReplacer("\\\r\n", "", "\\\n", "", "\\\r", "", "\\\f", "").Replace(value)
		value = cssComments.ReplaceAllString(value, "")
		value = cssEscapes.ReplaceAllStringFunc(value, func(s string) string {
			hex := strings.TrimSpace(s[1:])
			if n, err := strconv.ParseInt(hex, 16, 32); err == nil {
				return string(rune(n))
			}
			return s[1:]
		})
		for _, match := range cssURLs.FindAllStringSubmatch(value, -1) {
			for _, v := range match[1:] {
				if v != "" {
					check(v)
					break
				}
			}
		}
	}
	z := html.NewTokenizer(strings.NewReader(s))
	inStyle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			if z.Err() != io.EOF {
				return fmt.Errorf("The HTML document could not be read.")
			}
			if len(bad) > 0 {
				hosts := make(artifactHostsError, 0, len(bad))
				for h := range bad {
					hosts = append(hosts, h)
				}
				sort.Strings(hosts)
				return hosts
			}
			return nil
		case html.StartTagToken, html.SelfClosingTagToken:
			token := z.Token()
			inStyle = token.Data == "style"
			attrs := map[string]string{}
			for _, a := range token.Attr {
				attrs[a.Key] = a.Val
				switch a.Key {
				case "src", "href", "action", "formaction", "poster", "data", "xlink:href":
					check(a.Val)
				case "srcset", "imagesrcset":
					// A candidate URL ends at whitespace. Data URLs may contain commas.
					rest := strings.TrimSpace(a.Val)
					for rest != "" {
						end := strings.IndexFunc(rest, unicode.IsSpace)
						if end < 0 {
							end = len(rest)
						}
						candidate := rest[:end]
						if !strings.HasPrefix(strings.ToLower(candidate), "data:") {
							for _, u := range strings.Split(candidate, ",") {
								check(u)
							}
						} else {
							check(strings.TrimRight(candidate, ","))
						}
						rest = rest[end:]
						if strings.HasSuffix(candidate, ",") {
							rest = strings.TrimSpace(rest)
							continue
						}
						if comma := strings.IndexByte(rest, ','); comma >= 0 {
							rest = strings.TrimSpace(rest[comma+1:])
						} else {
							rest = ""
						}
					}
				case "style":
					css(a.Val)
				}
			}
			if token.Data == "meta" && strings.EqualFold(strings.TrimSpace(attrs["http-equiv"]), "refresh") {
				return errors.New("Meta refresh is not allowed in artifacts.")
			}
		case html.TextToken:
			if inStyle {
				css(string(z.Text()))
			}
		case html.EndTagToken:
			if z.Token().Data == "style" {
				inStyle = false
			}
		}
	}
}

func artifactDocumentTitle(document string) string {
	z := html.NewTokenizer(strings.NewReader(document))
	inTitle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken:
			inTitle = z.Token().Data == "title"
		case html.TextToken:
			if inTitle {
				title := []rune(strings.Join(strings.Fields(z.Token().Data), " "))
				if len(title) > 80 {
					title = title[:80]
				}
				return string(title)
			}
		}
	}
}

// Keep original tokens unless decoding entities reveals another secret. This
// preserves script/style text while also redacting rendered HTML attributes and text.
func redactArtifactHTML(document string, secrets []string) string {
	document = readhttp.Redact(document, secrets)
	if len(secrets) == 0 {
		return document
	}
	z := html.NewTokenizer(strings.NewReader(document))
	var out strings.Builder
	for {
		kind := z.Next()
		raw := string(z.Raw())
		if kind == html.ErrorToken {
			out.WriteString(raw)
			return out.String()
		}
		token := z.Token()
		data := readhttp.Redact(token.Data, secrets)
		changed := data != token.Data
		token.Data = data
		for i, attr := range token.Attr {
			value := readhttp.Redact(attr.Val, secrets)
			if value != attr.Val {
				token.Attr[i].Val = value
				changed = true
			}
		}
		if changed {
			out.WriteString(token.String())
		} else {
			out.WriteString(raw)
		}
	}
}
