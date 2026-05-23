// Package hostmac runs a GitHub Actions runner on the macOS host itself,
// supervised by launchd as a LaunchAgent. Each runner gets a per-name
// working directory at ~/.krapow/runners/<name>/ where the actions-runner
// agent is installed and where _work/ workspaces live.
//
// No sudo, no service users, no VM. The runner runs as the current user
// with the user's real HOME — codesign/notarize/keychain access all work
// out of the box. The only env override is TMPDIR (so per-runner temp files
// don't pile up in /var/folders). State pollution (gh auth, gitconfig, npm
// caches) is the accepted cost of --isolation host; users who want hard
// isolation use --isolation vm.
package hostmac

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

//go:embed launchagent.plist.tmpl
var plistTmpl string

// wrapperScript is dropped into each runner's home as `krapow-runner`. macOS
// reports background LaunchAgents by ProgramArguments[0]'s basename — pointing
// launchd at this wrapper means notifications and Login Items both show
// "krapow-runner" instead of the generic "run.sh".
//
//go:embed krapow-runner.sh
var wrapperScript []byte

// LabelPrefix is the launchd label namespace for krapow-managed runners.
// Used both to build per-runner labels and to enumerate orphans in `clean`.
const LabelPrefix = "com.monsterdept.krapow."

// Label returns the launchd label for a runner (e.g. com.monsterdept.krapow.foo).
func Label(name string) string { return LabelPrefix + name }

// RunnerHome returns the per-runner working directory under
// ~/.krapow/runners/<name>/. The actions-runner agent is installed here
// and _work/ workspaces live here, but the runner process's HOME is the
// user's real home — so codesign and other keychain-aware tools work.
func RunnerHome(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".krapow", "runners", name), nil
}

// LaunchAgentsDir is the user's ~/Library/LaunchAgents — where launchd
// auto-loads plists at login.
func LaunchAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

// LaunchAgentPath returns the plist path for a runner under the real
// LaunchAgents dir.
func LaunchAgentPath(name string) (string, error) {
	d, err := LaunchAgentsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, Label(name)+".plist"), nil
}

// PrepareHome creates the per-runner working directory and its private
// TMPDIR, and drops in the `krapow-runner` wrapper script that the
// LaunchAgent's ProgramArguments points at. Idempotent.
func PrepareHome(name string) error {
	rh, err := RunnerHome(name)
	if err != nil {
		return err
	}
	for _, sub := range []string{"", "tmp"} {
		if err := os.MkdirAll(filepath.Join(rh, sub), 0o755); err != nil {
			return err
		}
	}
	wrapper := filepath.Join(rh, "krapow-runner")
	if err := os.WriteFile(wrapper, wrapperScript, 0o755); err != nil {
		return fmt.Errorf("write %s: %w", wrapper, err)
	}
	return nil
}

// RunProvision pipes `script` to `bash -s` with cwd set to the runner's
// working directory and TMPDIR pointed at its private tmp/. HOME stays
// inherited from the parent (the user's real home) so the runner agent's
// install and any keychain-touching steps see the user's real Library/.
// stdout/stderr go to the writers (typically the TUI logger).
func RunProvision(name, script string, stdout, stderr io.Writer) error {
	rh, err := RunnerHome(name)
	if err != nil {
		return err
	}
	cmd := exec.Command("bash", "-s")
	cmd.Dir = rh
	cmd.Env = sandboxEnv(rh)
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// sandboxEnv returns the parent env with TMPDIR overridden to the runner's
// private tmp/. HOME is NOT overridden — macOS's keychain search list is
// HOME-relative (~/Library/Preferences/com.apple.security.plist), so an
// overridden HOME makes codesign / xcrun notarytool / iOS provisioning all
// fail. Per-runner state still lives in rh because cwd points there and the
// actions-runner agent stores its config relative to cwd.
func sandboxEnv(rh string) []string {
	out := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		if k == "TMPDIR" {
			continue
		}
		out = append(out, e)
	}
	out = append(out, "TMPDIR="+filepath.Join(rh, "tmp"))
	return out
}

// SandboxEnv returns the env the runner's processes should see — parent env
// with TMPDIR pointed at the per-runner tmp/. Exposed so `krapow shell` can
// drop the user into an interactive shell with the same env the runner
// agent itself runs under.
func SandboxEnv(name string) ([]string, error) {
	rh, err := RunnerHome(name)
	if err != nil {
		return nil, err
	}
	return sandboxEnv(rh), nil
}

// PlistVars is the template input for the embedded LaunchAgent plist.
type PlistVars struct {
	Label      string
	RunnerHome string
	PATH       string
}

