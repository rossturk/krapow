package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/monsterdept/krapow/internal/auth"
	"github.com/monsterdept/krapow/internal/githubapi"
	"github.com/monsterdept/krapow/internal/imagebuild"
	"github.com/monsterdept/krapow/internal/state"
	"github.com/spf13/cobra"
)

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose host readiness for krapow",
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := []func() checkResult{checkAuth}
			checks = append(checks, gitHubTokenChecks()...)
			if runtime.GOOS == "darwin" {
				// macOS host: host-isolation (default for `init mac`) needs
				// launchctl + the host's gh; tart drives `--isolation vm`
				// mac runners and the linux-arm path.
				checks = append(checks,
					checkLaunchctlReachable,
					checkHostToolchain,
					checkTartReachable,
					checkSshpassReachable,
					checkTartCacheHealth,
				)
			} else {
				checks = append(checks,
					checkIncusReachable,
					checkVsock,
					checkDockerForwardConflict,
					checkWindowsBuildDeps,
				)
			}
			// Cross-platform: free-disk warning. macOS hosts pull ~30 GB tart
			// images + ~80 GB clones; Linux hosts launch 75 GiB Incus VMs.
			// Either way, running tight is the single most common reason an
			// init fails midway and leaves cache state half-written.
			checks = append(checks, checkHostDiskSpace)
			anyFail := false
			for _, c := range checks {
				r := c()
				fmt.Printf("[%s] %s", r.status, r.name)
				if r.detail != "" {
					fmt.Printf(" — %s", r.detail)
				}
				fmt.Println()
				if r.fix != "" {
					fmt.Printf("        fix: %s\n", r.fix)
				}
				if r.status == statusFail {
					anyFail = true
				}
			}
			if anyFail {
				return errors.New("one or more checks failed")
			}
			return nil
		},
	}
}

type checkStatus string

const (
	statusOK   checkStatus = " ok "
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "fail"
	statusSkip checkStatus = "skip"
)

type checkResult struct {
	status checkStatus
	name   string
	detail string
	fix    string
}

func checkTartReachable() checkResult {
	if _, err := exec.LookPath("tart"); err != nil {
		return checkResult{
			status: statusFail,
			name:   "tart CLI on PATH",
			detail: "not found",
			fix:    "brew install cirruslabs/cli/tart",
		}
	}
	// `tart list` is the cheapest probe — exits 0 and prints "[]" when no VMs
	// exist, fails if Virtualization.framework can't initialize.
	if out, err := exec.Command("tart", "list", "--format", "json").CombinedOutput(); err != nil {
		return checkResult{
			status: statusFail,
			name:   "tart usable",
			detail: strings.TrimSpace(string(out)),
			fix:    "check tart install + Virtualization.framework availability",
		}
	}
	return checkResult{status: statusOK, name: "tart usable"}
}

// checkHostDiskSpace warns when the user's home volume has less free space
// than a typical init needs end-to-end. The thresholds aren't exact — they're
// "you'll be sad if you start an init now" guards, not hard requirements.
//
// macOS: image (~30 GB) + clone (~80 GB) + Virtualization.framework scratch.
// 50 GiB is the floor where things stop getting weird on the cirruslabs
// macos-sequoia-xcode image; lower than that and we've seen partial pulls
// leave ~/.tart in a half-broken state that VZ then refuses to personalize.
//
// Linux: 75 GiB Incus base disk + a few GB of scratch.
func checkHostDiskSpace() checkResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return checkResult{status: statusWarn, name: "host disk space", detail: err.Error()}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(home, &st); err != nil {
		return checkResult{status: statusWarn, name: "host disk space", detail: err.Error()}
	}
	// Bavail is "blocks available to non-root user" — matches what `df` reports
	// and what tart actually has to work with.
	free := uint64(st.Bavail) * uint64(st.Bsize)
	freeGiB := free >> 30
	threshold := uint64(50)
	if runtime.GOOS != "darwin" {
		threshold = 80 // Incus base disk alone is 75 GiB
	}
	if freeGiB < threshold {
		return checkResult{
			status: statusWarn,
			name:   "host disk space",
			detail: fmt.Sprintf("%d GiB free under %s — recommend ≥ %d GiB before next init", freeGiB, home, threshold),
			fix:    "free up space; see `du -sh ~/Library/Caches/* ~/.tart` etc.",
		}
	}
	return checkResult{status: statusOK, name: "host disk space", detail: fmt.Sprintf("%d GiB free", freeGiB)}
}

