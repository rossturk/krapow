package cmd

import (
	"fmt"
	"os/exec"
	"regexp"
	"runtime"

	"github.com/rossturk/krapow/internal/incus"
	"github.com/rossturk/krapow/internal/state"
	"github.com/rossturk/krapow/internal/tart"
	"github.com/spf13/cobra"
)

// krapowNamePattern matches the names krapow generates for runners:
// <prefix>-<6 alphanum>. Used by `clean` to decide which orphan VMs are
// safe to consider — we won't touch instances the user named themselves.
var krapowNamePattern = regexp.MustCompile(`^(linux-runner|win-runner|mac-runner)-[a-z0-9]{6}$`)

func cleanCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "clean",
		Short: "Remove tart/incus VMs whose names look like krapow runners but aren't tracked in state",
		Long: `clean finds VMs on the host whose names match krapow's <kind>-runner-<6char>
naming convention but don't have a corresponding ~/.krapow/state/<name>.json
file. Typical sources: a failed init from before automatic cleanup was added,
a manually-deleted state file, or a partial run that died before cleanup.

Defaults to dry-run; pass -y to actually delete.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runClean(yes)
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "actually delete the listed VMs (default: dry-run)")
	return c
}

func runClean(yes bool) error {
	tracked, err := state.All()
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, r := range tracked {
		known[r.Name] = true
	}

	type orphan struct {
		name    string
		backend string // "tart" or "incus"
	}
	var orphans []orphan

	// Scan only the backend krapow actually uses on this host. macOS = tart
	// (no incus there even if the user has it installed for a remote); Linux
	// = incus. Crossing that line risks deleting unrelated VMs that happen to
	// match the naming pattern.
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("tart"); err == nil {
			names, err := tart.LocalVMs()
			if err != nil {
				return fmt.Errorf("tart list: %w", err)
			}
			for _, n := range names {
				if !krapowNamePattern.MatchString(n) || known[n] {
					continue
				}
				orphans = append(orphans, orphan{name: n, backend: "tart"})
			}
		}
	} else {
		if _, err := exec.LookPath("incus"); err == nil {
			names, err := incus.Instances()
			if err != nil {
				return fmt.Errorf("incus list: %w", err)
			}
			for _, n := range names {
				if !krapowNamePattern.MatchString(n) || known[n] {
					continue
				}
				orphans = append(orphans, orphan{name: n, backend: "incus"})
			}
		}
	}

	if len(orphans) == 0 {
		fmt.Println("(no orphan krapow VMs found)")
		return nil
	}

	for _, o := range orphans {
		if yes {
			fmt.Printf("==> destroying %s VM %s\n", o.backend, o.name)
			if o.backend == "tart" {
				_ = tart.Stop(o.name, 30) // best-effort; Delete refuses a running VM
				if err := tart.Delete(o.name); err != nil {
					fmt.Printf("    (warn) tart delete: %v\n", err)
				}
			} else {
				if err := incus.Delete(o.name); err != nil {
					fmt.Printf("    (warn) incus delete: %v\n", err)
				}
			}
		} else {
			fmt.Printf("would delete %s VM %s\n", o.backend, o.name)
		}
	}

	if !yes {
		fmt.Printf("\n%d orphan(s) found — rerun with -y to delete.\n", len(orphans))
	}
	return nil
}
