package cmd

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/widdlab/krapow/internal/auth"
	"github.com/widdlab/krapow/internal/githubapi"
	"github.com/widdlab/krapow/internal/incus"
	"github.com/widdlab/krapow/internal/state"
	"github.com/widdlab/krapow/internal/tart"
	"github.com/spf13/cobra"
)

// installWindow is how long after a runner is created we assume a
// GitHub-reported "offline" status is just "still installing toolchain" rather
// than a genuine failure. Generous because Windows profile installs (MSVC
// Build Tools) routinely take 20+ minutes.
const installWindow = 45 * time.Minute

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "List krapow-managed runners with VM + GitHub runner state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rs, err := state.All()
			if err != nil {
				return err
			}
			if len(rs) == 0 {
				fmt.Println("(no krapow-managed runners)")
				return nil
			}

			// Look up runner state on GitHub. Build a name->runner index per
			// API target so we only hit the API once per target even with many
			// runners. If we can't resolve a token, every row falls back to
			// "unknown" — that's intentional; status is read-only and should
			// still print the state we have locally.
			ghRunners := map[string]map[string]githubapi.Runner{}
			if tok, _, err := auth.Token(); err == nil {
				gh := githubapi.New(tok)
				for _, target := range uniqueTargets(rs) {
					if list, err := gh.ListRunners(target); err == nil {
						idx := map[string]githubapi.Runner{}
						for _, r := range list {
							idx[r.Name] = r
						}
						ghRunners[target] = idx
					}
				}
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tKIND\tSCOPE\tTARGET\tVM\tRUNNER")
			for _, r := range rs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					r.Name, r.Kind, r.EffectiveScope(), r.Repo, vmState(r), runnerState(ghRunners, r))
			}
			return w.Flush()
		},
	}
}

// runnerState maps a krapow-tracked runner to a human-readable runner state.
//
// Lifecycle reads top-to-bottom:
//
//	provisioning → installing → idle ⇄ busy → offline
//
//	provisioning — krapow state exists, GitHub has no record yet (VM booting,
//	               agent hasn't registered yet).
//	installing   — registered with GitHub, agent not heartbeating, AND created
//	               within `installWindow`. Profile is still installing.
//	idle         — registered + heartbeating + free, ready to accept jobs.
//	busy         — registered + heartbeating + currently running a job.
//	offline      — registered + not heartbeating, past the install window.
//	               Real failure: agent crashed or VM stopped.
//	unknown      — couldn't query GitHub (bad creds, different repo, etc.).
func runnerState(idx map[string]map[string]githubapi.Runner, r state.Runner) string {
	targetIdx, ok := idx[r.APITarget()]
	if !ok {
		return "unknown"
	}
	gh, ok := targetIdx[r.Name]
	if !ok {
		return "provisioning"
	}
	if gh.Status == "online" {
		if gh.Busy {
			return "busy"
		}
		return "idle"
	}
	// GitHub says offline. If we created this runner recently, it's just
	// finishing its toolchain install — agent hasn't called home yet.
	if time.Since(r.Created) < installWindow {
		return "installing"
	}
	return gh.Status
}

// vmState returns the underlying VM state ("running", "stopped", "absent")
// from whichever backend owns this runner. Backend is recorded at init time;
// pre-mac records have an empty Backend and default to incus.
func vmState(r state.Runner) string {
	if r.EffectiveBackend() == "tart" {
		return tart.State(r.Name)
	}
	return incus.State(r.Name)
}

func uniqueTargets(rs []state.Runner) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rs {
		t := r.APITarget()
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
