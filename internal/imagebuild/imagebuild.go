// Package imagebuild builds a Windows base image for krapow without
// depending on any third-party repository at runtime.
//
// The build is self-contained: autounattend.xml and setup-ssh.ps1 are embedded
// in the krapow binary, the Windows Server 2022 Eval ISO + virtio-win ISO are
// downloaded from Microsoft / Fedora to a local cache on first run, and
// distrobuilder + Incus do the heavy lifting.
//
// Phases driven by the TUI:
//
//	iso → repack → answer → install → toolchain → sysprep → publish
package imagebuild

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/gofrs/flock"
	"github.com/rossturk/krapow/internal/incus"
	"github.com/rossturk/krapow/internal/sshkeys"
	"github.com/rossturk/krapow/internal/tui"
	"github.com/rossturk/krapow/internal/winssh"
)

// bakeOut / bakeErr are where runFG (the helper that shells out to
// distrobuilder, xorriso, qemu, incus) writes subprocess output. Build()
// rewires them to a TUI logger; default to stdout/stderr for non-TUI use.
var (
	bakeOut io.Writer = os.Stdout
	bakeErr io.Writer = os.Stderr
)

//go:embed assets/autounattend.xml assets/setup-ssh.ps1.tmpl assets/install-toolchain.ps1
var assets embed.FS

// Windows Server 2022 Eval — public Microsoft URL + known SHA-256.
const (
	winISOURL    = "https://software-download.microsoft.com/download/sg/20348.169.210806-2348.fe_release_svc_refresh_SERVER_EVAL_x64FRE_en-us.iso"
	winISOSHA256 = "4f1457c4fe14ce48c9b2324924f33ca4f0470475e6da851b39ccbf98f44e7852"
	virtioISOURL = "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/virtio-win.iso"
)

// acquireBakeLock takes an exclusive flock on ~/.krapow/bake.lock. If another
// process already holds it (another bake running), returns a typed error that
// names the holder so the user knows what to wait for or kill.
//
// The lock file content is "<pid> <ISO timestamp> <target alias>" written
// once on acquire; we read it before TryLock when reporting "already held"
// so the error names the running invocation. flock is released automatically
// when the process exits, so a hard-killed bake doesn't leave a stale lock.
func acquireBakeLock(targetAlias string) (*flock.Flock, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".krapow")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "bake.lock")
	lock := flock.New(path)
	got, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire bake lock %s: %w", path, err)
	}
	if !got {
		// Best-effort read of the holder's metadata for a friendlier error.
		holder := "unknown"
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			holder = strings.TrimSpace(string(b))
		}
		return nil, fmt.Errorf("another krapow bake is in progress (%s); wait for it to finish, or check `ps -ef | grep krapow`", holder)
	}
	// We hold the lock; record who we are so the next bake's error message
	// can name us.
	meta := fmt.Sprintf("pid=%d started=%s target=%s\n",
		os.Getpid(), time.Now().Format(time.RFC3339), targetAlias)
	_ = os.WriteFile(path, []byte(meta), 0o644)
	return lock, nil
}

