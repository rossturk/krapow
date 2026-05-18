// Package tart is a thin wrapper around the `tart` CLI used on macOS hosts.
//
// Mirrors the shape of internal/incus so the rest of krapow doesn't have to
// know which backend it's talking to. Tart drives Apple's Virtualization.
// framework and ships images via OCI registries (ghcr.io/cirruslabs/...).
package tart

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// StreamOut and StreamErr are where runStream writes subprocess output. Default
// to os.Stdout/Stderr; cmd-level code overrides them (e.g. to a TUI viewport).
var (
	StreamOut io.Writer = os.Stdout
	StreamErr io.Writer = os.Stderr
)

func run(args ...string) (string, error) {
	cmd := exec.Command("tart", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("tart %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func runStream(args ...string) error {
	cmd := exec.Command("tart", args...)
	cmd.Stdout = StreamOut
	cmd.Stderr = StreamErr
	return cmd.Run()
}

// Pull fetches an OCI image into the local cache. Streams progress so the TUI
// can show it.
func Pull(imageRef string) error {
	return runStream("pull", imageRef)
}

// Clone makes a per-runner copy of a base image. Tart 2.32.x has no --replace,
// so callers must Delete first if the dest may exist.
func Clone(source, dest string) error {
	return runStream("clone", source, dest)
}

// RunDetached starts `tart run --no-graphics <name>` as a background process
// whose lifetime is independent of krapow. stdout/stderr go to logPath. The
// process survives krapow exit (Setsid); callers stop it via `tart stop`.
func RunDetached(name, logPath string) error {
	// Open the log file in append mode so multiple boot attempts share one
	// rotating-friendly file.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log %s: %w", logPath, err)
	}
	cmd := exec.Command("tart", "run", "--no-graphics", name)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	// Detach into its own session so it survives krapow's exit and doesn't
	// receive SIGINT when the user Ctrl-C's the parent shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("tart run: %w", err)
	}
	// Release the file handle — Setsid'd child holds its own dup.
	_ = logFile.Close()
	// Release the Go *Process so we don't sit around waiting on it.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release tart run: %w", err)
	}
	return nil
}

// Stop sends a graceful shutdown signal to the VM. timeoutSecs is what tart
// passes to the guest before force-killing.
func Stop(name string, timeoutSecs int) error {
	return runStream("stop", name, "--timeout", fmt.Sprintf("%d", timeoutSecs))
}

// Delete removes the VM and its disk image. Tart refuses to delete a running
// VM, so callers should Stop first.
func Delete(name string) error {
	return runStream("delete", name)
}

// IP returns the DHCP-assigned IPv4 of the VM, or "" if it isn't up yet.
// Single-shot; callers poll with backoff.
//
// Tart's own `tart ip` is strict — one malformed entry anywhere in
// /var/db/dhcpd_leases makes it bail with "unexpected DHCPD leases file
// format" before it can find our VM. Apple's dhcpd is happy to leave stale,
// half-written blocks lying around (we saw a bare `name` with no value from a
// crashed VM months earlier). On any tart error we fall back to parsing the
// leases file ourselves, tolerantly, against the VM's MAC from
// ~/.tart/vms/<name>/config.json.
func IP(name string) (string, error) {
	out, err := run("ip", name)
	if err == nil {
		return strings.TrimSpace(out), nil
	}
	if ip, ferr := ipFromLeases(name); ferr == nil && ip != "" {
		return ip, nil
	}
	return "", err
}

// ipFromLeases reads the VM's MAC from tart's config.json and scans
// /var/db/dhcpd_leases for a matching block, skipping malformed entries.
// Returns ("", nil) if the lease isn't present yet — callers should keep
// polling. Returns ("", err) only when we couldn't read either file.
func ipFromLeases(name string) (string, error) {
	mac, err := vmMAC(name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile("/var/db/dhcpd_leases")
	if err != nil {
		return "", err
	}
	return findLeaseByMAC(string(data), mac), nil
}

func vmMAC(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(home, ".tart", "vms", name, "config.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p, err)
	}
	var cfg struct {
		MacAddress string `json:"macAddress"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", fmt.Errorf("parse %s: %w", p, err)
	}
	if cfg.MacAddress == "" {
		return "", fmt.Errorf("%s: macAddress empty", p)
	}
	return cfg.MacAddress, nil
}

// findLeaseByMAC scans Apple's dhcpd_leases format for a block whose
// hw_address matches `mac`. Returns the ip_address from that block, or "" if
// not found. Tolerates bare keys and other parse oddities by skipping the
// offending line — the bug we're working around.
func findLeaseByMAC(text, mac string) string {
	want := normalizeMAC(mac)
	var ip, hw string
	flush := func() string {
		defer func() { ip, hw = "", "" }()
		if hw == want && ip != "" {
			return ip
		}
		return ""
	}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		switch line {
		case "{":
			ip, hw = "", ""
			continue
		case "}":
			if v := flush(); v != "" {
				return v
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			// Bare key, comment, blank — skip.
			continue
		}
		switch k {
		case "ip_address":
			ip = v
		case "hw_address":
			// dhcpd prefixes with hardware type, e.g. "1,6:e8:a3:b0:23:b4".
			if i := strings.Index(v, ","); i >= 0 {
				v = v[i+1:]
			}
			hw = normalizeMAC(v)
		}
	}
	return ""
}

// normalizeMAC lowercases and zero-pads each colon-separated byte so that
// "06:e8:a3:b0:23:b4" (tart's config) and "6:e8:a3:b0:23:b4" (dhcpd's
// shortened form) compare equal.
func normalizeMAC(s string) string {
	parts := strings.Split(strings.ToLower(s), ":")
	for i, p := range parts {
		if len(p) == 1 {
			parts[i] = "0" + p
		}
	}
	return strings.Join(parts, ":")
}

// listEntry mirrors the fields of `tart list --format json` that we care about.
// Tart prints more fields (Size, Disk, etc.); leaving them out is fine.
type listEntry struct {
	Name   string `json:"Name"`
	Source string `json:"Source"` // "local" or "oci"
	State  string `json:"State"`  // "running" / "stopped" / ""
}

func listAll() ([]listEntry, error) {
	out, err := run("list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var entries []listEntry
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		return nil, fmt.Errorf("parse tart list: %w", err)
	}
	return entries, nil
}

// LocalVMs returns the names of every locally-cloned tart VM (Source=="local"),
// skipping pulled OCI images. Used by `krapow clean` to find orphan VMs.
func LocalVMs() ([]string, error) {
	entries, err := listAll()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Source == "local" {
			out = append(out, e.Name)
		}
	}
	return out, nil
}

// ImageExists reports whether a local VM/image with this name is in the tart
// cache. Used the same way as incus.ImageExists — to decide whether to pull.
func ImageExists(imageRef string) (bool, error) {
	entries, err := listAll()
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Name == imageRef {
			return true, nil
		}
	}
	return false, nil
}

// State returns "running", "stopped", or "absent" — matching the vocabulary
// internal/incus.State uses so callers can be backend-agnostic.
func State(name string) string {
	entries, err := listAll()
	if err != nil {
		return "absent"
	}
	for _, e := range entries {
		if e.Name != name {
			continue
		}
		s := strings.ToLower(e.State)
		if s == "" {
			return "stopped"
		}
		return s
	}
	return "absent"
}