// checkTartCacheHealth catches the "ghost tree" state — the OCI cache
// directory exists, has subdirs for the image references, but the actual
// layer files are missing or near-zero bytes. That's the signature of an
// interrupted/disk-full pull. tart then claims the image is present but
// VZ rejects the resulting VM with `VZErrorDomain Code=-9` on `tart run`.
//
// Recovery is `rm -rf ~/.tart && retry init`. There's no in-place repair
// because tart's manifest parsing won't notice the empty layers.
func checkTartCacheHealth() checkResult {
	home, err := os.UserHomeDir()
	if err != nil {
		return checkResult{status: statusOK, name: "tart cache health"}
	}
	cacheDir := filepath.Join(home, ".tart", "cache", "OCIs")
	info, err := os.Stat(cacheDir)
	if err != nil || !info.IsDir() {
		return checkResult{status: statusOK, name: "tart cache health", detail: "no OCI cache yet"}
	}

	// Walk the cache, tracking whether we see subdirs at all and the total
	// bytes of cached layer data. A healthy cache after even one pull is at
	// least hundreds of MB; <100 MB across the whole tree means the dirs are
	// just placeholders.
	var hasSubdirs bool
	var totalBytes int64
	_ = filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if path != cacheDir {
				hasSubdirs = true
			}
			return nil
		}
		if fi, ierr := d.Info(); ierr == nil {
			totalBytes += fi.Size()
		}
		return nil
	})

	if hasSubdirs && totalBytes < (100<<20) {
		return checkResult{
			status: statusWarn,
			name:   "tart cache health",
			detail: fmt.Sprintf("OCI cache has directories but only %.1f MB of data — signature of a failed pull", float64(totalBytes)/(1<<20)),
			fix:    "rm -rf ~/.tart  (forces a clean re-pull on next `krapow init`)",
		}
	}
	return checkResult{status: statusOK, name: "tart cache health"}
}

// checkLaunchctlReachable verifies the launchctl CLI is available and the
// gui/<uid> domain is reachable. Host-isolated runners are LaunchAgents in
// that domain; if launchctl can't talk to it (e.g. user is running over SSH
// without a real GUI session), `krapow init mac` will fail at activate.
func checkLaunchctlReachable() checkResult {
	if _, err := exec.LookPath("launchctl"); err != nil {
		return checkResult{
			status: statusFail,
			name:   "launchctl on PATH",
			detail: "not found (required for `krapow init mac` host isolation)",
			fix:    "comes with macOS — investigate /bin/launchctl",
		}
	}
	target := "gui/" + fmt.Sprintf("%d", os.Getuid())
	out, err := exec.Command("launchctl", "print", target).CombinedOutput()
	if err != nil {
		return checkResult{
			status: statusWarn,
			name:   "launchctl gui/<uid> reachable",
			detail: strings.TrimSpace(string(out)),
			fix:    "host-isolated runners need a real login session; if you're on SSH, try again after `launchctl asuser` or fall back to `--isolation vm`",
		}
	}
	return checkResult{status: statusOK, name: "launchctl gui/<uid> reachable"}
}

// checkHostToolchain verifies the tools host-isolated runners depend on are
// on the user's PATH. Unlike VM-based runners, host runners use whatever
// brew/gh/xcode the host has — krapow doesn't install them.
func checkHostToolchain() checkResult {
	var missing []string
	for _, bin := range []string{"gh", "git", "python3"} {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) == 0 {
		return checkResult{status: statusOK, name: "host toolchain (gh, git, python3)"}
	}
	return checkResult{
		status: statusWarn,
		name:   "host toolchain (gh, git, python3)",
		detail: "missing: " + strings.Join(missing, ", ") + " — needed only for `--isolation host` mac runners",
		fix:    "brew install " + strings.Join(missing, " "),
	}
}

