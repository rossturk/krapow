package cmd

import "github.com/spf13/cobra"

func Root() *cobra.Command {
	root := &cobra.Command{
		Use:           "krapow",
		Short:         "Run GitHub Actions self-hosted runners as Incus VMs",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(initCmd(), bakeCmd(), statusCmd(), startCmd(), stopCmd(), destroyCmd(), shellCmd(), doctorCmd(), cleanCmd())
	return root
}
