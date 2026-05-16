package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/rossturk/krapow/internal/incus"
	"github.com/rossturk/krapow/internal/sshkeys"
	"github.com/rossturk/krapow/internal/state"
	"github.com/rossturk/krapow/internal/tart"
	"github.com/spf13/cobra"
)

func shellCmd() *cobra.Command {
	c := &cobra.Command{
		Use:               "shell <name> [-- command...]",
		Short:             "Open an interactive shell on a runner (or run a one-shot command after --)",
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: completeRunnerNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			remote := args[1:]
			return doShell(name, remote)
		},
	}
	// Don't try to parse flags meant for the remote command (e.g. `shell foo -- ls -la`).
	c.Flags().SetInterspersed(false)
	return c
}

func doShell(name string, remote []string) error {
	s, err := state.Load(name)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("no krapow state for %q", name)
	}

	switch {
	case s.EffectiveBackend() == "tart":
		return shellTart(s, remote)
	case s.Kind == "windows":
		return shellWindows(s, remote)
	default:
		return shellIncusLinux(s, remote)
	}
}

// shellTart shells into a cirruslabs tart VM as admin:admin. Used for both
// macKind and the linux-on-darwin path (Linux ARM via tart).
func shellTart(s *state.Runner, remote []string) error {
	ip, err := tart.IP(s.Name)
	if err != nil || ip == "" {
		return fmt.Errorf("could not determine IP for tart VM %s (is it running?): %v", s.Name, err)
	}
	sshpass, err := exec.LookPath("sshpass")
	if err != nil {
		fmt.Fprintf(os.Stderr, "note: sshpass not found — you'll be prompted; password is 'admin'\n")
		return runInteractive("ssh", append(baseSSHFlags(), "admin@"+ip), remote)
	}
	args := []string{"-e", "ssh"}
	args = append(args, baseSSHFlags()...)
	args = append(args, "admin@"+ip)
	cmd := exec.Command(sshpass, appendRemote(args, remote)...)
	cmd.Env = append(os.Environ(), "SSHPASS=admin")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// shellWindows shells into a Windows incus VM as Administrator using the
// ed25519 key krapow generated at init time.
func shellWindows(s *state.Runner, remote []string) error {
	ip := vmIPv4(s.Name)
	if ip == "" {
		return fmt.Errorf("could not determine IP for VM %s (is it running?)", s.Name)
	}
	privPath, _, err := sshkeys.EnsureKeyPair()
	if err != nil {
		return err
	}
	args := append([]string{"-i", privPath}, baseSSHFlags()...)
	args = append(args, "Administrator@"+ip)
	return runInteractive("ssh", args, remote)
}

// shellIncusLinux drops into the guest via `incus exec -t`. Incus VMs from
// cloud images don't have a stable SSH user out of the box, so `incus exec`
// is both more reliable and matches what an operator would reach for.
func shellIncusLinux(s *state.Runner, remote []string) error {
	if st := incus.State(s.Name); st != "running" {
		return fmt.Errorf("VM %s is %s, not running", s.Name, st)
	}
	args := []string{"exec", "-t", s.Name, "--"}
	if len(remote) == 0 {
		args = append(args, "bash", "-l")
	} else {
		args = append(args, remote...)
	}
	return runInteractive("incus", args, nil)
}

// baseSSHFlags returns the per-invocation OpenSSH options we always want.
// Host keys are intentionally not verified: VMs are ephemeral and reachable
// only over the local bridge.
func baseSSHFlags() []string {
	return []string{
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
	}
}

func appendRemote(args, remote []string) []string {
	if len(remote) == 0 {
		return args
	}
	return append(args, strings.Join(remote, " "))
}

func runInteractive(bin string, args, remote []string) error {
	cmd := exec.Command(bin, appendRemote(args, remote)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
