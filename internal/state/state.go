// Package state persists per-runner metadata at ~/.krapow/state/<name>.json.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type Runner struct {
	Name    string    `json:"name"`
	Kind    string    `json:"kind"`              // "linux", "windows", or "mac"
	Backend string    `json:"backend,omitempty"` // "incus" or "tart"; empty == "incus" (pre-mac records)
	Repo    string    `json:"repo"`              // "owner/name" when Scope=="repo"; "orgname" when Scope=="org"
	Scope   string    `json:"scope,omitempty"`   // "repo" or "org"; empty == "repo" (pre-org-runner records)
	Labels  string    `json:"labels"`
	Created time.Time `json:"created"`
}

// EffectiveBackend returns r.Backend with the legacy default ("incus") applied
// so consumers can branch without sprinkling empty-string checks everywhere.
func (r *Runner) EffectiveBackend() string {
	if r.Backend == "" {
		return "incus"
	}
	return r.Backend
}

// EffectiveScope returns r.Scope with the legacy default ("repo") applied,
// matching how older state records without the field should be interpreted.
func (r *Runner) EffectiveScope() string {
	if r.Scope == "" {
		return "repo"
	}
	return r.Scope
}

// APITarget returns the GitHub API path prefix for this runner's scope:
// "repos/owner/name" for repo runners or "orgs/orgname" for org runners.
// Suitable for passing to githubapi.Client methods.
func (r *Runner) APITarget() string {
	if r.EffectiveScope() == "org" {
		return "orgs/" + r.Repo
	}
	return "repos/" + r.Repo
}

func dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".krapow", "state"), nil
}

func path(name string) (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, name+".json"), nil
}

func Save(r Runner) error {
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	p, _ := path(r.Name)
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

func Load(name string) (*Runner, error) {
	p, err := path(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var r Runner
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &r, nil
}

func Remove(name string) error {
	p, err := path(name)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func All() ([]Runner, error) {
	d, err := dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Runner
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d, e.Name()))
		if err != nil {
			continue
		}
		var r Runner
		if err := json.Unmarshal(b, &r); err == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
