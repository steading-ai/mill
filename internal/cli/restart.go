package cli

import "github.com/spf13/cobra"

// newRestartCommand abandons a node's run and starts a new generation. It is
// human-only: no worker reaches for it.
func newRestartCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "restart [node]",
		Short: "Abandon a node's run and start a new generation",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return placeholder(cmd)
		},
	}
}
