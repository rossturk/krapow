package cmd

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/rossturk/krapow/internal/config"
	"github.com/rossturk/krapow/internal/githubapi"
	"github.com/rossturk/krapow/internal/imagebuild"
	"github.com/rossturk/krapow/internal/incus"
	"github.com/rossturk/krapow/internal/provision"
	"github.com/rossturk/krapow/internal/sshkeys"
	"github.com/rossturk/krapow/internal/state"
	"github.com/rossturk/krapow/internal/tui"
	"github.com/rossturk/krapow/internal/winssh"
	"github.com/spf13/cobra"
)

// randomSuffix returns a 6-char [a-z0-9] string for runner names.
func randomSuffix() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()&0xffffff)
	}
	out := make([]byte, 6)
	for i, x := range b {
		out[i] = alphabet[int(x)%len(alphabet)]
	}
	return string(out)
}

func initCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init",
		Short: "Create a runner VM and register it with GitHub",
	}
	c.AddCommand(initLinuxCmd(), initWinCmd())
	return c
}

func initLinuxCmd() *cobra.Command {
	var name, labels string
	var plain bool
	c := &cobra.Command{
		Use:   "linux",
		Short: "Launch an Ubuntu Incus VM as a Linux runner",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInit(linuxKind, name, labels, plain)
		},
	}
	c.Flags().StringVar(&name, "name", "", "instance + runner name (default: linux-runner-<6 alphanum>)")
	c.Flags().StringVar(&labels, "labels", "self-hosted,linux,krapow", "comma-separated runner labels")
	c.Flags().BoolVar(&plain, "plain", false, "disable the interactive TUI and print plain status lines")
	return c
}

func initWinCmd() *cobra.Command {
	var name, labels string
	var yesBuild, plain bool
	c := &cobra.Command{
		Use:   "win",
		Short: "Launch a Windows Incus VM as a runner (auto-bakes base image on first run)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInitWin(name, labels, yesBuild, plain)
		},
	}
	c.Flags().StringVar(&name, "name", "", "instance + runner name (default: win-runner-<6 alphanum>)")
	c.Flags().StringVar(&labels, "labels", "self-hosted,windows,krapow", "comma-separated runner labels")
	c.Flags().BoolVarP(&yesBuild, "yes", "y", false, "skip the confirmation prompt before kicking off a base-image build")
	c.Flags().BoolVar(&plain, "plain", false, "disable the interactive TUI and print plain status lines")
	return c
}

type kind int

const (
	linuxKind kind = iota
	windowsKind
)