func checkSshpassReachable() checkResult {
	if _, err := exec.LookPath("sshpass"); err != nil {
		return checkResult{
			status: statusWarn,
			name:   "sshpass on PATH",
			detail: "needed by `krapow init mac` / `init linux` on macOS — cirruslabs images use admin:admin password auth",
			fix:    "brew install sshpass  (or: brew tap esolitos/ipa && brew install esolitos/ipa/sshpass)",
		}
	}
	return checkResult{status: statusOK, name: "sshpass on PATH"}
}

func checkIncusReachable() checkResult {
	if _, err := exec.LookPath("incus"); err != nil {
		return checkResult{
			status: statusFail,
			name:   "incus CLI on PATH",
			detail: "not found",
			fix:    "https://linuxcontainers.org/incus/docs/main/installing/",
		}
	}
	if out, err := exec.Command("incus", "list", "--format", "csv").CombinedOutput(); err != nil {
		return checkResult{
			status: statusFail,
			name:   "incus daemon reachable",
			detail: strings.TrimSpace(string(out)),
			fix:    "sudo usermod -aG incus-admin $USER  &&  newgrp incus-admin",
		}
	}
	return checkResult{status: statusOK, name: "incus daemon reachable"}
}

func checkVsock() checkResult {
	// vsock is VM-only — needed for the Incus agent in a VM guest. If no
	// VM-isolated incus runners are tracked, skip the check entirely so a
	// container-only fleet doesn't get a permanent warn for an unused module.
	// Windows runners are always Incus VMs, so they count too.
	if !anyVMIncusRunner() {
		return checkResult{
			status: statusSkip,
			name:   "vhost-vsock available",
			detail: "no VM-isolated incus runners tracked; check skipped",
		}
	}
	if _, err := os.Stat("/dev/vhost-vsock"); err == nil {
		return checkResult{status: statusOK, name: "vhost-vsock available"}
	}
	return checkResult{
		status: statusWarn,
		name:   "vhost-vsock available",
		detail: "/dev/vhost-vsock missing; Incus VMs need this for the agent",
		fix:    "sudo modprobe vhost_vsock  &&  echo vhost_vsock | sudo tee /etc/modules-load.d/vsock.conf",
	}
}

func anyVMIncusRunner() bool {
	runners, _ := state.All()
	for i := range runners {
		r := &runners[i]
		if r.Kind == "windows" {
			return true
		}
		if r.Kind == "linux" && r.EffectiveIsolation() == "vm" {
			return true
		}
	}
	return false
}

func checkAuth() checkResult {
	_, src, err := auth.Token()
	if err != nil {
		return checkResult{
			status: statusFail,
			name:   "GitHub token resolvable",
			detail: err.Error(),
			fix:    "export GITHUB_TOKEN=ghp_... or run `gh auth login`",
		}
	}
	return checkResult{status: statusOK, name: "GitHub token resolvable", detail: "via " + string(src)}
}

// gitHubTokenChecks returns one check per distinct (scope,target) tracked in
// state, plus a token-only fallback when no runners exist yet. Probing every
// target catches the common mixed-scope case where a token works for a repo
// but lacks admin:org for an org runner — and avoids the old "doctor probes
// some random first-registered repo" surprise.
func gitHubTokenChecks() []func() checkResult {
	runners, _ := state.All()
	if len(runners) == 0 {
		return []func() checkResult{checkTokenAlive}
	}
	// Deduplicate by APITarget so a fleet of N runners against the same
	// target results in one probe.
	seen := map[string]bool{}
	var out []func() checkResult
	for _, r := range runners {
		t := r.APITarget()
		if seen[t] {
			continue
		}
		seen[t] = true
		scope := r.EffectiveScope()
		label := r.Repo
		out = append(out, func() checkResult { return probeTarget(scope, label, t) })
	}
	return out
}