// MissingDeps lists host commands the build needs that aren't on PATH.
func MissingDeps() []string {
	var missing []string
	for _, bin := range []string{"distrobuilder", "xorriso", "incus"} {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	return missing
}

// Build runs the full pipeline and ends with `targetAlias` pointing at the new
// base image. Idempotent against the cached ISOs (re-downloads only on miss).
//
// Holds an exclusive flock on ~/.krapow/bake.lock for the duration of the
// build so two concurrent `krapow init win`/`just rebake` invocations can't
// trample each other's work dir, fight over a bake-VM name, or saturate the
// host with two parallel Windows installs.
func Build(targetAlias string) error {
	if deps := MissingDeps(); len(deps) > 0 {
		return fmt.Errorf("missing host deps: %v (sudo apt install -y %s)",
			deps, strings.Join(deps, " "))
	}

	lock, err := acquireBakeLock(targetAlias)
	if err != nil {
		return err
	}
	defer func() {
		_ = lock.Unlock()
		_ = os.Remove(lock.Path()) // remove metadata file when we release
	}()

	// Prime sudo before the TUI takes over — distrobuilder needs root, and
	// once bubbletea is running it captures stdin, so a sudo password prompt
	// from inside the TUI is invisible and breaks the bake.
	if err := primeSudo(); err != nil {
		return err
	}

	runner := tui.New("bake "+targetAlias, []tui.PhaseSpec{
		{ID: "iso", Label: "iso"},
		{ID: "repack", Label: "repack"},
		{ID: "answer", Label: "answer"},
		{ID: "install", Label: "install"},
		{ID: "toolchain", Label: "toolchain"},
		{ID: "shutdown", Label: "shutdown"},
		{ID: "publish", Label: "publish"},
	}, false)

	// Route every subprocess writer through the TUI viewport for the bake's
	// duration. Restore on exit so subsequent flows aren't affected.
	prevBakeOut, prevBakeErr := bakeOut, bakeErr
	prevIncOut, prevIncErr := incus.StreamOut, incus.StreamErr
	prevWinOut, prevWinErr := winssh.StreamOut, winssh.StreamErr
	bakeOut, bakeErr = runner.Logger(), runner.Logger()
	incus.StreamOut, incus.StreamErr = runner.Logger(), runner.Logger()
	winssh.StreamOut, winssh.StreamErr = runner.Logger(), runner.Logger()
	defer func() {
		bakeOut, bakeErr = prevBakeOut, prevBakeErr
		incus.StreamOut, incus.StreamErr = prevIncOut, prevIncErr
		winssh.StreamOut, winssh.StreamErr = prevWinOut, prevWinErr
	}()

	var workErr error
	go func() {
		defer func() { runner.Finish(workErr) }()
		workErr = doBake(runner, targetAlias)
	}()
	if err := runner.Run(); err != nil {
		return err
	}
	return workErr
}

func doBake(r *tui.Runner, targetAlias string) error {
	cache, err := cacheDir()
	if err != nil {
		return err
	}
	work, err := workDir()
	if err != nil {
		return err
	}

	// ---------- iso ----------
	r.Start("iso")
	winISO := os.Getenv("WINDOWS_ISO")
	if winISO == "" {
		winISO = filepath.Join(cache, "windows-server-2022-eval.iso")
		r.Log("Windows Server 2022 Eval → %s", winISO)
		if err := ensureDownload(winISO, winISOURL, winISOSHA256, func(s string) {
			r.SetDetail("iso", s)
		}); err != nil {
			r.End("iso", err)
			return fmt.Errorf("download Windows ISO: %w", err)
		}
	} else {
		r.Log("using user-supplied Windows ISO: %s", winISO)
	}
	virtioISO := filepath.Join(cache, "virtio-win.iso")
	r.Log("virtio-win → %s", virtioISO)
	if err := ensureDownload(virtioISO, virtioISOURL, "", func(s string) {
		r.SetDetail("iso", s)
	}); err != nil {
		r.End("iso", err)
		return fmt.Errorf("download virtio-win ISO: %w", err)
	}
	r.End("iso", nil)

	// ---------- repack ----------
	r.Start("repack")
	r.Log("distrobuilder repack-windows (injects virtio drivers into install.wim)")
	repackedISO := filepath.Join(work, "windows-incus.iso")
	if err := repackWindows(winISO, repackedISO, virtioISO); err != nil {
		r.End("repack", err)
		return fmt.Errorf("repack ISO: %w", err)
	}
	r.End("repack", nil)

	// ---------- answer ----------
	r.Start("answer")
	r.Log("building answer ISO (autounattend.xml + setup-ssh.ps1)")
	unattendPath := filepath.Join(work, "autounattend.xml")
	if err := writeAsset("assets/autounattend.xml", unattendPath); err != nil {
		r.End("answer", err)
		return err
	}
	pubKey, err := sshkeys.PublicKey()
	if err != nil {
		r.End("answer", err)
		return fmt.Errorf("ensure ssh keypair: %w", err)
	}
	setupSSHPath := filepath.Join(work, "setup-ssh.ps1")
	if err := renderAsset("assets/setup-ssh.ps1.tmpl", setupSSHPath,
		map[string]string{"PubKey": pubKey}); err != nil {
		r.End("answer", err)
		return err
	}
	answerISO := filepath.Join(work, "krapow-answer.iso")
	if err := buildAnswerISO(answerISO, unattendPath, setupSSHPath); err != nil {
		r.End("answer", err)
		return fmt.Errorf("build answer ISO: %w", err)
	}
	r.End("answer", nil)

	// ---------- install ----------
	bakeInstance := "krapow-win-bake-" + fmt.Sprint(time.Now().Unix())
	r.Start("install")
	r.Log("creating bake VM %s", bakeInstance)
	r.Log("Windows install (~30 min). FirstLogonCommand runs setup-ssh.ps1.")
	if err := runInstall(bakeInstance, repackedISO, virtioISO, answerISO); err != nil {
		r.End("install", err)
		fmt.Fprintf(os.Stderr, "\nbake failed; leaving %s in place for debugging.\n", bakeInstance)
		fmt.Fprintf(os.Stderr, "  incus console %s --type=vga\n", bakeInstance)
		fmt.Fprintf(os.Stderr, "  incus delete --force %s   # when done\n", bakeInstance)
		return fmt.Errorf("run install: %w", err)
	}
	r.End("install", nil)

	// ---------- toolchain ----------
	r.Start("toolchain")
	r.Log("SSHing into bake VM, installing VS 2022 Build Tools + chocolatey (~15 min)")
	if err := installToolchain(bakeInstance); err != nil {
		r.End("toolchain", err)
		fmt.Fprintf(os.Stderr, "\ntoolchain install failed; %s left in place for debugging.\n", bakeInstance)
		return fmt.Errorf("install toolchain: %w", err)
	}
	r.End("toolchain", nil)

	// ---------- sysprep + publish ----------
	if err := finalizeBake(r, bakeInstance, targetAlias); err != nil {
		fmt.Fprintf(os.Stderr, "\nfinalize failed; %s left in place for debugging.\n", bakeInstance)
		return fmt.Errorf("finalize: %w", err)
	}
	_ = incus.Delete(bakeInstance)
	return nil
}

// ---------- helpers ----------

func cacheDir() (string, error) { return ensureDir(".krapow", "cache") }
func workDir() (string, error)  { return ensureDir(".krapow", "work") }

func ensureDir(parts ...string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(append([]string{home}, parts...)...)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}

func writeAsset(name, dst string) error {
	b, err := assets.ReadFile(name)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// renderAsset reads an embedded text/template, applies `data`, writes the result.
func renderAsset(name, dst string, data any) error {
	raw, err := assets.ReadFile(name)
	if err != nil {
		return err
	}
	tpl, err := template.New(name).Parse(string(raw))
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, data); err != nil {
		return err
	}
	return os.WriteFile(dst, buf.Bytes(), 0o644)
}

// ensureDownload fetches `url` to `dst` if missing. If `wantSHA` is non-empty,
// verifies it after download (or against the cached file on subsequent runs).
// ensureDownload fetches `url` to `dst` if missing. onProgress (if non-nil)
// is called ~once per second during the download with a human-readable
// progress string suitable for an inline TUI status (e.g. "1.2 GiB / 5.3
// GiB, 23% — 47 MiB/s"). Cleared (called with "") on completion.
func ensureDownload(dst, url, wantSHA string, onProgress func(string)) error {
	if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
		if wantSHA == "" {
			fmt.Fprintf(bakeOut, "using cached %s\n", filepath.Base(dst))
			return nil
		}
		got, err := sha256File(dst)
		if err != nil {
			return err
		}
		if got == wantSHA {
			fmt.Fprintf(bakeOut, "using cached %s (sha256 ok)\n", filepath.Base(dst))
			return nil
		}
		fmt.Fprintf(bakeOut, "cached %s has stale checksum, re-downloading\n", filepath.Base(dst))
		_ = os.Remove(dst)
	}

	fmt.Fprintf(bakeOut, "downloading %s -> %s\n", url, dst)
	tmp := dst + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer f.Close()

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d fetching %s — Microsoft may have rotated the URL; "+
			"set WINDOWS_ISO=<path> in .env after downloading manually from "+
			"https://www.microsoft.com/en-us/evalcenter/download-windows-server-2022",
			resp.StatusCode, url)
	}

	pw := &progressWriter{total: resp.ContentLength, started: time.Now(), onProgress: onProgress}
	if _, err := io.Copy(io.MultiWriter(f, pw), resp.Body); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress("")
	}
	if err := f.Close(); err != nil {
		return err
	}
	if wantSHA != "" {
		got, err := sha256File(tmp)
		if err != nil {
			return err
		}
		if got != wantSHA {
			_ = os.Remove(tmp)
			return fmt.Errorf("sha256 mismatch for %s: got %s want %s", url, got, wantSHA)
		}
	}
	return os.Rename(tmp, dst)
}

