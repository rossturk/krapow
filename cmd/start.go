package cmd

import (
	"fmt"
	"time"

	"github.com/widdlab/krapow/internal/auth"
	"github.com/widdlab/krapow/internal/githubapi"
	"github.com/widdlab/krapow/internal/incus"
	"github.com/widdlab/krapow/internal/state"
	"github.com/widdlab/krapow/internal/tart"
	"github.com/spf13/cobra"
)

func startCmd() *cobra.Command {
	return &cobra.Command{
		Use:               "start <name>",
		Short:             "Start a stopped runner (boots the VM; the runner agent reconnects to GitHub on its own)",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeRunnerNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return doStart(args[0])
		},
	}
}

// doStart boots an existing krapow runner. Because `stop` leaves the GitHub
// registration intact, the runner agent inside the VM still holds valid
// credentials — its systemd / launchd / Windows service is already enabled,
// so on boot it auto-starts and reconnects. We just need to start the VM
// and wait for the heartbeat to confirm.
func doStart(name string) error {
	s, err := state.Load(name)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("no krapow state for %q", name)
	}

	if s.EffectiveBackend() == "tart" {
		st := tart.State(name)
		if st == "running" {
			fmt.Printf("==> tart VM %s already running\n", name)
		} else {
			logPath, err := tartLogPath(name)
			if err != nil {
				return err
			}
			fmt.Printf("==> starting tart VM %s (logs: %s)\n", name, logPath)
			if err := tart.RunDetached(name, logPath); err != nil {
				return err
			}
		}
	} else {
		st := incus.State(name)
		if st == "running" {
			fmt.Printf("==> VM %s already running\n", name)
		} else {
			fmt.Printf("==> starting VM %s\n", name)
			if err := incus.Start(name); err != nil {
				return err
			}
		}
	}

	tok, _, err := auth.Token()
	if err != nil {
		return err
	}
	gh := githubapi.New(tok)
	fmt.Printf("==> waiting for runner to report 'online'\n")
	// Windows boot is ~3-5 min; Linux/Mac are usually under a minute. 10
	// minutes covers all three with headroom.
	return pollRunnerOnline(gh, s.APITarget(), name, 10*time.Minute)
}

func pollRunnerOnline(gh *githubapi.Client, target, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := gh.FindRunner(target, name)
		if err != nil {
			return err
		}
		if r != nil && r.Status == "online" {
			fmt.Printf("==> %s online (id=%d)\n", name, r.ID)
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("%s never reported 'online' within %s", name, timeout)
}
