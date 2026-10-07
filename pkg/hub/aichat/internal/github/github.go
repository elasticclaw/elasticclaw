// Package github provides bounded, read-only GitHub requests for chat sources.
package github

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/elasticclaw/elasticclaw/pkg/hub/aichat/internal/readhttp"
)

const MaxFileBytes = 128 * 1024
const MaxTreeEntries = 5000

type Client struct {
	BaseURL string
	HTTP    *http.Client
	Token   func(string) string
}

type Entry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int    `json:"size"`
	SHA  string `json:"sha"`
	Mode string `json:"mode"`
}

func (c Client) Credential(repo string) string {
	if c.Token == nil {
		return ""
	}
	return c.Token(repo)
}

func (c Client) Redact(repo, value string) string {
	token := c.Credential(repo)
	if token != "" {
		value = strings.ReplaceAll(value, token, "[redacted]")
	}
	return value
}

func (c Client) Get(ctx context.Context, repo, endpoint string, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	token := c.Credential(repo)
	if token == "" {
		return errors.New("GitHub credentials are not available.")
	}
	base := c.BaseURL
	if base == "" {
		base = "https://api.github.com"
	}
	client := readhttp.Client{HTTP: c.HTTP, BaseURL: base, Headers: http.Header{"Authorization": {"Bearer " + token}, "Accept": {"application/vnd.github.text-match+json"}}, Secrets: []string{token}}
	return client.Do(ctx, http.MethodGet, endpoint, nil, out)
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func ValidRepo(repo string) bool {
	if !repoPattern.MatchString(repo) {
		return false
	}
	for _, part := range strings.Split(repo, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}
func ValidPath(value string, optional bool) bool {
	if value == "" {
		return optional
	}
	if len(value) > 2048 || value == "." || path.IsAbs(value) || path.Clean(value) != value || strings.ContainsAny(value, "\\\x00") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}
func ValidRef(ref string) bool {
	return len(ref) <= 256 && !strings.ContainsAny(ref, "\\\x00\r\n\t ?#") && !strings.HasPrefix(ref, "/") && !strings.Contains(ref, "..")
}
func RepoPath(repo string) string { return "/repos/" + repo }
func (c Client) DefaultBranch(ctx context.Context, repo string) (string, error) {
	var data struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.Get(ctx, repo, RepoPath(repo), &data); err != nil {
		return "", err
	}
	if data.DefaultBranch == "" || !ValidRef(data.DefaultBranch) {
		return "", errors.New("GitHub returned an invalid default branch.")
	}
	return data.DefaultBranch, nil
}
func (c Client) Commit(ctx context.Context, repo, ref string) (string, error) {
	if ref == "" {
		var err error
		ref, err = c.DefaultBranch(ctx, repo)
		if err != nil {
			return "", err
		}
	}
	if !ValidRef(ref) {
		return "", errors.New("Invalid repository reference.")
	}
	var data struct {
		SHA string `json:"sha"`
	}
	if err := c.Get(ctx, repo, RepoPath(repo)+"/commits/"+url.PathEscape(ref), &data); err != nil {
		return "", err
	}
	if !validSHA(data.SHA) {
		return "", errors.New("GitHub returned an invalid commit.")
	}
	return data.SHA, nil
}
func validSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (c Client) PullHead(ctx context.Context, repo string, pr int) (string, error) {
	var data struct {
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := c.Get(ctx, repo, RepoPath(repo)+"/pulls/"+strconv.Itoa(pr), &data); err != nil {
		return "", err
	}
	if !validSHA(data.Head.SHA) {
		return "", errors.New("GitHub returned an invalid pull request head.")
	}
	return data.Head.SHA, nil
}
func (c Client) Tree(ctx context.Context, repo, sha string) ([]Entry, error) {
	var data struct {
		Tree      []Entry `json:"tree"`
		Truncated bool    `json:"truncated"`
	}
	if err := c.Get(ctx, repo, RepoPath(repo)+"/git/trees/"+url.PathEscape(sha)+"?recursive=1", &data); err != nil {
		return nil, err
	}
	if data.Truncated || len(data.Tree) > MaxTreeEntries {
		return nil, errors.New("Repository tree exceeds the read limit.")
	}
	entries := make([]Entry, 0, len(data.Tree))
	for _, e := range data.Tree {
		if ValidPath(e.Path, false) {
			entries = append(entries, e)
		}
	}
	return entries, nil
}
func (c Client) Read(ctx context.Context, repo, file, ref string) (string, error) {
	if !ValidPath(file, false) {
		return "", errors.New("Invalid relative file path.")
	}
	var data struct {
		Content   string `json:"content"`
		Encoding  string `json:"encoding"`
		Type      string `json:"type"`
		Size      int    `json:"size"`
		Target    string `json:"target"`
		Submodule string `json:"submodule_git_url"`
	}
	endpoint := RepoPath(repo) + "/contents/" + url.PathEscape(file) + "?ref=" + url.QueryEscape(ref)
	if err := c.Get(ctx, repo, endpoint, &data); err != nil {
		return "", err
	}
	if data.Type != "file" || data.Target != "" || data.Submodule != "" || data.Encoding != "base64" {
		return "", errors.New("Only regular text files can be read.")
	}
	if data.Size > MaxFileBytes || len(data.Content) > 2*MaxFileBytes {
		return "", errors.New("File exceeds the read limit.")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(data.Content, "\n", ""))
	if err != nil || len(decoded) > MaxFileBytes {
		return "", errors.New("File content is invalid or exceeds the read limit.")
	}
	if !utf8.Valid(decoded) || strings.ContainsRune(string(decoded), 0) {
		return "", errors.New("Only text files can be read.")
	}
	return c.Redact(repo, string(decoded)), nil
}