// progressWriter is a sink io.Writer that consumes bytes without storing
// them, calling onProgress periodically with a human-readable status.
type progressWriter struct {
	total      int64
	done       int64
	started    time.Time
	lastReport time.Time
	onProgress func(string)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.onProgress == nil {
		return len(b), nil
	}
	now := time.Now()
	if now.Sub(p.lastReport) < time.Second {
		return len(b), nil
	}
	p.lastReport = now
	rate := float64(p.done) / now.Sub(p.started).Seconds()
	var msg string
	if p.total > 0 {
		pct := float64(p.done) / float64(p.total) * 100
		msg = fmt.Sprintf("%s / %s, %.0f%% — %s/s",
			fmtBytes(p.done), fmtBytes(p.total), pct, fmtBytes(int64(rate)))
	} else {
		msg = fmt.Sprintf("%s — %s/s", fmtBytes(p.done), fmtBytes(int64(rate)))
	}
	p.onProgress(msg)
	return len(b), nil
}

func fmtBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------- phase: repack ----------

func repackWindows(srcISO, dstISO, virtioISO string) error {
	// distrobuilder injects virtio drivers into the ISO's install.wim. We do
	// not touch the Windows install media beyond this — earlier attempts to
	// xorriso-add autounattend.xml at the root truncated install.wim past 4GB
	// (UDF-bridged ISO, ISO 9660 view caps at ~4GB). Our autounattend is
	// instead shipped as a separate small CD via buildAnswerISO.
	return runAsRoot("distrobuilder", "repack-windows",
		"--windows-version", "2k22",
		"--drivers", virtioISO,
		srcISO, dstISO)
}

