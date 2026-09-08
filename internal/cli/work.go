package cli

import "github.com/spf13/cobra"

// newWorkCommand is the worker face: ClaimNext against dagger, then drive
// the claimed node's run to a result.
func newWorkCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "work",
		Short: "Claim ready dagger nodes and drive their runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return placeholder(cmd)
		},
	}
}
