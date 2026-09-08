package cli

import "github.com/spf13/cobra"

// newRunCommand is "work" with the claim replaced by an argument: one node,
// driven in the calling process, with gates answerable from the terminal.
func newRunCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "run [node]",
		Short: "Drive a single node's run in this process",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return placeholder(cmd)
		},
	}
}