// buildAnswerISO produces a tiny ISO containing /autounattend.xml and
// /setup-ssh.ps1 at the root. Windows Setup searches removable read-only media
// for autounattend.xml at boot, and FirstLogonCommands invokes setup-ssh.ps1
// from the same disc to install OpenSSH + authorize krapow's pubkey.
func buildAnswerISO(dst string, files ...string) error {
	stage, err := os.MkdirTemp("", "krapow-answer-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	for _, src := range files {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(stage, filepath.Base(src)), b, 0o644); err != nil {
			return err
		}
	}
	return runFG("xorriso", "-as", "mkisofs",
		"-volid", "KRAPOW_ANSWER",
		"-J", "-r",
		"-o", dst,
		stage,
	)
}

// ---------- phase: install ----------

// runInstall attaches install + answer ISOs as Incus `disk` devices and boots.
//
// KNOWN ISSUE: OVMF's SCSI CD-ROM probe times out on first boot, dropping
// into its boot picker after cascading failures through HDD/PXE/HTTP-boot
// entries. May require a VGA-console keypress or several minutes of waiting.
//
// Attempted fixes that DIDN'T work as of 2026-05-15:
//   - raw.qemu with IDE -device: works for the boot, but Incus 7's QMP
//     monitor handshake then fails on root/eth0 device add ("Failed
//     adding NIC device" / "Failed adding block device"). Race between
//     raw.qemu device IDs and Incus's monitor commands.
//   - raw.apparmor to grant qemu read access to raw.qemu paths: required,
//     but doesn't help with the monitor handshake conflict above.
//
// Real fix is its own project — likely needs Incus source-reading or
// OVMF NVRAM pre-population. Tracked as a known limitation.
func runInstall(name, repackedISO, _ /*virtioISO*/, answerISO string) error {
	if err := runFG("incus", "init", name, "--vm", "--empty",
		"-c", "security.secureboot=false",
		"-c", "limits.cpu=4",
		"-c", "limits.memory=4GiB",
		"-d", "root,size=60GiB"); err != nil {
		return err
	}
	if err := runFG("incus", "config", "device", "add", name, "install", "disk",
		"source="+repackedISO, "boot.priority=10"); err != nil {
		return err
	}
	if err := runFG("incus", "config", "device", "add", name, "answer", "disk",
		"source="+answerISO, "readonly=true"); err != nil {
		return err
	}
	if err := runFG("incus", "start", name); err != nil {
		return err
	}
	_, err := waitForSSH(name, 60*time.Minute)
	return err
}