// RenderPlist returns the plist XML for a runner. Exposed for testability
// (no filesystem side effects) — callers usually want WritePlist.
func RenderPlist(name string) (string, error) {
	rh, err := RunnerHome(name)
	if err != nil {
		return "", err
	}
	t, err := template.New("plist").Parse(plistTmpl)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	err = t.Execute(&sb, PlistVars{
		Label:      Label(name),
		RunnerHome: rh,
		PATH:       os.Getenv("PATH"),
	})
	return sb.String(), err
}

// WritePlist generates and installs the LaunchAgent plist for a runner in
// the user's real ~/Library/LaunchAgents/ so launchd loads it at login.
func WritePlist(name string) error {
	pp, err := LaunchAgentPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		return err
	}
	content, err := RenderPlist(name)
	if err != nil {
		return err
	}
	return os.WriteFile(pp, []byte(content), 0o644)
}

// userDomain returns the launchctl per-user domain target for the current
// uid. We use user/<uid> rather than gui/<uid> because the latter requires
// an Aqua session — `krapow init mac` over SSH would otherwise fail with
// "Domain does not support specified action". user/<uid> works in both
// SSH and GUI contexts. Runners don't need GUI access (codesign etc.
// reach the keychain via Security.framework, not the WindowServer).
func userDomain() string {
	return "user/" + strconv.Itoa(os.Getuid())
}

// Bootstrap loads the plist via `launchctl bootstrap user/<uid>`. Treats
// already-loaded as success so `krapow start` is idempotent.
func Bootstrap(name string) error {
	pp, err := LaunchAgentPath(name)
	if err != nil {
		return err
	}
	out, err := exec.Command("launchctl", "bootstrap", userDomain(), pp).CombinedOutput()
	if err == nil {
		return nil
	}
	s := string(out)
	if strings.Contains(s, "already") || strings.Contains(s, "exists") {
		return nil
	}
	return fmt.Errorf("launchctl bootstrap %s: %w: %s", pp, err, strings.TrimSpace(s))
}

// Bootout unloads the plist. Returns nil if it wasn't loaded.
func Bootout(name string) error {
	target := userDomain() + "/" + Label(name)
	out, err := exec.Command("launchctl", "bootout", target).CombinedOutput()
	if err == nil {
		return nil
	}
	s := string(out)
	if strings.Contains(s, "No such process") || strings.Contains(s, "Could not find") {
		return nil
	}
	return fmt.Errorf("launchctl bootout %s: %w: %s", target, err, strings.TrimSpace(s))
}

// State reports whether the LaunchAgent is currently loaded:
// "running" (loaded), "stopped" (plist on disk but not loaded), or "absent".
//
// "running" here means "launchd has the job"; if the supervised process
// itself is crash-looping, GitHub will show the runner as Offline anyway.
func State(name string) string {
	target := userDomain() + "/" + Label(name)
	cmd := exec.Command("launchctl", "print", target)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err == nil {
		return "running"
	}
	pp, err := LaunchAgentPath(name)
	if err != nil {
		return "absent"
	}
	if _, err := os.Stat(pp); err == nil {
		return "stopped"
	}
	return "absent"
}

// Destroy unloads the LaunchAgent and removes the plist + runner home.
// Best-effort: returns the first error but always tries every step so a
// partial install leaves no stragglers behind.
func Destroy(name string) error {
	var firstErr error
	if err := Bootout(name); err != nil && firstErr == nil {
		firstErr = err
	}
	if pp, err := LaunchAgentPath(name); err == nil {
		if rerr := os.Remove(pp); rerr != nil && !os.IsNotExist(rerr) && firstErr == nil {
			firstErr = fmt.Errorf("rm %s: %w", pp, rerr)
		}
	}
	if rh, err := RunnerHome(name); err == nil {
		if rerr := os.RemoveAll(rh); rerr != nil && firstErr == nil {
			firstErr = fmt.Errorf("rm -rf %s: %w", rh, rerr)
		}
	}
	return firstErr
}

// LocalRunners enumerates names of host-isolated runners visible on disk —
// either via a plist in ~/Library/LaunchAgents or a runner home directory.
// Used by `clean` to find orphans not tracked in state.
func LocalRunners() ([]string, error) {
	seen := map[string]struct{}{}

	if d, err := LaunchAgentsDir(); err == nil {
		entries, err := os.ReadDir(d)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, e := range entries {
			n := e.Name()
			if !strings.HasPrefix(n, LabelPrefix) || !strings.HasSuffix(n, ".plist") {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(n, LabelPrefix), ".plist")
			if name != "" {
				seen[name] = struct{}{}
			}
		}
	}

	if home, err := os.UserHomeDir(); err == nil {
		runnersDir := filepath.Join(home, ".krapow", "runners")
		entries, err := os.ReadDir(runnersDir)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				seen[e.Name()] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	return out, nil
}
