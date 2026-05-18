// Package githubapi is a tiny client for the runner-management endpoints we need.
package githubapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	Token string
	HTTP  *http.Client
}

func New(token string) *Client {
	return &Client{Token: token, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

type registrationToken struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

type runnerList struct {
	TotalCount int      `json:"total_count"`
	Runners    []Runner `json:"runners"`
}

// `target` for the runner-management endpoints is the path prefix that scopes
// the call: "repos/owner/name" for a repo-level runner or "orgs/orgname" for
// an org-level runner. GitHub's two endpoint families are otherwise identical,
// so threading the prefix through is enough to support both.

// ListRunners returns every runner registered under `target` (up to 100).
func (c *Client) ListRunners(target string) ([]Runner, error) {
	var out runnerList
	if err := c.do(http.MethodGet, "/"+target+"/actions/runners?per_page=100", &out); err != nil {
		return nil, err
	}
	return out.Runners, nil
}

// RegistrationToken returns a short-lived token used to `config.sh` a new runner.
func (c *Client) RegistrationToken(target string) (string, error) {
	var out registrationToken
	if err := c.do(http.MethodPost, "/"+target+"/actions/runners/registration-token", &out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// FindRunner returns the runner with the given name, or (nil, nil) if absent.
func (c *Client) FindRunner(target, name string) (*Runner, error) {
	runners, err := c.ListRunners(target)
	if err != nil {
		return nil, err
	}
	for i := range runners {
		if runners[i].Name == name {
			return &runners[i], nil
		}
	}
	return nil, nil
}

func (c *Client) DeleteRunner(target string, id int64) error {
	return c.do(http.MethodDelete, fmt.Sprintf("/%s/actions/runners/%d", target, id), nil)
}

// WhoAmI checks that the token is accepted by GitHub at all. Doesn't prove
// any particular scope — just that the token isn't expired or revoked.
func (c *Client) WhoAmI() error {
	return c.do(http.MethodGet, "/user", nil)
}

// ErrNotFound is returned when GetFile cannot locate the requested path.
var ErrNotFound = fmt.Errorf("not found")

type contentsResp struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// GetFile fetches a file from `path` in `repo` at the default branch.
//
// Returns ErrNotFound (wrapped) if the path doesn't exist. Refusing
// non-default-branch refs is a safety rail: a PR-author can stage a
// malicious profile, but it can't run on a krapow-managed runner unless it
// merges to default first — which goes through the same review the workflow
// changes do.
func (c *Client) GetFile(repo, path string) (content []byte, sha string, err error) {
	var out contentsResp
	if err := c.do(http.MethodGet, fmt.Sprintf("/repos/%s/contents/%s", repo, path), &out); err != nil {
		// The do() helper turns 404 into a wrapped error string; surface a typed one.
		if strings.Contains(err.Error(), "404") {
			return nil, "", fmt.Errorf("%s: %w", path, ErrNotFound)
		}
		return nil, "", err
	}
	if out.Encoding != "base64" {
		return nil, "", fmt.Errorf("unexpected encoding %q for %s", out.Encoding, path)
	}
	b, err := base64.StdEncoding.DecodeString(out.Content)
	if err != nil {
		// GitHub line-wraps the base64; tolerate that.
		b, err = base64.StdEncoding.DecodeString(stripWhitespace(out.Content))
		if err != nil {
			return nil, "", fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return b, out.SHA, nil
}

func stripWhitespace(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' && s[i] != '\r' && s[i] != ' ' && s[i] != '\t' {
			b = append(b, s[i])
		}
	}
	return string(b)
}

func (c *Client) do(method, path string, out any) error {
	req, err := http.NewRequest(method, "https://api.github.com"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "krapow/0.1")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, string(body))
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}