// waitForSSH polls the VM until DHCP gives it an IPv4 AND SSH accepts our key.
func waitForSSH(name string, timeout time.Duration) (string, error) {
	privPath, _, err := sshkeys.EnsureKeyPair()
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ip := vmIPv4(name)
		if ip != "" {
			c, err := winssh.Dial(ip, privPath, 30*time.Second)
			if err == nil {
				_ = c.Close()
				return ip, nil
			}
		}
		time.Sleep(20 * time.Second)
	}
	return "", fmt.Errorf("timed out waiting for SSH on %s", name)
}

// vmIPv4 returns the VM's first IPv4 address, or "" if not yet assigned.
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

// installToolchain SSHes into the post-Windows-install bake VM and runs the
// universal toolchain installer (VS 2022 Build Tools, chocolatey). Bakes the
// result into the image so every `krapow init win` starts with parity to
// GitHub-hosted windows-latest.
func installToolchain(name string) error {
	privPath, _, err := sshkeys.EnsureKeyPair()
	if err != nil {
		return err
	}
	ip := vmIPv4(name)
	if ip == "" {
		return fmt.Errorf("bake VM %s has no IPv4 for toolchain install", name)
	}
	c, err := winssh.Dial(ip, privPath, 30*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()

	script, err := assets.ReadFile("assets/install-toolchain.ps1")
	if err != nil {
		return err
	}
	_, err = c.RunPowerShell(string(script))
	return err
}

// ---------- phase: finalize (shutdown + publish) ----------
//
// We deliberately do NOT run sysprep. Twice in development sysprep silently
// failed during `Sysprep_Clean_Validate_Opk` and never shut the VM down,
// leaving the bake hung. For ephemeral runners the trade-off is fine:
//
//   - Cloned runner VMs all share the same Windows SID and hostname
//     (something like "WIN-XXXX"), but they're never AD-joined and GitHub
//     identifies them by the krapow-given runner name, not Windows machine ID.
//   - Skipping sysprep keeps the bake reliable. Cost is a slightly larger
//     image (no /generalize cleanup of install caches) — measured at ~1-2 GB.

func finalizeBake(r *tui.Runner, name, alias string) error {
	r.Start("shutdown")
	// Graceful shutdown is REQUIRED, not a nicety. `incus stop --force` is
	// equivalent to pulling the plug — Windows doesn't get to flush the
	// registry, and machine-PATH updates from chocolatey / Git for Windows
	// (written by install-toolchain.ps1) end up only in memory. The image
	// then captures a disk where the .exes are present but PATH still points
	// at System32 only, so workflows like dtolnay/rust-toolchain fail with
	// "bash: command not found" even though git.exe is sitting there.
	r.Log("sending Windows graceful shutdown via SSH (flushes registry)")
	privPath, _, err := sshkeys.EnsureKeyPair()
	if err != nil {
		r.End("shutdown", err)
		return err
	}
	ip := vmIPv4(name)
	if ip == "" {
		err := fmt.Errorf("bake VM %s has no IPv4; cannot graceful-shutdown", name)
		r.End("shutdown", err)
		return err
	}
	c, err := winssh.Dial(ip, privPath, 30*time.Second)
	if err != nil {
		r.End("shutdown", err)
		return err
	}
	// shutdown /s /t 0 — initiate immediate clean shutdown; Windows takes ~30s
	// to actually power off. SSH session disconnects mid-command (expected).
	_, _ = c.RunPowerShell(`shutdown /s /t 0 /f`)
	c.Close()

	r.Log("waiting up to 5 min for VM to power off cleanly")
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if incus.State(name) == "stopped" {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if incus.State(name) != "stopped" {
		// Fall through to force-stop, but warn — image may have stale PATH.
		r.Log("graceful shutdown didn't complete in 5 min; force-stopping (image may have stale Machine PATH)")
		if err := runFG("incus", "stop", "--force", name); err != nil {
			r.End("shutdown", err)
			return err
		}
	}
	r.End("shutdown", nil)

	r.Start("publish")
	r.Log("detaching bake-time devices (install, answer)")
	for _, dev := range []string{"install", "answer"} {
		_ = runFG("incus", "config", "device", "remove", name, dev)
	}
	r.Log("incus publish %s --alias %s", name, alias)
	err = runFG("incus", "publish", name, "--alias", alias)
	r.End("publish", err)
	return err
}

// ---------- shell helpers ----------

func runFG(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = bakeOut
	c.Stderr = bakeErr
	c.Stdin = os.Stdin
	return c.Run()
}

// runAsRoot invokes a command as root, prepending `sudo` when we're not
// already root. Assumes sudo credentials are already primed (see primeSudo)
// so the subprocess doesn't trigger a password prompt under the TUI.
func runAsRoot(name string, args ...string) error {
	if os.Geteuid() == 0 {
		return runFG(name, args...)
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("%s requires root and sudo is not on PATH; rerun krapow as root", name)
	}
	// -n: never prompt. If the prime expired, fail fast instead of hanging.
	return runFG("sudo", append([]string{"-n", name}, args...)...)
}

// primeSudo calls `sudo -v` to refresh the user's sudo credential cache.
// Called once before the TUI starts so the password prompt (if needed) is
// visible to the user on a normal terminal. Subsequent `sudo -n` calls
// from inside the TUI then succeed silently while the cache is valid
// (typically 5-15 minutes — long enough for distrobuilder to finish).
func primeSudo() error {
	if os.Geteuid() == 0 {
		return nil
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return nil // not on PATH; runAsRoot will error later with a clearer message
	}
	// Probe first — if cred is already cached, skip the user-visible prompt.
	probe := exec.Command("sudo", "-n", "true")
	if err := probe.Run(); err == nil {
		return nil
	}
	fmt.Fprintln(os.Stderr, "krapow: bake needs root for distrobuilder; priming sudo (you may be prompted)")
	c := exec.Command("sudo", "-v")
	c.Stdin = os.Stdin
	c.Stdout = os.Stderr
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("sudo prime failed: %w", err)
	}
	return nil
}