// checkTokenAlive is the no-runners-tracked fallback: a /user probe just
// proves the token isn't expired or revoked. Doesn't verify any particular
// repo/org scope — that gets exercised on first init.
func checkTokenAlive() checkResult {
	tok, _, err := auth.Token()
	if err != nil {
		return checkResult{
			status: statusWarn,
			name:   "GitHub token works",
			detail: "skipped (no token resolvable)",
		}
	}
	if err := githubapi.New(tok).WhoAmI(); err != nil {
		return checkResult{
			status: statusFail,
			name:   "GitHub token works",
			detail: err.Error(),
			fix:    "regenerate token; classic PAT needs 'repo'; fine-grained needs 'admin:repo runners'",
		}
	}
	return checkResult{
		status: statusOK,
		name:   "GitHub token works",
		detail: "no runners yet — scope unverified (will check on first init)",
	}
}

// probeTarget exercises the token against one runner-management target.
// FindRunner with a sentinel name is the cheapest call that hits the same
// permission gate as actual runner operations without minting a token.
func probeTarget(scope, label, target string) checkResult {
	tok, _, err := auth.Token()
	if err != nil {
		return checkResult{
			status: statusWarn,
			name:   "GitHub token works for " + label,
			detail: "skipped (no token resolvable)",
		}
	}
	gh := githubapi.New(tok)
	name := "GitHub token works for " + scope + ":" + label
	if _, err := gh.FindRunner(target, "__krapow-doctor-probe__"); err != nil {
		fix := "regenerate token; repo runners need 'repo' scope"
		if scope == "org" {
			fix = "org runners need admin:org — try `gh auth refresh -h github.com -s admin:org`, or use a PAT with the org's 'Self-hosted runners: read & write' permission"
		}
		return checkResult{
			status: statusFail,
			name:   name,
			detail: err.Error(),
			fix:    fix,
		}
	}
	return checkResult{status: statusOK, name: name}
}

func checkWindowsBuildDeps() checkResult {
	missing := imagebuild.MissingDeps()
	if len(missing) == 0 {
		return checkResult{status: statusOK, name: "Windows base-image build deps"}
	}
	return checkResult{
		status: statusWarn,
		name:   "Windows base-image build deps",
		detail: "missing: " + strings.Join(missing, ", ") + " — only needed if you'll run `krapow init win` without a pre-built base image",
		fix:    "sudo apt install -y " + strings.Join(missing, " "),
	}
}

func checkDockerForwardConflict() checkResult {
	if _, err := os.Stat("/sys/class/net/docker0"); err != nil {
		return checkResult{status: statusOK, name: "no Docker FORWARD interference (Docker not installed)"}
	}
	out, err := exec.Command("iptables", "-S", "FORWARD").CombinedOutput()
	if err != nil {
		return checkResult{
			status: statusWarn,
			name:   "Docker FORWARD interference",
			detail: "Docker installed; need root to inspect iptables. If VMs can't reach github.com, apply the fix.",
			fix:    "sudo iptables -I DOCKER-USER -i incusbr0 -j ACCEPT && sudo iptables -I DOCKER-USER -o incusbr0 -j ACCEPT",
		}
	}
	if !strings.Contains(string(out), "-P FORWARD DROP") {
		return checkResult{status: statusOK, name: "Docker FORWARD policy is not DROP"}
	}
	duOut, _ := exec.Command("iptables", "-S", "DOCKER-USER").CombinedOutput()
	if strings.Contains(string(duOut), "-i incusbr0") || strings.Contains(string(duOut), "-o incusbr0") {
		return checkResult{status: statusOK, name: "DOCKER-USER bypasses incusbr0 past Docker FORWARD=DROP"}
	}
	return checkResult{
		status: statusFail,
		name:   "Docker FORWARD=DROP blocks incusbr0 traffic",
		detail: "VMs will silently fail to reach some external services (notably GitHub edge IPs)",
		fix:    "sudo iptables -I DOCKER-USER -i incusbr0 -j ACCEPT && sudo iptables -I DOCKER-USER -o incusbr0 -j ACCEPT",
	}
}
