package cli

import "github.com/spf13/cobra"

// newServeCommand is the service face: Connect handlers over PostgreSQL,
// owning workflows, runs, the ledger, and step tokens.
func newServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the service: the store, the workflows, and the ledger",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return placeholder(cmd)
		},
	}
}
