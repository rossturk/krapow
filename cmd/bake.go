package cmd

import (
	"github.com/rossturk/krapow/internal/imagebuild"
	"github.com/spf13/cobra"
)

// bakeCmd rebuilds the Windows base image without registering a runner. The
// `init win` flow auto-bakes when the image is missing, but that path also
// requires --repo + GitHub access so it can register a runner immediately
// after. `just rebake` and other "just refresh the image" workflows don't
// want that — they want bake-only.
func bakeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "bake",
		Short: "Build (or rebuild) the Windows base image without registering a runner",
		RunE: func(_ *cobra.Command, _ []string) error {
			return imagebuild.Build("win-runner-base")
		},
	}
	return c
}
