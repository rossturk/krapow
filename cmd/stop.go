package cmd

import (
	"fmt"

	"github.com/widdlab/krapow/internal/auth"
	"github.com/widdlab/krapow/internal/githubapi"
	"github.com/widdlab/krapow/internal/incus"
	"github.com/widdlab/krapow/internal/state"
	"github.com/widdlab/krapow/internal/tart"
	"github.com/spf13/cobra"
)

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "stop <name>",
		Short:             "Stop the VM (leaves the runner registered; use `start` to bring it back)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRunnerNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doStopOrDestroy(args[0], false)
		},
	}
}

func destroyCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "destroy <name>",
		Short:             "Delete the VM and unregister the runner from GitHub",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRunnerNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doStopOrDestroy(args[0], true)
		},
	}
}

// completeRunnerNames returns krapow-tracked runner names for shell completion.
// Used by `stop` and `destroy` so `krapow destroy <Tab>` shows live names.
// Only suggests when no positional arg has been given yet.
func completeRunnerNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	runners, err := state.All()
	if err != nil {
		return nil, cobra.ShellCompDirectiveError
	}
	out := make([]string, 0, len(runners))
	for _, r := range runners {
		// Each completion entry can optionally include a description after a tab.
		// Some shells (zsh, fish) display it; bash ignores it.
		out = append(out, r.Name+"\t"+r.Kind+" runner ("+r.EffectiveScope()+":"+r.Repo+")")
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func doStopOrDestroy(name string, destroy bool) error {
	s, err := state.Load(name)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("no krapow state for %q", name)
	}

	// Only `destroy` unregisters from GitHub. `stop` leaves the registration
	// intact so the runner agent's stored credentials stay valid — `start`
	// can then just boot the VM and the agent reconnects on its own. The
	// runner shows as 'offline' in GitHub's UI while stopped; informational
	// only, doesn't affect anything.
	if destroy {
		tok, _, err := auth.Token()
		if err != nil {
			return err
		}
		gh := githubapi.New(tok)
		target := s.APITarget()
		r, err := gh.FindRunner(target, name)
		if err != nil {
			return err
		}
		if r == nil {
			fmt.Printf("==> runner %s not found on GitHub (already removed)\n", name)
		} else {
			fmt.Printf("==> deleting runner %s (id=%d) from GitHub\n", name, r.ID)
			if err := gh.DeleteRunner(target, r.ID); err != nil {
				return err
			}
		}
	}

	if s.EffectiveBackend() == "tart" {
		if destroy {
			fmt.Printf("==> destroying tart VM %s\n", name)
			// Best-effort stop first — `tart delete` refuses a running VM.
			_ = tart.Stop(name, 30)
			_ = tart.Delete(name)
			return state.Remove(name)
		}
		fmt.Printf("==> stopping tart VM %s\n", name)
		return tart.Stop(name, 30)
	}

	if destroy {
		fmt.Printf("==> destroying VM %s\n", name)
		_ = incus.Delete(name)
		return state.Remove(name)
	}
	fmt.Printf("==> stopping VM %s\n", name)
	return incus.Stop(name)
}