var (
	linuxImage   = envOr("KRAPOW_LINUX_IMAGE", "images:ubuntu/noble/cloud")
	windowsImage = envOr("KRAPOW_WIN_IMAGE", "local:win-runner-base")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func runInitWin(name, labels string, yesBuild, plain bool) error {
	exists, err := incus.ImageExists(windowsImage)
	if err != nil {
		return err
	}
	if !exists {
		if !yesBuild {
			fmt.Printf("Base image %q not found.\n", windowsImage)
			fmt.Printf("krapow will bake it now: download Windows Server 2022 Eval + virtio drivers, run\n")
			fmt.Printf("an unattended install, sysprep, and publish — about 45–90 minutes total.\n")
			fmt.Printf("Proceed? [y/N] ")
			if !readYes() {
				return fmt.Errorf("aborted; rerun with -y to skip this prompt")
			}
		}
		if err := imagebuild.Build("win-runner-base"); err != nil {
			return err
		}
	}
	return runInit(windowsKind, name, labels, plain)
}

func readYes() bool {
	s := bufio.NewScanner(os.Stdin)
	if !s.Scan() {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(s.Text()))
	return a == "y" || a == "yes"
}

// initContext bundles everything a phase needs.
type initContext struct {
	cfg      *config.Config
	gh       *githubapi.Client
	kind     kind
	name     string
	labels   string
	regToken string
	vmIP     string // populated by Windows ssh phase
}

func runInit(k kind, name, labels string, plain bool) error {
	cfg, err := config.Load(".env")
	if err != nil {
		return err
	}
	if name == "" {
		prefix := "linux-runner"
		if k == windowsKind {
			prefix = "win-runner"
		}
		name = fmt.Sprintf("%s-%s", prefix, randomSuffix())
	}
	if s, _ := state.Load(name); s != nil {
		return fmt.Errorf("runner %q already exists in krapow state", name)
	}

	ic := &initContext{cfg: cfg, gh: githubapi.New(cfg.PAT), kind: k, name: name, labels: labels}

	var phases []tui.PhaseSpec
	if k == linuxKind {
		phases = []tui.PhaseSpec{
			{ID: "token", Label: "token"},
			{ID: "launch", Label: "launch"},
			{ID: "state", Label: "state"},
			{ID: "cloud_init", Label: "cloud-init"},
			{ID: "verify", Label: "verify"},
		}
	} else {
		phases = []tui.PhaseSpec{
			{ID: "token", Label: "token"},
			{ID: "launch", Label: "launch"},
			{ID: "ssh", Label: "ssh"},
			{ID: "partition", Label: "partition"},
			{ID: "register", Label: "register"},
			{ID: "verify", Label: "verify"},
		}
	}

	runner := tui.New(name, phases, plain)
	incus.StreamOut = runner.Logger()
	incus.StreamErr = runner.Logger()
	winssh.StreamOut = runner.Logger()
	winssh.StreamErr = runner.Logger()

	var workErr error
	go func() {
		defer func() { runner.Finish(workErr) }()
		workErr = doInit(runner, ic)
	}()

	if err := runner.Run(); err != nil {
		return err
	}
	if workErr != nil {
		return workErr
	}
	fmt.Printf("==> %s registered (%s)\n", ic.name, kindName(k))
	return nil
}

func kindName(k kind) string {
	if k == windowsKind {
		return "windows"
	}
	return "linux"
}

func doInit(r *tui.Runner, ic *initContext) error {
	r.Start("token")
	r.Log("POST /repos/%s/actions/runners/registration-token", ic.cfg.Repo)
	tok, err := ic.gh.RegistrationToken(ic.cfg.Repo)
	if err == nil {
		r.Log("token issued (1h ttl)")
	}
	r.End("token", err)
	if err != nil {
		return err
	}
	ic.regToken = tok

	vars := provision.Vars{
		RepoURL: ic.cfg.RepoURL, RegToken: ic.regToken,
		Name: ic.name, Labels: ic.labels,
	}
	if ic.kind == linuxKind {
		return doInitLinux(r, ic, vars)
	}
	return doInitWindows(r, ic, vars)
}

func doInitLinux(r *tui.Runner, ic *initContext, vars provision.Vars) error {
	userData, err := provision.LinuxCloudInit(vars)
	if err != nil {
		return err
	}
	r.Start("launch")
	r.Log("incus launch %s %s --vm", linuxImage, ic.name)
	r.Log("  cpus=4  memory=8GiB  root=20GiB")
	err = incus.LaunchVM(linuxImage, ic.name, map[string]string{
		"user.user-data":      userData,
		"security.secureboot": "false",
		"limits.cpu":          "4",
		"limits.memory":       "8GiB",
	}, map[string]string{"root.size": "20GiB"})
	if err == nil {
		r.Log("VM started (cloud-init now running async inside the guest)")
	}
	r.End("launch", err)
	if err != nil {
		return err
	}

	r.Start("state")
	r.Log("writing ~/.krapow/state/%s.json", ic.name)
	err = state.Save(state.Runner{
		Name: ic.name, Kind: "linux", Repo: ic.cfg.Repo,
		Labels: ic.labels, Created: time.Now(),
	})
	r.End("state", err)
	if err != nil {
		return err
	}

	r.Start("cloud_init")
	r.Log("waiting for cloud-init (runner agent install)")
	tail := &cloudInitTail{name: ic.name}
	err = waitForLinuxCloudInit(r, ic.name, tail, 30*time.Minute)
	r.End("cloud_init", err)
	if err != nil {
		return err
	}

	r.Start("verify")
	r.Log("polling GitHub for runner to report 'online'")
	err = verifyRunnerOnline(r, ic.gh, ic.cfg.Repo, ic.name, 2*time.Minute)
	r.End("verify", err)
	return err
}

func doInitWindows(r *tui.Runner, ic *initContext, vars provision.Vars) error {
	r.Start("launch")
	r.Log("incus launch %s %s --vm", windowsImage, ic.name)
	r.Log("  cpus=4  memory=8GiB  root=60GiB")
	// Must be >= the published base image's disk size (60 GiB — set in the
	// bake VM). Cloning into a smaller volume fails with "Source image size
	// exceeds specified volume size".
	err := incus.LaunchVM(windowsImage, ic.name, map[string]string{
		"security.secureboot": "false",
		"limits.cpu":          "4",
		"limits.memory":       "8GiB",
	}, map[string]string{"root.size": "60GiB"})
	if err == nil {
		r.Log("VM started; writing state file")
		err = state.Save(state.Runner{
			Name: ic.name, Kind: "windows", Repo: ic.cfg.Repo,
			Labels: ic.labels, Created: time.Now(),
		})
	}
	r.End("launch", err)
	if err != nil {
		return err
	}

	r.Start("ssh")
	r.Log("polling DHCP for IPv4 + SSH port 22 (Windows boot ~3-5 min)")
	ip, err := waitForWindowsSSHLogged(r, ic.name, 15*time.Minute)
	if err == nil {
		r.Log("connected: %s", ip)
	}
	r.End("ssh", err)
	if err != nil {
		return err
	}
	ic.vmIP = ip

	privPath, _, err := sshkeys.EnsureKeyPair()
	if err != nil {
		return err
	}
	c, err := winssh.Dial(ip, privPath, 30*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()

	r.Start("partition")
	r.Log("Resize-Partition C: to fill the disk")
	_, err = c.RunPowerShell(`
$max = (Get-PartitionSupportedSize -DriveLetter C).SizeMax
$cur = (Get-Partition -DriveLetter C).Size
if ($max -gt $cur) {
    Resize-Partition -DriveLetter C -Size $max
    Write-Host ("C: grown to {0:N1} GiB" -f ($max / 1GB))
} else {
    Write-Host ("C: already at max ({0:N1} GiB)" -f ($cur / 1GB))
}
`)
	r.End("partition", err)
	if err != nil {
		return fmt.Errorf("resize C: failed: %w", err)
	}

	ps1, err := provision.WindowsPS1(vars)
	if err != nil {
		return err
	}
	r.Start("register")
	r.Log("downloading actions-runner-win-x64 release & running config.cmd")
	_, err = c.RunPowerShell(ps1)
	r.End("register", err)
	if err != nil {
		return err
	}

	r.Start("verify")
	r.Log("polling GitHub for runner to report 'online'")
	err = verifyRunnerOnline(r, ic.gh, ic.cfg.Repo, ic.name, 2*time.Minute)
	r.End("verify", err)
	return err
}

// verifyRunnerOnline polls GitHub until the runner is registered AND its
// status is 'online' (heartbeating).
func verifyRunnerOnline(r *tui.Runner, gh *githubapi.Client, repo, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		runner, err := gh.FindRunner(repo, name)
		if err != nil {
			return err
		}
		if runner == nil {
			r.Log("not in GitHub's runner list yet")
		} else if runner.Status != "online" {
			r.Log("registered (id=%d) but status=%s; waiting for heartbeat", runner.ID, runner.Status)
		} else {
			r.Log("online (id=%d, busy=%v)", runner.ID, runner.Busy)
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("runner %s never reported 'online' within %s", name, timeout)
}

// cloudInitTail tracks how far we've streamed cloud-init-output.log so
// successive phases don't replay the same bytes into the viewport.
type cloudInitTail struct {
	name   string
	offset int64
}

func (t *cloudInitTail) stream(r *tui.Runner) {
	tailCmd := exec.Command("incus", "exec", t.name, "--",
		"sh", "-c", fmt.Sprintf("tail -c +%d /var/log/cloud-init-output.log 2>/dev/null || true", t.offset+1))
	newBytes, err := tailCmd.Output()
	if err != nil || len(newBytes) == 0 {
		return
	}
	t.offset += int64(len(newBytes))
	for _, line := range strings.Split(string(newBytes), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r.Log("%s", line)
	}
}

func waitForLinuxCloudInit(r *tui.Runner, name string, tail *cloudInitTail, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		statusOut, _ := exec.Command("incus", "exec", name, "--", "cloud-init", "status").Output()
		status := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(statusOut)), "status:"))

		tail.stream(r)

		switch status {
		case "done":
			return nil
		case "error":
			return fmt.Errorf("cloud-init failed in %s (check `incus exec %s -- cloud-init status --long`)", name, name)
		}
		time.Sleep(8 * time.Second)
	}
	return fmt.Errorf("timed out waiting for cloud-init in %s", name)
}

func waitForWindowsSSHLogged(r *tui.Runner, name string, timeout time.Duration) (string, error) {
	privPath, _, err := sshkeys.EnsureKeyPair()
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	announcedIP := false
	attempt := 0
	for time.Now().Before(deadline) {
		ip := vmIPv4(name)
		if ip != "" {
			if !announcedIP {
				r.Log("IPv4 up: %s", ip)
				announcedIP = true
			}
			attempt++
			r.Log("ssh attempt %d on %s:22", attempt, ip)
			c, err := winssh.Dial(ip, privPath, 20*time.Second)
			if err == nil {
				_ = c.Close()
				return ip, nil
			}
		}
		time.Sleep(15 * time.Second)
	}
	return "", fmt.Errorf("timed out waiting for SSH on %s", name)
}

func vmIPv4(name string) string {
	out, err := exec.Command("incus", "list", name, "--format", "csv", "-c", "4").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if i := strings.Index(line, " "); i > 0 {
		return line[:i]
	}
	return line
}
